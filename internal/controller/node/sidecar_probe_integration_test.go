package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

// TestIntegration_PodReplacement_ReissuesMarkReady walks the full round-trip:
// steady-state Running node → sidecar flips to 503 (simulating pod replacement)
// → controller spawns a MarkReady plan → sidecar returns to 200 →
// plan completes, condition restored.
func TestIntegration_PodReplacement_ReissuesMarkReady(t *testing.T) {
	g := NewGomegaWithT(t)

	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "probe-node", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "atlantic-2",
			Image:    "sei:v1.0.0",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase:        seiv1alpha1.PhaseRunning,
			CurrentImage: "sei:v1.0.0",
		},
	}

	healthy := true
	mock := &mockSidecarClient{healthz: &healthy}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetch := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// 1. Steady state: sidecar healthy, no plan.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	got := fetch()
	g.Expect(got.Status.Plan).To(BeNil(), "no plan in steady state")
	c1 := findSidecarReady(got)
	g.Expect(c1).NotTo(BeNil())
	g.Expect(c1.Status).To(Equal(metav1.ConditionTrue))

	// 2. Pod replacement: sidecar now 503.
	unhealthy := false
	mock.healthz = &unhealthy
	submittedBefore := len(mock.submitted)

	// First reconcile after flip: probe detects 503, SidecarReady=False set.
	// ResolvePlan builds a MarkReady plan and persists it; no execution this
	// tick per the "persist new plan before side effects" rule.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	got = fetch()
	g.Expect(got.Status.Plan).NotTo(BeNil(), "MarkReady plan should be spawned")
	g.Expect(got.Status.Plan.Tasks).To(HaveLen(1))
	g.Expect(got.Status.Plan.Tasks[0].Type).To(Equal(planner.TaskMarkReady))
	c2 := findSidecarReady(got)
	g.Expect(c2.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(c2.Reason).To(Equal("NotReady"))

	// 3. Next reconcile: execute the plan. Mark-ready is fire-and-forget.
	// Simulate sidecar coming back healthy (mark-ready landed in real life).
	mock.healthz = &healthy
	reconcileOnce(t, g, r, node.Name, node.Namespace)

	// MarkReady submission should have happened.
	g.Expect(len(mock.submitted)).To(BeNumerically(">", submittedBefore),
		"sidecar should have received a mark-ready submission")

	// 4. Eventually: plan completes and condition returns to True.
	// Drive a few reconciles so the executor marks the plan Complete and
	// the probe re-observes 200.
	for i := 0; i < 5; i++ {
		reconcileOnce(t, g, r, node.Name, node.Namespace)
		got = fetch()
	}
	g.Expect(got.Status.Plan).NotTo(BeNil())
	g.Expect(got.Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete),
		"MarkReady plan should reach Complete")
	cFinal := findSidecarReady(got)
	g.Expect(cFinal.Status).To(Equal(metav1.ConditionTrue),
		"condition should return to True once sidecar is healthy again")
}
