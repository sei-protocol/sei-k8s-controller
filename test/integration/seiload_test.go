//go:build integration

package integration

import (
	"bytes"
	"context"
	_ "embed"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

//go:embed seiload_job.yaml.tmpl
var seiloadJobTmpl string

// seiloadProfilesCM is the platform-owned ConfigMap holding the profile
// templates (placeholders __SEI_CHAIN_ID__ / __RPC_ENDPOINTS__). The harness
// reads it from the cluster rather than vendoring the profile, so the load
// shape stays owned by platform.
const seiloadProfilesCM = "seiload-profiles"

// seiloadParams are the per-run values templated into the seiload Job manifest.
type seiloadParams struct {
	RunID           string
	ChainID         string
	Commit          string
	Image           string
	DurationMinutes int
	ProfileCM       string
}

// clientset builds a client-go clientset from the ambient config — the harness
// uses it for the Job/ConfigMap operations the SDK does not cover.
func clientset(t *testing.T) *kubernetes.Clientset {
	t.Helper()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build clientset: %v", err)
	}
	return cs
}

// renderProfile reads the platform profile template from seiload-profiles and
// substitutes the per-run chain id + the fleet's EVM endpoints (JSON-quoted).
func renderProfile(
	ctx context.Context, t *testing.T, cs *kubernetes.Clientset,
	ns, profile, chainID string, endpoints []string,
) string {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, seiloadProfilesCM, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s/%s: %v", ns, seiloadProfilesCM, err)
	}
	tmpl, ok := cm.Data[profile+".json"]
	if !ok {
		t.Fatalf("profile %q.json absent from %s", profile, seiloadProfilesCM)
	}
	quoted := make([]string, len(endpoints))
	for i, e := range endpoints {
		quoted[i] = strconv.Quote(e)
	}
	tmpl = strings.ReplaceAll(tmpl, "__SEI_CHAIN_ID__", chainID)
	tmpl = strings.ReplaceAll(tmpl, "__RPC_ENDPOINTS__", strings.Join(quoted, ","))
	return tmpl
}

// createProfileCM writes the rendered profile to a per-run ConfigMap stamped
// with the run label so the GC sweep reaps it on an abnormal exit.
func createProfileCM(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, ns, name, runID, profileJSON string) {
	t.Helper()
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{runLabelKey: runID},
		},
		Data: map[string]string{"profile.json": profileJSON},
	}
	if _, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create profile cm %q: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = cs.CoreV1().ConfigMaps(ns).Delete(ctx, name, metav1.DeleteOptions{})
	})
}

// renderJob templates the embedded seiload Job manifest with the per-run params.
// The manifest owns seiload's shape; only per-run values are injected.
func renderJob(t *testing.T, p seiloadParams) *batchv1.Job {
	t.Helper()
	var buf bytes.Buffer
	if err := template.Must(template.New("job").Parse(seiloadJobTmpl)).Execute(&buf, p); err != nil {
		t.Fatalf("render seiload job: %v", err)
	}
	var job batchv1.Job
	if err := yaml.Unmarshal(buf.Bytes(), &job); err != nil {
		t.Fatalf("unmarshal seiload job: %v", err)
	}
	return &job
}

// runSeiload drives seiload against the fleet as a decoupled unit: render the
// platform profile, apply seiload's own Job manifest, wait for it to run the
// full load, then assert the chain stayed live under it.
//
// The pass/fail signal is Job completion (seiload ran the load to the end
// without erroring) plus post-load chain liveness. A throughput/regression gate
// belongs in telemetry — a PromQL query over the run's metrics — not in this
// harness; the Job carries the metrics scrape label so that gate can be added.
func runSeiload(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, ch *chain, s spec) {
	t.Helper()
	ns := envOr("SEI_NAMESPACE", ch.network.Namespace())

	profileCM := "seiload-profile-" + s.runID
	profileJSON := renderProfile(ctx, t, cs, ns, s.seiloadProfile, s.chainID, ch.evmEndpoints())
	createProfileCM(ctx, t, cs, ns, profileCM, s.runID, profileJSON)

	job := renderJob(t, seiloadParams{
		RunID:           s.runID,
		ChainID:         s.chainID,
		Commit:          s.seiloadCommit,
		Image:           s.seiloadImage,
		DurationMinutes: s.durationMin,
		ProfileCM:       profileCM,
	})
	job.Namespace = ns
	if _, err := cs.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create seiload job: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		bg := metav1.DeletePropagationBackground
		_ = cs.BatchV1().Jobs(ns).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &bg})
	})

	waitJob(ctx, t, cs, ns, job.Name)

	// Chain survived the load: node-0 still caught up.
	hc := &http.Client{Timeout: 10 * time.Second}
	n0 := ch.rpcNodes[0]
	if err := sei.WaitCaughtUp(ctx, hc, n0.TendermintRPC()); err != nil {
		t.Errorf("post-load %s not caught up: %v", n0.Name(), err)
	}
}

// waitJob blocks until the seiload Job reaches a terminal condition. A Failed
// Job fails the suite; success returns. Bounded by ctx.
func waitJob(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, ns, name string) {
	t.Helper()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		job, err := cs.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get seiload job %q: %v", name, err)
		}
		for _, cond := range job.Status.Conditions {
			if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
				return
			}
			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				t.Fatalf("seiload job %q failed: %s", name, cond.Message)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("seiload job %q did not finish before deadline: %v", name, ctx.Err())
		case <-tick.C:
		}
	}
}
