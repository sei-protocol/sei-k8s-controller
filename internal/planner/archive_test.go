package planner

import (
	"slices"
	"strings"
	"testing"

	seiconfig "github.com/sei-protocol/sei-config"
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

	if !slices.Contains(types, TaskDiscoverPeers) {
		t.Errorf("archive plan with peers should contain discover-peers, got %v", types)
	}
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

func TestArchivePlanner_SnapshotUploadInProgression(t *testing.T) {
	cases := []struct {
		name       string
		sg         *seiv1alpha1.SnapshotGenerationConfig
		wantUpload bool
	}{
		{
			name:       "no snapshotGeneration omits upload",
			sg:         nil,
			wantUpload: false,
		},
		{
			name: "tendermint without publish omits upload",
			sg: &seiv1alpha1.SnapshotGenerationConfig{
				Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{KeepRecent: 3},
			},
			wantUpload: false,
		},
		{
			name: "tendermint with publish includes upload before mark-ready",
			sg: &seiv1alpha1.SnapshotGenerationConfig{
				Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{
					KeepRecent: 5,
					Publish:    &seiv1alpha1.TendermintSnapshotPublishConfig{},
				},
			},
			wantUpload: true,
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

			hasUpload := slices.Contains(types, TaskSnapshotUpload)
			if hasUpload != tc.wantUpload {
				t.Fatalf("snapshot-upload present = %v, want %v; progression = %v", hasUpload, tc.wantUpload, types)
			}
			if !tc.wantUpload {
				return
			}
			uploadIdx := slices.Index(types, TaskSnapshotUpload)
			readyIdx := slices.Index(types, TaskMarkReady)
			if uploadIdx < 0 || readyIdx < 0 || uploadIdx >= readyIdx {
				t.Fatalf("snapshot-upload (%d) must precede mark-ready (%d); progression = %v", uploadIdx, readyIdx, types)
			}
			validateIdx := slices.Index(types, TaskConfigValidate)
			if validateIdx >= 0 && validateIdx > uploadIdx {
				t.Fatalf("config-validate (%d) must precede snapshot-upload (%d); progression = %v", validateIdx, uploadIdx, types)
			}
		})
	}
}
