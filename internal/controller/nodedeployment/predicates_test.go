package nodedeployment

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestChildPhaseChangedPredicate_Update(t *testing.T) {
	pred := childPhaseChangedPredicate()

	tests := []struct {
		name     string
		oldPhase seiv1alpha1.SeiNodePhase
		newPhase seiv1alpha1.SeiNodePhase
		oldGen   int64
		newGen   int64
		want     bool
	}{
		{
			name:     "phase unchanged, same generation — filtered",
			oldPhase: seiv1alpha1.PhaseInitializing,
			newPhase: seiv1alpha1.PhaseInitializing,
			oldGen:   1,
			newGen:   1,
			want:     false,
		},
		{
			name:     "phase changed — passes through",
			oldPhase: seiv1alpha1.PhaseInitializing,
			newPhase: seiv1alpha1.PhaseRunning,
			oldGen:   1,
			newGen:   1,
			want:     true,
		},
		{
			name:     "generation changed — passes through",
			oldPhase: seiv1alpha1.PhaseRunning,
			newPhase: seiv1alpha1.PhaseRunning,
			oldGen:   1,
			newGen:   2,
			want:     true,
		},
		{
			name:     "phase to failed — passes through",
			oldPhase: seiv1alpha1.PhaseRunning,
			newPhase: seiv1alpha1.PhaseFailed,
			oldGen:   1,
			newGen:   1,
			want:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldNode := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Generation: tt.oldGen},
				Status:     seiv1alpha1.SeiNodeStatus{Phase: tt.oldPhase},
			}
			newNode := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Generation: tt.newGen},
				Status:     seiv1alpha1.SeiNodeStatus{Phase: tt.newPhase},
			}
			got := pred.Update(event.UpdateEvent{
				ObjectOld: oldNode,
				ObjectNew: newNode,
			})
			if got != tt.want {
				t.Errorf("Update() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChildPhaseChangedPredicate_CreateDelete(t *testing.T) {
	pred := childPhaseChangedPredicate()

	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test"},
	}

	if !pred.Create(event.CreateEvent{Object: node}) {
		t.Error("Create should always pass through")
	}
	if !pred.Delete(event.DeleteEvent{Object: node}) {
		t.Error("Delete should always pass through")
	}
	if !pred.Generic(event.GenericEvent{Object: node}) {
		t.Error("Generic should always pass through")
	}
}
