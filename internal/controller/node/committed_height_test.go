package node

import (
	"context"
	"errors"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestObserveCommittedHeight(t *testing.T) {
	fixed := func(h int64) HeightReader {
		return func(context.Context, *seiv1alpha1.SeiNode) (int64, error) { return h, nil }
	}
	failing := func(context.Context, *seiv1alpha1.SeiNode) (int64, error) {
		return 0, errors.New("sidecar down")
	}
	stamped := func(h int64) *seiv1alpha1.SeiNode {
		old := metav1.NewTime(metav1.Now().Add(-1e12))
		return &seiv1alpha1.SeiNode{Status: seiv1alpha1.SeiNodeStatus{
			Phase: seiv1alpha1.PhaseRunning, CommittedHeight: &h, CommittedHeightTime: &old,
		}}
	}

	t.Run("running node stamps height and time", func(t *testing.T) {
		g := NewWithT(t)
		node := &seiv1alpha1.SeiNode{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}}
		(&SeiNodeReconciler{HeightReader: fixed(42)}).observeCommittedHeight(context.Background(), node, false)
		g.Expect(node.Status.CommittedHeight).To(HaveValue(Equal(int64(42))))
		g.Expect(node.Status.CommittedHeightTime).NotTo(BeNil())
	})

	t.Run("read failure keeps the previous stamp", func(t *testing.T) {
		g := NewWithT(t)
		node := stamped(7)
		prevTime := *node.Status.CommittedHeightTime
		(&SeiNodeReconciler{HeightReader: failing}).observeCommittedHeight(context.Background(), node, false)
		g.Expect(node.Status.CommittedHeight).To(HaveValue(Equal(int64(7))))
		g.Expect(*node.Status.CommittedHeightTime).To(Equal(prevTime))
	})

	t.Run("read failure backs off; a later success clears it", func(t *testing.T) {
		g := NewWithT(t)
		calls := 0
		err := errors.New("sidecar down")
		r := &SeiNodeReconciler{HeightReader: func(context.Context, *seiv1alpha1.SeiNode) (int64, error) {
			calls++
			return 9, err
		}}
		node := stamped(7)
		node.UID = "n1"
		r.observeCommittedHeight(context.Background(), node, false)
		r.observeCommittedHeight(context.Background(), node, false)
		g.Expect(calls).To(Equal(1), "second poll inside the backoff does not hit the sidecar")
		g.Expect(node.Status.CommittedHeight).To(HaveValue(Equal(int64(7))))

		r.heightReadRetryAt.Store(node.UID, time.Now().Add(-time.Second))
		err = nil
		r.observeCommittedHeight(context.Background(), node, false)
		g.Expect(calls).To(Equal(2))
		g.Expect(node.Status.CommittedHeight).To(HaveValue(Equal(int64(9))))
		_, pending := r.heightReadRetryAt.Load(node.UID)
		g.Expect(pending).To(BeFalse(), "success clears the backoff")
	})

	t.Run("skipped when not Running, drift suppressed, plan active, or no reader", func(t *testing.T) {
		g := NewWithT(t)
		cases := []struct {
			name     string
			node     *seiv1alpha1.SeiNode
			suppress bool
			reader   HeightReader
		}{
			{"pending", &seiv1alpha1.SeiNode{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhasePending}}, false, fixed(1)},
			{"suppressed", &seiv1alpha1.SeiNode{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}}, true, fixed(1)},
			{"active plan", &seiv1alpha1.SeiNode{Status: seiv1alpha1.SeiNodeStatus{
				Phase: seiv1alpha1.PhaseRunning, Plan: &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive},
			}}, false, fixed(1)},
			{"no reader", &seiv1alpha1.SeiNode{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}}, false, nil},
		}
		for _, tc := range cases {
			(&SeiNodeReconciler{HeightReader: tc.reader}).observeCommittedHeight(context.Background(), tc.node, tc.suppress)
			g.Expect(tc.node.Status.CommittedHeight).To(BeNil(), tc.name)
		}
	})
}
