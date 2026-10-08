package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

func withUpdateCondition(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason string) *seiv1alpha1.SeiNode {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type: seiv1alpha1.ConditionNodeUpdateInProgress, Status: status, Reason: reason,
	})
	return node
}

// Spec 012 Req 1.5: only an event that can free or add a slot wakes the
// waiting nodes.
func TestSlotReleased(t *testing.T) {
	running := func(status metav1.ConditionStatus, reason string) func() *seiv1alpha1.SeiNode {
		return func() *seiv1alpha1.SeiNode {
			n := withUpdateCondition(resizeNode("n", false, ""), status, reason)
			n.Status.Phase = seiv1alpha1.PhaseRunning
			return n
		}
	}
	updating := running(metav1.ConditionTrue, "UpdateStarted")
	done := running(metav1.ConditionFalse, "UpdateComplete")
	failed := running(metav1.ConditionFalse, "UpdateFailed")
	deferred := running(metav1.ConditionFalse, planner.ReasonUpdateDeferred)
	cases := []struct {
		name     string
		old, new *seiv1alpha1.SeiNode
		want     bool
	}{
		{"update completes", updating(), done(), true},
		{"update fails", updating(), failed(), true},
		{"update starts", deferred(), updating(), false},
		{"still updating", updating(), updating(), false},
		{"still waiting", deferred(), deferred(), false},
		{"no condition", resizeNode("n", false, ""), resizeNode("n", false, ""), false},
		{"holder paused", updating(), func() *seiv1alpha1.SeiNode { n := updating(); n.Spec.Paused = true; return n }(), true},
		{"node leaves Running", updating(), func() *seiv1alpha1.SeiNode { n := updating(); n.Status.Phase = seiv1alpha1.PhaseFailed; return n }(), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(slotReleased.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.new})).To(Equal(tc.want))
		})
	}

	g := NewWithT(t)
	g.Expect(slotReleased.Delete(event.DeleteEvent{Object: updating()})).To(BeTrue())
	g.Expect(slotReleased.Create(event.CreateEvent{Object: deferred()})).To(BeFalse())
}

// Spec 012 Req 1.5: a freed slot wakes every waiting node in the namespace and
// no other node.
func TestDeferredPeers(t *testing.T) {
	g := NewWithT(t)

	waitA := withUpdateCondition(resizeNode("wait-a", false, ""), metav1.ConditionFalse, planner.ReasonUpdateDeferred)
	waitB := withUpdateCondition(resizeNode("wait-b", true, ""), metav1.ConditionFalse, planner.ReasonUpdateDeferred)
	rolling := withUpdateCondition(resizeNode("rolling", false, ""), metav1.ConditionTrue, "UpdateStarted")
	settled := withUpdateCondition(resizeNode("settled", false, ""), metav1.ConditionFalse, "UpdateComplete")
	plain := resizeNode("plain", false, "")
	elsewhere := withUpdateCondition(resizeNode("elsewhere", false, ""), metav1.ConditionFalse, planner.ReasonUpdateDeferred)
	elsewhere.Namespace = "other"

	r, _ := newNodeReconciler(t, waitA, waitB, rolling, settled, plain, elsewhere)

	got := r.deferredPeers(context.Background(), settled)
	g.Expect(got).To(ConsistOf(
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "wait-a"}},
		reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: "wait-b"}},
	))
}
