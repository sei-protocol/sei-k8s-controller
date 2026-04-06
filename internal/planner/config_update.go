package planner

import (
	"github.com/google/uuid"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// ConditionPeerUpdateNeeded is the condition type set by the node controller
// when peer spec changes are detected on a Running node. The planner reads
// this condition in BuildPlan to return a peer-update plan.
const ConditionPeerUpdateNeeded = "PeerUpdateNeeded"

// needsPeerUpdatePlan returns true when the node is Running, has no active
// plan, and the PeerUpdateNeeded condition is True.
func needsPeerUpdatePlan(node *seiv1alpha1.SeiNode) bool {
	if node.Status.Phase != seiv1alpha1.PhaseRunning {
		return false
	}
	if node.Status.Plan != nil {
		return false
	}
	c := apimeta.FindStatusCondition(node.Status.Conditions, ConditionPeerUpdateNeeded)
	return c != nil && c.Status == metav1.ConditionTrue
}

// buildPeerUpdatePlan constructs a lightweight plan for applying peer config
// changes on an already-running node. It reuses existing task types:
// discover-peers, incremental config-apply, and config-validate.
func buildPeerUpdatePlan(node *seiv1alpha1.SeiNode, configParams *task.ConfigApplyParams) (*seiv1alpha1.TaskPlan, error) {
	planID := uuid.New().String()
	planIndex := 0

	var tasks []seiv1alpha1.PlannedTask

	if len(node.Spec.Peers) > 0 {
		t, err := buildPlannedTask(planID, TaskDiscoverPeers, planIndex, discoverPeersParams(node))
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
		planIndex++
	}

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
