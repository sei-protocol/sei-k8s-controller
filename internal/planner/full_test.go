package planner

import (
	"slices"
	"strings"
	"testing"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestFullNodePlanner_Validate_SnapshotGeneration(t *testing.T) {
	cases := []struct {
		name    string
		sg      *seiv1alpha1.SnapshotGenerationConfig
		wantErr string
	}{
		{
			name: "nil is fine",
			sg:   nil,
		},
		{
			name: "tendermint set without publish accepts keepRecent=1",
			sg: &seiv1alpha1.SnapshotGenerationConfig{
				Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{KeepRecent: 1},
			},
		},
		{
			name: "tendermint set with publish and keepRecent=2 is fine",
			sg: &seiv1alpha1.SnapshotGenerationConfig{
				Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{
					KeepRecent: 2,
					Publish:    &seiv1alpha1.TendermintSnapshotPublishConfig{},
				},
			},
		},
		{
			name:    "empty snapshotGeneration is rejected",
			sg:      &seiv1alpha1.SnapshotGenerationConfig{},
			wantErr: "fullNode: snapshotGeneration is set but has no sub-struct",
		},
		{
			name: "publish with keepRecent=1 is rejected",
			sg: &seiv1alpha1.SnapshotGenerationConfig{
				Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{
					KeepRecent: 1,
					Publish:    &seiv1alpha1.TendermintSnapshotPublishConfig{},
				},
			},
			wantErr: "fullNode: snapshotGeneration.tendermint.keepRecent must be >= 2 when publish is set",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "full-0", Namespace: "pacific-1"},
				Spec: seiv1alpha1.SeiNodeSpec{
					ChainID: "pacific-1",
					Image:   "seid:v6.4.1",
					FullNode: &seiv1alpha1.FullNodeSpec{
						SnapshotGeneration: tc.sg,
					},
				},
			}
			err := (&fullNodePlanner{}).Validate(node)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate: expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate: error = %q, want containing %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestFullNodePlanner_NoSnapshotUploadInProgression pins the Phase B behavior:
// snapshot publishing is now an external CronJob submitting one-shot
// snapshot-upload-once tasks, so the controller must NOT insert the old
// forever-loop snapshot-upload task into any progression — even when publish
// is set. Publish presence is projected as the sei.io/snapshot-publish pod
// label (see noderesource), not as a plan task.
func TestFullNodePlanner_NoSnapshotUploadInProgression(t *testing.T) {
	cases := []struct {
		name string
		sg   *seiv1alpha1.SnapshotGenerationConfig
	}{
		{
			name: "no snapshotGeneration omits upload",
			sg:   nil,
		},
		{
			name: "tendermint without publish omits upload",
			sg: &seiv1alpha1.SnapshotGenerationConfig{
				Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{KeepRecent: 3},
			},
		},
		{
			name: "tendermint with publish still omits upload (external CronJob owns it)",
			sg: &seiv1alpha1.SnapshotGenerationConfig{
				Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{
					KeepRecent: 5,
					Publish:    &seiv1alpha1.TendermintSnapshotPublishConfig{},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "full-0", Namespace: "pacific-1"},
				Spec: seiv1alpha1.SeiNodeSpec{
					ChainID: "pacific-1",
					Image:   "seid:v6.4.1",
					FullNode: &seiv1alpha1.FullNodeSpec{
						Snapshot:           &seiv1alpha1.SnapshotSource{StateSync: &seiv1alpha1.StateSyncSource{}},
						SnapshotGeneration: tc.sg,
					},
				},
			}
			plan, err := (&fullNodePlanner{}).BuildPlan(node)
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}
			types := make([]string, 0, len(plan.Tasks))
			for _, pt := range plan.Tasks {
				types = append(types, pt.Type)
			}

			if slices.Contains(types, sidecar.TaskTypeSnapshotUpload) {
				t.Fatalf("snapshot-upload must never be inserted; progression = %v", types)
			}
		})
	}
}
