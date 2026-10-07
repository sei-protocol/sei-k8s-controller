package planner

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// PLT-1399: the drift-roll budget.

// driftedNode is a Running full node named name whose spec image moved ahead
// of its running image.
func driftedNode(name string) *seiv1alpha1.SeiNode {
	n := runningFullNode()
	n.Name = name
	n.Spec.Image = testImageV2
	return n
}

func updating(n *seiv1alpha1.SeiNode) {
	setNodeUpdateCondition(n, metav1.ConditionTrue, "UpdateStarted", "rolling")
}

func budgetResolver(t *testing.T, percent int, nodes ...*seiv1alpha1.SeiNode) *NodeResolver {
	t.Helper()
	objs := make([]client.Object, 0, len(nodes))
	for _, n := range nodes {
		objs = append(objs, n.DeepCopy())
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	r := &NodeResolver{Nodes: c}
	r.Platform.DriftUpdateBudgetPercent = percent
	return r
}

func eightDrifted() []*seiv1alpha1.SeiNode {
	nodes := make([]*seiv1alpha1.SeiNode, 0, 8)
	for _, s := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		nodes = append(nodes, driftedNode("node-"+s))
	}
	return nodes
}

func TestDriftSlot_FirstNamesGetTheSlots(t *testing.T) {
	g := NewWithT(t)
	nodes := eightDrifted()
	r := budgetResolver(t, 25, nodes...) // 8 * 25% = 2 slots
	for i, n := range nodes {
		free, msg, err := r.DriftSlot(context.Background(), n)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(free).To(Equal(i < 2), n.Name)
		if !free {
			g.Expect(msg).To(ContainSubstring("2 slot(s), held by node-a, node-b"))
		}
	}
}

func TestDriftSlot_UpdatesInProgressHoldSlotsFirst(t *testing.T) {
	g := NewWithT(t)
	nodes := eightDrifted()
	updating(nodes[6]) // node-g
	updating(nodes[7]) // node-h
	r := budgetResolver(t, 25, nodes...)
	free, msg, err := r.DriftSlot(context.Background(), nodes[0])
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(free).To(BeFalse(), "node-a waits while two updates hold both slots")
	g.Expect(msg).To(ContainSubstring("held by node-g, node-h"))
}

func TestDriftSlot_MinimumOneSlotAndExclusions(t *testing.T) {
	g := NewWithT(t)
	paused := driftedNode("node-a")
	paused.Spec.Paused = true
	initializing := driftedNode("node-b")
	initializing.Status.Phase = seiv1alpha1.PhaseInitializing
	c := driftedNode("node-c")
	d := driftedNode("node-d")
	r := budgetResolver(t, 10, paused, initializing, c, d) // 2 Running * 10% rounds to 0; minimum 1
	free, _, err := r.DriftSlot(context.Background(), c)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(free).To(BeTrue(), "paused and non-Running nodes take no slot, so node-c is first")
	free, _, _ = r.DriftSlot(context.Background(), d)
	g.Expect(free).To(BeFalse())
}

func TestDriftSlot_DisabledOrUnwired(t *testing.T) {
	g := NewWithT(t)
	nodes := eightDrifted()
	for _, r := range []*NodeResolver{budgetResolver(t, 0, nodes...), {}} {
		free, _, err := r.DriftSlot(context.Background(), nodes[7])
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(free).To(BeTrue())
	}
}

// The plan gate: a drifted node with no slot gets no plan and says why; a node
// with a slot gets its update plan.
func TestResolvePlan_DriftWaitsForASlot(t *testing.T) {
	g := NewWithT(t)
	nodes := eightDrifted()
	r := budgetResolver(t, 25, nodes...)

	waiting := nodes[3].DeepCopy() // node-d
	g.Expect(r.ResolvePlan(context.Background(), waiting)).To(Succeed())
	g.Expect(waiting.Status.Plan).To(BeNil())
	cond := meta.FindStatusCondition(waiting.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(reasonUpdateDeferred))
	g.Expect(cond.Message).To(ContainSubstring("held by node-a, node-b"))

	first := nodes[0].DeepCopy() // node-a
	g.Expect(r.ResolvePlan(context.Background(), first)).To(Succeed())
	g.Expect(first.Status.Plan).NotTo(BeNil())
	g.Expect(classifyPlan(first.Status.Plan)).To(Equal(planClassNodeUpdate))
}

// A reset is not a drift update: it runs even when the drift budget is full.
func TestResolvePlan_ResetIgnoresTheDriftBudget(t *testing.T) {
	g := NewWithT(t)
	nodes := eightDrifted()
	updating(nodes[0])
	updating(nodes[1])
	resetNode := withNodeConfig(driftedNode("node-z"))
	resetNode.Spec.DataResetGeneration = 1
	r := budgetResolver(t, 25, append(nodes, resetNode)...)

	g.Expect(r.ResolvePlan(context.Background(), resetNode)).To(Succeed())
	g.Expect(isDataResetPlan(resetNode.Status.Plan)).To(BeTrue())
}

