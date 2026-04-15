package planner

import (
	"github.com/google/uuid"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// buildNodeUpdatePlan constructs a TaskPlan for re-initializing the sidecar
// after a spec change in the Running phase. The task sequence mirrors
// post-bootstrap initialization: configure genesis, apply config, discover
// peers, validate config, and mark ready. All tasks are idempotent — if
// the PVC state is already current, each task is a fast no-op on the sidecar.
//
// Callers provide the mode and configApplyParams so that mode-specific
// controller overrides (e.g., snapshot generation settings) are included.
func buildNodeUpdatePlan(
	node *seiv1alpha1.SeiNode,
	configApplyParams *task.ConfigApplyParams,
) (*seiv1alpha1.TaskPlan, error) {
	prog := []string{TaskConfigureGenesis, TaskConfigApply}
	if len(node.Spec.Peers) > 0 {
		prog = append(prog, TaskDiscoverPeers)
	}
	prog = append(prog, TaskConfigValidate, TaskMarkReady)

	planID := uuid.New().String()
	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		t, err := buildPlannedTask(planID, taskType, i, paramsForTaskType(node, taskType, nil, configApplyParams))
		if err != nil {
			return nil, err
		}
		tasks[i] = t
	}
	return &seiv1alpha1.TaskPlan{
		ID:    planID,
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: tasks,
	}, nil
}
