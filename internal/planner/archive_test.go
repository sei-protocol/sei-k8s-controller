package planner

import (
	"slices"
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

	var types []string
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

	var types []string
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
					KeepRecent: 5,
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
