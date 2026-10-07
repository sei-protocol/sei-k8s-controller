package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

func seidImage(t *testing.T, sts *appsv1.StatefulSet) string {
	t.Helper()
	for _, c := range sts.Spec.Template.Spec.Containers {
		if c.Name == "seid" {
			return c.Image
		}
	}
	t.Fatalf("StatefulSet %s has no seid container", sts.Name)
	return ""
}

// PLT-1399 through Reconcile: a nodeConfig node rolls by its StatefulSet, so a
// drift that waits for a roll slot must keep the current pod template.
func TestReconcile_DriftWaitingForSlotKeepsTemplate(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sidecar := platformtest.Config().SidecarImage

	node := resizeNode("drift-wait", true, "")
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.CurrentImage = testImage
	node.Status.CurrentSidecarImage = sidecar

	holder := resizeNode("drift-holder", false, "")
	holder.UID = types.UID("drift-holder-uid")
	holder.Status.Phase = seiv1alpha1.PhaseRunning
	holder.Status.CurrentImage = testImage
	holder.Status.CurrentSidecarImage = sidecar
	meta.SetStatusCondition(&holder.Status.Conditions, metav1.Condition{
		Type: seiv1alpha1.ConditionNodeUpdateInProgress, Status: metav1.ConditionTrue, Reason: "UpdateStarted",
	})

	r, c := newNodeReconciler(t, node, holder)
	r.Planner.Nodes = c
	r.Planner.Platform.DriftUpdateBudgetPercent = 25 // 2 nodes: 1 slot, held by drift-holder

	_, err := r.Reconcile(ctx, nodeReqFor("drift-wait", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())
	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "drift-wait", Namespace: testNamespace}, sts)).To(Succeed())
	g.Expect(seidImage(t, sts)).To(Equal(testImage))

	n := getSeiNode(t, ctx, c, "drift-wait", testNamespace)
	n.Status.Plan = nil
	g.Expect(c.Status().Update(ctx, n)).To(Succeed())
	n = getSeiNode(t, ctx, c, "drift-wait", testNamespace)
	n.Spec.Image = "sei:v9.9.9"
	g.Expect(c.Update(ctx, n)).To(Succeed())

	_, err = r.Reconcile(ctx, nodeReqFor("drift-wait", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "drift-wait", Namespace: testNamespace}, sts)).To(Succeed())
	g.Expect(seidImage(t, sts)).To(Equal(testImage), "the drift waits for a slot, so the template must not roll")

	n = getSeiNode(t, ctx, c, "drift-wait", testNamespace)
	g.Expect(n.Status.Plan).To(BeNil())
	cond := meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("UpdateDeferred"))
	g.Expect(cond.Message).To(ContainSubstring("held by drift-holder"))
}

// seidroid on #605: pausing a drifted nodeConfig node must still scale it to
// zero while the drift budget is full, so spec.paused keeps its contract.
func TestReconcile_PausedDriftedNodeScalesToZero(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sidecar := platformtest.Config().SidecarImage

	node := resizeNode("drift-paused", true, "")
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.CurrentImage = testImage
	node.Status.CurrentSidecarImage = sidecar

	holder := resizeNode("drift-holder", false, "")
	holder.UID = types.UID("drift-holder-uid")
	holder.Status.Phase = seiv1alpha1.PhaseRunning
	holder.Status.CurrentImage = testImage
	holder.Status.CurrentSidecarImage = sidecar
	meta.SetStatusCondition(&holder.Status.Conditions, metav1.Condition{
		Type: seiv1alpha1.ConditionNodeUpdateInProgress, Status: metav1.ConditionTrue, Reason: "UpdateStarted",
	})

	r, c := newNodeReconciler(t, node, holder)
	r.Planner.Nodes = c
	r.Planner.Platform.DriftUpdateBudgetPercent = 25

	_, err := r.Reconcile(ctx, nodeReqFor("drift-paused", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	n := getSeiNode(t, ctx, c, "drift-paused", testNamespace)
	n.Status.Plan = nil
	g.Expect(c.Status().Update(ctx, n)).To(Succeed())
	n = getSeiNode(t, ctx, c, "drift-paused", testNamespace)
	n.Spec.Image = "sei:v9.9.9"
	n.Spec.Paused = true
	g.Expect(c.Update(ctx, n)).To(Succeed())

	_, err = r.Reconcile(ctx, nodeReqFor("drift-paused", testNamespace))
	g.Expect(err).NotTo(HaveOccurred())
	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "drift-paused", Namespace: testNamespace}, sts)).To(Succeed())
	g.Expect(sts.Spec.Replicas).NotTo(BeNil())
	g.Expect(*sts.Spec.Replicas).To(Equal(int32(0)), "a paused node scales to zero even while its drift waits")
}
