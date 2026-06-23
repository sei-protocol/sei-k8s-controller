//go:build integration

package integration

import (
	"bytes"
	"context"
	_ "embed"
	"testing"
	"text/template"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/yaml"
)

//go:embed faults/network_partition.yaml.tmpl
var networkPartitionTmpl string

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

// gateInjected blocks until the fault reports status.conditions[AllInjected]=True
// — the anti-false-green guard. Creating a fault CR is async (chaos-mesh must
// program tc/tproxy/cgroups); asserting before it lands tests an undisturbed
// chain.
func gateInjected(ctx context.Context, t *testing.T, dc dynamic.Interface, ns string, f fault) {
	t.Helper()
	waitFaultCondition(ctx, t, dc, ns, f, "AllInjected")
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
	for {
		u, err := ri.Get(ctx, f.obj.GetName(), metav1.GetOptions{})
		if err == nil && faultConditionTrue(u, condType) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("fault %s/%s: %s not True before deadline: %v", f.resource, f.obj.GetName(), condType, ctx.Err())
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
