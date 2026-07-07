//go:build integration

package integration

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
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
	DeadlineSeconds int
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

// runSeiload renders the platform profile, applies seiload's Job manifest, waits
// for the Job to complete, and asserts (1) every follower is still caught up and
// (2) the chain included transactions during the load window. The inclusion gate
// is load-bearing because seiload exits 0 on its duration deadline regardless of
// outcome: a chain that accepts every submission into mempools but includes none
// in blocks yields a Complete Job, live followers, and a green run despite being
// effectively write-only. Fine-grained throughput gating stays in the metrics
// layer (podMonitor + alerts); this asserts the floor: included > 0.
func runSeiload(ctx context.Context, t *testing.T, cs *kubernetes.Clientset, ch *chain, s spec) {
	t.Helper()
	// The seiload Job co-locates with the chain; the network's resolved
	// namespace is authoritative (never re-resolve from env here).
	ns := ch.network.Namespace()

	// The inclusion window opens at the committed height before load starts.
	hc := &http.Client{Timeout: 10 * time.Second}
	tmRPC := ch.rpcNodes[0].TendermintRPC()
	startHeight := mustLatestHeight(ctx, t, hc, tmRPC, "pre-load")

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
		// Self-terminating cap independent of the harness ctx: the load plus
		// generous slack for image pull + the post-summary flush.
		DeadlineSeconds: (s.durationMin + 15) * 60,
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

	// Chain survived the load: every follower still caught up (a follower can't
	// catch up to a halted chain, so this transitively covers validator quorum).
	for _, n := range ch.rpcNodes {
		if err := sei.WaitCaughtUp(ctx, hc, n.TendermintRPC()); err != nil {
			t.Errorf("post-load %s not caught up: %v", n.Name(), err)
		}
	}

	// Chain included the load: at least one transaction landed in a block during
	// the window.
	endHeight := mustLatestHeight(ctx, t, hc, tmRPC, "post-load")
	included := includedTxCount(ctx, t, hc, tmRPC, startHeight, endHeight)
	if included == 0 {
		t.Errorf("seiload ran %dm against %s but 0 transactions were included in blocks %d..%d — the chain accepted load without including any of it",
			s.durationMin, s.chainID, startHeight, endHeight)
		return
	}
	t.Logf("inclusion gate: >=%d transactions included in blocks %d..%d", included, startHeight, endHeight)
}

// mustLatestHeight reads the committed height with a bounded retry: a transient
// blip must not discard a run whose load already completed, but an endpoint that
// stays unreachable means inclusion cannot be verified, which fails closed.
func mustLatestHeight(ctx context.Context, t *testing.T, hc *http.Client, tmRPC, phase string) int64 {
	t.Helper()
	for attempt := 0; attempt < 3; attempt++ {
		if h, ok := sei.LatestHeight(ctx, hc, tmRPC); ok {
			return h
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("read %s height from %s: endpoint unreachable — cannot verify inclusion", phase, tmRPC)
	return 0
}

// blockchainPageSize is CometBFT's cap on blocks per /blockchain response.
const blockchainPageSize = 20

// blockchainInfo models just enough of CometBFT /blockchain to sum per-block tx
// counts; like /status, the Sei fork may return it with or without the JSON-RPC
// envelope.
type blockchainInfo struct {
	Result *struct {
		BlockMetas []blockMeta `json:"block_metas"`
	} `json:"result,omitempty"`
	BlockMetas []blockMeta `json:"block_metas"`
}

type blockMeta struct {
	NumTxs string `json:"num_txs"`
}

func (b *blockchainInfo) metas() []blockMeta {
	if b.Result != nil {
		return b.Result.BlockMetas
	}
	return b.BlockMetas
}

// includedTxCount sums num_txs over blocks (from, to] via /blockchain, returning
// early once the sum is positive so a healthy chain pays for one page while only
// the failing case walks the whole window. Pages that stay unreachable after
// retries, or that return fewer blocks than requested (pruned range or an error
// envelope decoding to empty), fail the test — a partial sum that reads as zero
// would defeat the gate.
func includedTxCount(ctx context.Context, t *testing.T, hc *http.Client, tmRPC string, from, to int64) int64 {
	t.Helper()
	var total int64
	for lo := from + 1; lo <= to; lo += blockchainPageSize {
		hi := lo + blockchainPageSize - 1
		if hi > to {
			hi = to
		}
		url := fmt.Sprintf("%s/blockchain?minHeight=%d&maxHeight=%d", tmRPC, lo, hi)
		var page blockchainInfo
		ok := false
		for attempt := 0; attempt < 3 && !ok; attempt++ {
			if attempt > 0 {
				time.Sleep(2 * time.Second)
			}
			page = blockchainInfo{}
			ok = getJSONInto(ctx, hc, url, &page)
		}
		if !ok {
			t.Fatalf("read %s: unreachable, non-200, or undecodable after retries — cannot verify inclusion", url)
		}
		if got, want := int64(len(page.metas())), hi-lo+1; got != want {
			t.Fatalf("%s returned %d block_metas, want %d — pruned range or error envelope; a short page cannot be trusted as zero", url, got, want)
		}
		for _, m := range page.metas() {
			n, err := strconv.ParseInt(m.NumTxs, 10, 64)
			if err != nil {
				t.Fatalf("parse num_txs %q at %s: %v", m.NumTxs, url, err)
			}
			total += n
		}
		if total > 0 {
			return total
		}
	}
	return total
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
				t.Fatalf("job %q failed: %s\n--- pod log (tail) ---\n%s",
					name, cond.Message, podLogTail(ctx, cs, ns, name))
			}
		}
		select {
		case <-ctx.Done():
			// The suite ctx fired (deadline or SIGTERM) — grab the pod log on a
			// fresh ctx (the suite ctx is already dead) so the failure carries the
			// job's last output, not just "deadline".
			logCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			tail := podLogTail(logCtx, cs, ns, name)
			cancel()
			t.Fatalf("job %q did not finish before deadline: %v\n--- pod log (tail) ---\n%s", name, ctx.Err(), tail)
		case <-tick.C:
		}
	}
}

// podLogTail returns the tail of the seiload pod's log for a Job, best-effort —
// the failure-time signal a Job condition message alone cannot give.
func podLogTail(ctx context.Context, cs *kubernetes.Clientset, ns, jobName string) string {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "batch.kubernetes.io/job-name=" + jobName,
	})
	if err != nil || len(pods.Items) == 0 {
		return fmt.Sprintf("(no pod for job %q: %v)", jobName, err)
	}
	lines := int64(50)
	raw, err := cs.CoreV1().Pods(ns).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{TailLines: &lines}).DoRaw(ctx)
	if err != nil {
		return fmt.Sprintf("(read logs failed: %v)", err)
	}
	return string(raw)
}
