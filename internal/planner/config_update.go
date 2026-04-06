package planner

import (
	"github.com/google/uuid"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// BuildConfigUpdatePlan constructs a lightweight reconfiguration plan for
// applying config changes (peers, overrides) to an already-running node.
// The plan reuses existing sidecar task types: discover-peers (if the node
// has peers), config-apply (incremental), and config-validate.
func BuildConfigUpdatePlan(node *seiv1alpha1.SeiNode, configParams *task.ConfigApplyParams) (*seiv1alpha1.TaskPlan, error) {
	planID := uuid.New().String()
	planIndex := 0

	var tasks []seiv1alpha1.PlannedTask

	// Include discover-peers only when the node has peer sources.
	if len(node.Spec.Peers) > 0 {
		t, err := buildPlannedTask(planID, TaskDiscoverPeers, planIndex, discoverPeersParams(node))
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
		planIndex++
	}

	// Apply config incrementally — the sidecar skips home directory setup
	// and genesis init on an already-configured node.
	incrementalParams := *configParams
	incrementalParams.Incremental = true
	t, err := buildPlannedTask(planID, TaskConfigApply, planIndex, &incrementalParams)
	if err != nil {
		return nil, err
	}
	tasks = append(tasks, t)
	planIndex++

	t, err = buildPlannedTask(planID, TaskConfigValidate, planIndex, &task.ConfigValidateParams{})
	if err != nil {
		return nil, err
	}
	tasks = append(tasks, t)

	return &seiv1alpha1.TaskPlan{
		ID:    planID,
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: tasks,
	}, nil
}