// seidroid on #605: a node the list does not show as drifted may start only if
// a slot is left after every node in the order, waiting ones included.
func TestDriftSlot_FallbackCountsWaitingNodes(t *testing.T) {
	g := NewWithT(t)
	a := driftedNode("node-a")
	updating(a)
	b := driftedNode("node-b")
	c := runningFullNode() // stored as not drifted yet
	c.Name = "node-c"
	r := budgetResolver(t, 67, a, b, c) // 3 * 67% = 2 slots, held by node-a and node-b

	inMemory := driftedNode("node-c")
	free, msg, err := r.DriftSlot(context.Background(), inMemory)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(free).To(BeFalse(), "both slots are taken once the waiting node-b counts")
	g.Expect(msg).To(ContainSubstring("held by node-a, node-b"))
}

// The template gate: a nodeConfig node rolls by its StatefulSet, so while its
// drift waits for a slot, the StatefulSet renders from a copy pinned to the
// running images.
func TestDriftRenderNode(t *testing.T) {
	nodes := eightDrifted()
	updating(nodes[0])
	updating(nodes[1])
	cases := []struct {
		name   string
		node   func() *seiv1alpha1.SeiNode
		pinned bool
	}{
		{"nodeConfig drift without a slot", func() *seiv1alpha1.SeiNode { return withNodeConfig(driftedNode("node-y")) }, true},
		{"controller-configured node", func() *seiv1alpha1.SeiNode { return driftedNode("node-y") }, false},
		{"paused", func() *seiv1alpha1.SeiNode {
			n := withNodeConfig(driftedNode("node-y"))
			n.Spec.Paused = true
			return n
		}, false},
		{"own drift update in progress", func() *seiv1alpha1.SeiNode {
			n := withNodeConfig(driftedNode("node-y"))
			updating(n)
			return n
		}, false},
		{"reset pending", func() *seiv1alpha1.SeiNode {
			n := withNodeConfig(driftedNode("node-y"))
			n.Spec.DataResetGeneration = 1
			return n
		}, false},
		{"hold change pending", func() *seiv1alpha1.SeiNode {
			n := withNodeConfig(driftedNode("node-y"))
			n.Spec.Maintenance = &seiv1alpha1.MaintenanceSpec{Hold: seiv1alpha1.MaintenanceHoldImmediate}
			return n
		}, false},
		{"plan active", func() *seiv1alpha1.SeiNode {
			n := withNodeConfig(driftedNode("node-y"))
			n.Status.Plan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive}
			return n
		}, false},
		{"not drifted", func() *seiv1alpha1.SeiNode { return withNodeConfig(runningFullNode()) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			n := tc.node()
			r := budgetResolver(t, 25, append(nodes, n)...)
			got, err := r.DriftRenderNode(context.Background(), n)
			g.Expect(err).NotTo(HaveOccurred())
			if !tc.pinned {
				g.Expect(got).To(BeIdenticalTo(n))
				return
			}
			g.Expect(got).NotTo(BeIdenticalTo(n))
			g.Expect(got.Spec.Image).To(Equal(n.Status.CurrentImage), "the pinned copy renders the running image")
			g.Expect(n.Spec.Image).To(Equal(testImageV2), "the node itself keeps its spec")
			cond := meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Reason).To(Equal(reasonUpdateDeferred))
		})
	}
}

// Bugbot on #605: a node whose running image status has not observed counts
// as drifted, so the pin reads the image from the live StatefulSet template.
func TestDriftRenderNode_UnobservedImagePinsLiveTemplate(t *testing.T) {
	nodes := eightDrifted()
	updating(nodes[0])
	updating(nodes[1])
	unobserved := func() *seiv1alpha1.SeiNode {
		n := withNodeConfig(driftedNode("node-y"))
		n.Status.CurrentImage = ""
		return n
	}

	t.Run("live template supplies the running image", func(t *testing.T) {
		g := NewWithT(t)
		n := unobserved()
		sts := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: n.Name, Namespace: n.Namespace},
			Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "seid", Image: "sei:v0.9.0"}},
			}}},
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).
			WithObjects(nodes[0].DeepCopy(), nodes[1].DeepCopy(), n.DeepCopy(), sts).Build()
		r := &NodeResolver{Nodes: c}
		r.Platform.DriftUpdateBudgetPercent = 25

		got, err := r.DriftRenderNode(context.Background(), n)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).NotTo(BeIdenticalTo(n))
		g.Expect(got.Spec.Image).To(Equal("sei:v0.9.0"))
	})

	t.Run("no running image anywhere: not pinned", func(t *testing.T) {
		g := NewWithT(t)
		n := unobserved()
		r := budgetResolver(t, 25, append(nodes, n)...)
		got, err := r.DriftRenderNode(context.Background(), n)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got).To(BeIdenticalTo(n))
	})
}
