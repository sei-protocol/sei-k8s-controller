package node

import (
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// 009 Req 5.8 (seidroid #594 nit): each reset transition records an event,
// including a counter that rose during a reset, which moves from one
// ResetRunning straight to the next.
func TestEmitDataResetEvent(t *testing.T) {
	cond := func(reason, msg string) *metav1.Condition {
		return &metav1.Condition{Type: seiv1alpha1.ConditionDataResetInProgress, Status: metav1.ConditionTrue, Reason: reason, Message: msg}
	}
	cases := []struct {
		name      string
		prev, cur *metav1.Condition
		want      string
	}{
		{"start", cond(seiv1alpha1.ReasonResetPending, "pending 1"), cond(seiv1alpha1.ReasonResetRunning, "resetting 1"), "DataResetStarted"},
		{"next counter folds in", cond(seiv1alpha1.ReasonResetRunning, "resetting 1"), cond(seiv1alpha1.ReasonResetRunning, "resetting 2"), "DataResetStarted"},
		{"complete", cond(seiv1alpha1.ReasonResetRunning, "resetting 1"), cond(seiv1alpha1.ReasonResetComplete, "complete 1"), "DataResetComplete"},
		{"failure again", cond(seiv1alpha1.ReasonResetFailed, "err a"), cond(seiv1alpha1.ReasonResetFailed, "err b"), "DataResetFailed"},
		{"no change", cond(seiv1alpha1.ReasonResetRunning, "resetting 1"), cond(seiv1alpha1.ReasonResetRunning, "resetting 1"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			rec := record.NewFakeRecorder(4)
			r := &SeiNodeReconciler{Recorder: rec}
			node := &seiv1alpha1.SeiNode{}
			apimeta.SetStatusCondition(&node.Status.Conditions, *tc.cur)

			r.emitDataResetEvent(node, tc.prev)
			if tc.want == "" {
				g.Expect(rec.Events).To(BeEmpty())
				return
			}
			g.Expect(rec.Events).To(Receive(ContainSubstring(tc.want)))
		})
	}
}

// 010 Req 5.6 (harbor e2e finding): a release reaches NotHeld from HoldPending,
// because the release plan runs first, and still records MaintenanceReleased.
func TestEmitMaintenanceEvent(t *testing.T) {
	cond := func(status metav1.ConditionStatus, reason string) *metav1.Condition {
		return &metav1.Condition{Type: seiv1alpha1.ConditionMaintenanceInProgress, Status: status, Reason: reason, Message: reason}
	}
	cases := []struct {
		name      string
		prev, cur *metav1.Condition
		want      string
	}{
		{"held", cond(metav1.ConditionTrue, seiv1alpha1.ReasonHoldPending), cond(metav1.ConditionTrue, seiv1alpha1.ReasonHeld), "MaintenanceHeld"},
		{"armed", cond(metav1.ConditionTrue, seiv1alpha1.ReasonHoldPending), cond(metav1.ConditionTrue, seiv1alpha1.ReasonArmed), "MaintenanceArmed"},
		{"released via HoldPending", cond(metav1.ConditionTrue, seiv1alpha1.ReasonHoldPending), cond(metav1.ConditionFalse, seiv1alpha1.ReasonNotHeld), "MaintenanceReleased"},
		{"released from Held", cond(metav1.ConditionTrue, seiv1alpha1.ReasonHeld), cond(metav1.ConditionFalse, seiv1alpha1.ReasonNotHeld), "MaintenanceReleased"},
		{"seeded, never held", nil, cond(metav1.ConditionFalse, seiv1alpha1.ReasonNotHeld), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			rec := record.NewFakeRecorder(4)
			r := &SeiNodeReconciler{Recorder: rec}
			node := &seiv1alpha1.SeiNode{}
			apimeta.SetStatusCondition(&node.Status.Conditions, *tc.cur)

			r.emitMaintenanceEvent(node, tc.prev)
			if tc.want == "" {
				g.Expect(rec.Events).To(BeEmpty())
				return
			}
			g.Expect(rec.Events).To(Receive(ContainSubstring(tc.want)))
		})
	}
}
