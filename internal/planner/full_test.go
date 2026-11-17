package planner

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	seiconfig "github.com/sei-protocol/sei-config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch"
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
			name: "publish accepts keepRecent=1",
			sg: &seiv1alpha1.SnapshotGenerationConfig{
				Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{
					KeepRecent: 1,
					Publish:    &seiv1alpha1.TendermintSnapshotPublishConfig{},
				},
			},
		},
		{
			name: "publish rejects keepRecent=0",
			sg: &seiv1alpha1.SnapshotGenerationConfig{
				Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{
					KeepRecent: 0,
					Publish:    &seiv1alpha1.TendermintSnapshotPublishConfig{},
				},
			},
			wantErr: "fullNode: snapshotGeneration.tendermint.keepRecent must be >= 1",
		},
		{
			name: "without publish rejects keepRecent=-1",
			sg: &seiv1alpha1.SnapshotGenerationConfig{
				Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{KeepRecent: -1},
			},
			wantErr: "fullNode: snapshotGeneration.tendermint.keepRecent must be >= 1",
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

func TestFullNodePlanner_PublishKeepRecentOneReachesAppConfig(t *testing.T) {
	node := pendingNode(func(n *seiv1alpha1.SeiNode) {
		n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{
			SnapshotGeneration: &seiv1alpha1.SnapshotGenerationConfig{
				Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{
					KeepRecent: 1,
					Publish:    &seiv1alpha1.TendermintSnapshotPublishConfig{},
				},
			},
		}
	})
	if err := (&NodeResolver{}).ResolvePlan(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	if node.Status.Plan == nil {
		t.Fatal("valid retention produced no plan")
	}
	intent := configApplyIntent(t, node.Status.Plan)
	if got := intent.Overrides[seiconfig.KeySnapshotKeepRecent]; got != "1" {
		t.Fatalf("snapshot retention override = %q, want 1", got)
	}
	result, err := seiconfig.ResolveIntent(*intent)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid {
		t.Fatalf("invalid configuration: %v", result.Diagnostics)
	}
	home := t.TempDir()
	if err := seiconfig.WriteConfigToDir(result.Config, home); err != nil {
		t.Fatal(err)
	}
	app, err := tomlpatch.ReadTOML(filepath.Join(home, "config", "app.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := app["state-sync"].(map[string]any)["snapshot-keep-recent"]; got != int64(1) {
		t.Fatalf("app.toml snapshot-keep-recent = %v, want 1", got)
	}
}

// TestFullNodePlanner_NoSnapshotUploadInProgression asserts the planner never
// inserts a snapshot-upload task into any progression — even when publish is
// set. Publish presence is projected as the sei.io/snapshot-publish pod label
// (see noderesource), not as a plan task.
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
