package planner

import (
	seiconfig "github.com/sei-protocol/sei-config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

// buildConfigUpdatePlan leaves the StatefulSet and images untouched. The
// shared assembler regenerates base, patches peers, then overlays configValues
// before validation, start-gate approval, and the polled restart. Approval must
// precede restart so seid can pass its sidecar healthz startup gate. Image
// updates use replacement instead. Running-path base regeneration resets
// [statesync] to mode defaults, clearing the trust point written at init by
// configure-state-sync. The StateSync workflow patches config separately.
func buildConfigUpdatePlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	plan, err := assembleUpdatePlan(node, []string{
		TaskConfigPatch, TaskConfigValidate, TaskMarkReady, sidecar.TaskTypeRestartSeid,
	}, p2pConfigPatch(node))
	if err != nil {
		return nil, err
	}
	setNodeUpdateCondition(node, metav1.ConditionTrue, "ConfigUpdateStarted",
		"configValues drift detected: regenerating configuration and restarting seid")
	return plan, nil
}

// runningConfigIntent mirrors each mode's init intent, without snapshot,
// genesis ceremony or state-sync tasks. The overlay never enters Overrides.
func runningConfigIntent(node *seiv1alpha1.SeiNode) *seiconfig.ConfigIntent {
	mode := seiconfig.ModeFull
	var overrides map[string]string
	switch {
	case node.Spec.FullNode != nil:
		overrides = (&fullNodePlanner{}).controllerOverrides(node)
	case node.Spec.Archive != nil:
		mode = seiconfig.ModeArchive
		overrides = (&archiveNodePlanner{}).controllerOverrides(node)
	case node.Spec.Validator != nil:
		mode = seiconfig.ModeValidator
	case node.Spec.Seed != nil:
		mode = seiconfig.ModeSeed
	case node.Spec.Replayer != nil:
		overrides = (&replayerPlanner{}).controllerOverrides()
	}
	return &seiconfig.ConfigIntent{
		Mode:      mode,
		Overrides: mergeOverrides(mergeOverrides(commonOverrides(node), overrides), node.Spec.Overrides),
	}
}
