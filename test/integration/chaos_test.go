//go:build integration

package integration

import (
	"bytes"
	"context"
	_ "embed"
	"testing"
	"text/template"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"
)

// injectDeadline bounds fault injection independently of the per-scenario
// timeout. A fault that has not reached AllInjected within this window is stuck
// (a chaos-daemon apply erroring on every retry), not slow — fail fast rather
// than poll the never-flipping condition until the scenario budget expires. Set
// well above chaos-mesh's observed inject latency (seconds), with margin for
// selector settling and daemon apply retries.
const injectDeadline = 5 * time.Minute

//go:embed faults/network_partition.yaml.tmpl
var networkPartitionTmpl string

//go:embed faults/packet_loss.yaml.tmpl
var packetLossTmpl string

//go:embed faults/cpu_stress.yaml.tmpl
var cpuStressTmpl string

//go:embed faults/time_skew.yaml.tmpl
var timeSkewTmpl string

//go:embed faults/network_latency.yaml.tmpl
var networkLatencyTmpl string

//go:embed faults/bandwidth_limit.yaml.tmpl
var bandwidthLimitTmpl string

//go:embed faults/memory_stress.yaml.tmpl
var memoryStressTmpl string

//go:embed faults/byzantine.yaml.tmpl
var byzantineTmpl string

//go:embed faults/pod_failure.yaml.tmpl
var podFailureTmpl string

//go:embed faults/container_kill.yaml.tmpl
var containerKillTmpl string

// chaosGVR is the GroupVersionResource for a Chaos-Mesh fault kind. Faults are
// applied unstructured so the chaos-mesh API stays out of the module's deps.
func chaosGVR(resource string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "chaos-mesh.org", Version: "v1alpha1", Resource: resource}
}

// faultParams are the per-run values templated into a fault CR.
type faultParams struct {
	ChainID   string
	RunID     string
	Namespace string
	Duration  string
}

// fault is a rendered Chaos-Mesh fault CR plus the resource name its dynamic
// client needs.
type fault struct {
	resource string // GVR resource, e.g. "networkchaos"
	obj      *unstructured.Unstructured
}

// hasDuration reports whether the fault self-expires (carries spec.duration). A
// duration-less fault would block gateRecovered until the scenario deadline, so
// every ported fault must currently carry one.
func (f fault) hasDuration() bool {
	_, found, _ := unstructured.NestedString(f.obj.Object, "spec", "duration")
	return found
}

// dynClient builds a dynamic client from the ambient config — the harness
// applies fault CRs unstructured through it.
func dynClient(t *testing.T) dynamic.Interface {
	t.Helper()
	cfg, err := ctrl.GetConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build dynamic client: %v", err)
	}
	return dc
}

// renderFault templates a fault CR and decodes it to an unstructured object.
func renderFault(t *testing.T, resource, tmpl string, p faultParams) fault {
	t.Helper()
	var buf bytes.Buffer
	if err := template.Must(template.New("fault").Parse(tmpl)).Execute(&buf, p); err != nil {
		t.Fatalf("render fault: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal fault: %v", err)
	}
	return fault{resource: resource, obj: &unstructured.Unstructured{Object: m}}
}

// applyFault creates the fault CR and registers best-effort deletion. Deletion
// is also what stops a duration-less fault; duration-bearing faults self-expire.
func applyFault(ctx context.Context, t *testing.T, dc dynamic.Interface, ns string, f fault) {
	t.Helper()
	ri := dc.Resource(chaosGVR(f.resource)).Namespace(ns)
	if _, err := ri.Create(ctx, f.obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("apply %s/%s: %v", f.resource, f.obj.GetName(), err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = ri.Delete(ctx, f.obj.GetName(), metav1.DeleteOptions{})
	})
}

// gateInjected blocks until AllInjected, then asserts the fault hit the right
// number of targets. AllInjected is set after chaos-mesh writes the per-target
// records to the (durable) CR status, so a single read here doesn't race the
// injection. oneShot kills must hit EXACTLY one validator (mode:one): ≥1 alone
// wouldn't catch a future selector edit that killed 2/4 and halted the chain.
func gateInjected(ctx context.Context, t *testing.T, dc dynamic.Interface, ns string, f fault, oneShot bool) {
	t.Helper()
	// Bound injection independently of the scenario deadline: a fault that has
	// not reached AllInjected within injectDeadline is stuck (e.g. a chaos-daemon
	// apply erroring on every retry), not merely slow. Without this it would poll
	// the never-flipping condition until the full scenario timeout, turning a
	// single stuck injection into a ~40m run that reds the suite with an opaque
	// "before deadline" at the very end. Recovery keeps the scenario ctx — it is
	// gated on the fault's own spec.duration, not a fixed budget.
	injectCtx, cancel := context.WithTimeout(ctx, injectDeadline)
	defer cancel()
	waitFaultCondition(injectCtx, t, dc, ns, f, "AllInjected")
	u, err := dc.Resource(chaosGVR(f.resource)).Namespace(ns).Get(ctx, f.obj.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read injected targets for %s/%s: %v", f.resource, f.obj.GetName(), err)
	}
	n := faultInjectedTargets(u)
	if n < 1 {
		t.Fatalf("fault %s/%s injected 0 targets — selector matched nothing (false-green guard)", f.resource, f.obj.GetName())
	}
	if oneShot && n != 1 {
		t.Fatalf("one-shot fault %s/%s injected %d targets, want exactly 1 — quorum risk", f.resource, f.obj.GetName(), n)
	}
	t.Logf("%s/%s injected %d target(s)", f.resource, f.obj.GetName(), n)
}

// faultInjectedTargets counts the targets chaos-mesh actually injected. The
// count lives per-target in status.experiment.containerRecords[].injectedCount
// (there is no top-level injectedCount), so a 0-match selector yields no records.
func faultInjectedTargets(u *unstructured.Unstructured) int {
	recs, _, _ := unstructured.NestedSlice(u.Object, "status", "experiment", "containerRecords")
	n := 0
	for _, r := range recs {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if ic, _, _ := unstructured.NestedInt64(m, "injectedCount"); ic > 0 {
			n++
		}
	}
	return n
}

// gateRecovered blocks until AllRecovered=True, catching chaos-mesh finalizers
// stuck on leftover tc/tproxy rules before teardown.
func gateRecovered(ctx context.Context, t *testing.T, dc dynamic.Interface, ns string, f fault) {
	t.Helper()
	waitFaultCondition(ctx, t, dc, ns, f, "AllRecovered")
}

func waitFaultCondition(ctx context.Context, t *testing.T, dc dynamic.Interface, ns string, f fault, condType string) {
	t.Helper()
	ri := dc.Resource(chaosGVR(f.resource)).Namespace(ns)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var lastErr error // a real (non-NotFound) Get error, surfaced on timeout
	for {
		u, err := ri.Get(ctx, f.obj.GetName(), metav1.GetOptions{})
		switch {
		case err == nil && faultConditionTrue(u, condType):
			return
		case err != nil && !apierrors.IsNotFound(err): // NotFound is transient post-create cache lag
			lastErr = err
		}
		select {
		case <-ctx.Done():
			t.Fatalf("fault %s/%s: %s not True before deadline: %v (last get: %v)",
				f.resource, f.obj.GetName(), condType, ctx.Err(), lastErr)
		case <-tick.C:
		}
	}
}

// faultConditionTrue reports whether the fault CR carries condType=True.
func faultConditionTrue(u *unstructured.Unstructured, condType string) bool {
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !found || err != nil {
		return false
	}
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == condType && m["status"] == "True" {
			return true
		}
	}
	return false
}
