package planner

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestArchivePlanner_BlockSyncProgression(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "archive-0", Namespace: "pacific-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   "seid:v6.4.1",
			Archive: &seiv1alpha1.ArchiveSpec{},
		},
	}

	p := &archiveNodePlanner{}
	plan, err := p.BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	types := make([]string, 0, len(plan.Tasks))
	for _, task := range plan.Tasks {
		types = append(types, task.Type)
	}

	// Archive nodes use the genesis (block sync) progression: no snapshot-restore, no state-sync
	if slices.Contains(types, TaskSnapshotRestore) {
		t.Error("archive plan should not contain snapshot-restore")
	}
	if slices.Contains(types, TaskConfigureStateSync) {
		t.Error("archive plan should not contain configure-state-sync")
	}

	// Must contain the base genesis progression
	for _, expected := range []string{TaskConfigureGenesis, TaskConfigApply, TaskConfigValidate, TaskMarkReady} {
		if !slices.Contains(types, expected) {
			t.Errorf("archive plan missing expected task %s, got %v", expected, types)
		}
	}
}

// Peers reach config via the config-apply override
// (network.p2p.persistent_peers): the controller resolves spec.peers into
// status.resolvedPeers before plan build, and the planner folds that set into
// the override.
func TestArchivePlanner_WithPeers(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "archive-0", Namespace: "pacific-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   "seid:v6.4.1",
			Archive: &seiv1alpha1.ArchiveSpec{},
			Peers: []seiv1alpha1.PeerSource{
				{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"peer1@host:26656"}}},
			},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			ResolvedPeers: []string{"peer1@host:26656"},
		},
	}

	p := &archiveNodePlanner{}
	plan, err := p.BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	intent := configApplyIntent(t, plan)
	if got := intent.Overrides[keyP2PPersistentPeers]; got != "peer1@host:26656" {
		t.Errorf("persistent_peers override = %q, want %q", got, "peer1@host:26656")
	}
}

// configApplyIntent extracts the *seiconfig.ConfigIntent from the plan's
// config-apply task params.
func configApplyIntent(t *testing.T, plan *seiv1alpha1.TaskPlan) *seiconfig.ConfigIntent {
	t.Helper()
	for _, tk := range plan.Tasks {
		if tk.Type != TaskConfigApply {
			continue
		}
		if tk.Params == nil {
			t.Fatal("config-apply task has nil params")
		}
		var intent seiconfig.ConfigIntent
		if err := json.Unmarshal(tk.Params.Raw, &intent); err != nil {
			t.Fatalf("unmarshaling config-apply intent: %v", err)
		}
		return &intent
	}
	t.Fatal("plan has no config-apply task")
	return nil
}

func TestArchivePlanner_SnapshotGenerationOverrides(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "archive-0", Namespace: "pacific-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   "seid:v6.4.1",
			Archive: &seiv1alpha1.ArchiveSpec{
				SnapshotGeneration: &seiv1alpha1.SnapshotGenerationConfig{
					Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{
						KeepRecent: 5,
					},
				},
			},
		},
	}

	p := &archiveNodePlanner{}
	overrides := p.controllerOverrides(node)

	if overrides == nil {
		t.Fatal("expected overrides for snapshotGeneration, got nil")
	}
	if got := overrides[seiconfig.KeySnapshotKeepRecent]; got != "5" {
		t.Errorf("snapshot-keep-recent = %q, want %q", got, "5")
	}
	if _, ok := overrides[seiconfig.KeySnapshotInterval]; !ok {
		t.Error("expected snapshot-interval override to be set")
	}
}

func TestArchivePlanner_NoSnapshotGenerationOverrides(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "archive-0", Namespace: "pacific-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   "seid:v6.4.1",
			Archive: &seiv1alpha1.ArchiveSpec{},
		},
	}

	p := &archiveNodePlanner{}
	overrides := p.controllerOverrides(node)

	if overrides != nil {
		t.Errorf("expected nil overrides without snapshotGeneration, got %v", overrides)
	}
}

func TestArchivePlanner_Mode(t *testing.T) {
	p := &archiveNodePlanner{}
	if got := p.Mode(); got != string(seiconfig.ModeArchive) {
		t.Errorf("Mode() = %q, want %q", got, seiconfig.ModeArchive)
	}
}

func TestArchivePlanner_Validate(t *testing.T) {
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
			wantErr: "archive: snapshotGeneration is set but has no sub-struct",
		},
		{
			name: "publish with keepRecent=1 is rejected",
			sg: &seiv1alpha1.SnapshotGenerationConfig{
				Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{
					KeepRecent: 1,
					Publish:    &seiv1alpha1.TendermintSnapshotPublishConfig{},
				},
			},
			wantErr: "archive: snapshotGeneration.tendermint.keepRecent must be >= 2 when publish is set",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "archive-0", Namespace: "pacific-1"},
				Spec: seiv1alpha1.SeiNodeSpec{
					ChainID: "pacific-1",
					Image:   "seid:v6.4.1",
					Archive: &seiv1alpha1.ArchiveSpec{SnapshotGeneration: tc.sg},
				},
			}
			err := (&archiveNodePlanner{}).Validate(node)
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

// TestArchivePlanner_NoSnapshotUploadInProgression pins the Phase B behavior:
// snapshot publishing is now an external CronJob submitting one-shot
// snapshot-upload-once tasks, so the controller must NOT insert the old
// forever-loop snapshot-upload task into any progression — even when publish
// is set. Publish presence is projected as the sei.io/snapshot-publish pod
// label (see noderesource), not as a plan task.
func TestArchivePlanner_NoSnapshotUploadInProgression(t *testing.T) {
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
				ObjectMeta: metav1.ObjectMeta{Name: "archive-0", Namespace: "pacific-1"},
				Spec: seiv1alpha1.SeiNodeSpec{
					ChainID: "pacific-1",
					Image:   "seid:v6.4.1",
					Archive: &seiv1alpha1.ArchiveSpec{SnapshotGeneration: tc.sg},
				},
			}
			plan, err := (&archiveNodePlanner{}).BuildPlan(node)
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
