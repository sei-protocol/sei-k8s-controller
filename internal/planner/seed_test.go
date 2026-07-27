package planner

import (
	"encoding/json"
	"slices"
	"testing"

	seiconfig "github.com/sei-protocol/sei-config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// seedImage and its predecessor exercise the image-drift update path.
const (
	seedImage     = "seid:v6.4.1"
	seedPrevImage = "seid:v6.4.0"
)

func seedNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "seed-0", Namespace: sourceChainID},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: sourceChainID,
			Image:   seedImage,
			Seed: &seiv1alpha1.SeedSpec{
				NodeKey: seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: "seed-0-node-key"},
				},
			},
		},
	}
}

// A seed takes the genesis progression and validates its pinned identity before
// the StatefulSet is applied. The absences are the point: a seed stores no chain
// state, so restoring or state-syncing one is meaningless, and it signs nothing.
func TestSeedPlanner_GenesisProgression(t *testing.T) {
	p := &seedPlanner{}
	plan, err := p.BuildPlan(seedNode())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	got := planTaskTypes(plan)
	want := []string{
		task.TaskTypeEnsureDataPVC,
		task.TaskTypeValidateNodeKey,
		task.TaskTypeApplyRBACProxyConfig,
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		TaskConfigureGenesis,
		TaskConfigApply,
		TaskConfigValidate,
		TaskMarkReady,
	}
	if !slices.Equal(got, want) {
		t.Errorf("seed init progression:\n got %v\nwant %v", got, want)
	}

	for _, absent := range []string{
		TaskSnapshotRestore,
		TaskConfigureStateSync,
		task.TaskTypeValidateSigningKey,
		task.TaskTypeValidateOperatorKeyring,
	} {
		if slices.Contains(got, absent) {
			t.Errorf("seed plan must not contain %s, got %v", absent, got)
		}
	}
}

// The config-apply payload carries the mode; sei-config's applySeedOverrides
// resolves every seed-specific key from it sidecar-side.
func TestSeedPlanner_ConfigIntentCarriesSeedMode(t *testing.T) {
	p := &seedPlanner{}
	plan, err := p.BuildPlan(seedNode())
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	idx := slices.IndexFunc(plan.Tasks, func(pt seiv1alpha1.PlannedTask) bool {
		return pt.Type == TaskConfigApply
	})
	if idx < 0 {
		t.Fatalf("plan has no %s task: %v", TaskConfigApply, planTaskTypes(plan))
	}

	var intent seiconfig.ConfigIntent
	if err := json.Unmarshal(plan.Tasks[idx].Params.Raw, &intent); err != nil {
		t.Fatalf("unmarshaling config-apply params: %v", err)
	}
	if intent.Mode != seiconfig.ModeSeed {
		t.Errorf("config-apply mode = %q, want %q", intent.Mode, seiconfig.ModeSeed)
	}
}

// A seed's NodeID is published, so an unpinned identity must fail the plan
// rather than boot a node that regenerates its key onto the data volume.
func TestSeedPlanner_ValidateRequiresNodeKey(t *testing.T) {
	node := seedNode()
	node.Spec.Seed.NodeKey = seiv1alpha1.NodeKeySource{}

	p := &seedPlanner{}
	if err := p.Validate(node); err == nil {
		t.Error("Validate should reject a seed with no nodeKey Secret")
	}

	if err := p.Validate(seedNode()); err != nil {
		t.Errorf("Validate on a well-formed seed: %v", err)
	}
}

// An image roll re-validates the identity Secret before touching the
// StatefulSet, so a broken Secret surfaces controller-side rather than as a
// kubelet mount failure on the replacement pod.
func TestSeedPlanner_UpdatePlanValidatesNodeKeyFirst(t *testing.T) {
	node := seedNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.CurrentImage = seedPrevImage

	p := &seedPlanner{}
	plan, err := p.BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan == nil {
		t.Fatal("expected an update plan for a drifted image")
	}

	got := planTaskTypes(plan)
	if got[0] != task.TaskTypeValidateNodeKey {
		t.Errorf("update plan should validate the node key first, got %v", got)
	}
	if slices.Contains(got, TaskConfigureStateSync) {
		t.Errorf("update plan must not contain configure-state-sync, got %v", got)
	}
}

// Steady state builds nothing: no drift, no plan.
func TestSeedPlanner_NoPlanWithoutDrift(t *testing.T) {
	node := seedNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.CurrentImage = node.Spec.Image

	p := &seedPlanner{}
	plan, err := p.BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan != nil {
		t.Errorf("expected no plan in steady state, got %v", planTaskTypes(plan))
	}
}
