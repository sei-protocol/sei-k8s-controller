package node

import (
	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
)

const taskAwaitCondition = sidecar.TaskTypeAwaitCondition

// buildPreInitPlan constructs the task plan for the PreInit Job. For nodes
// that require bootstrap infrastructure (e.g. S3 snapshot with a bootstrap
// image), it builds the standard bootstrap sequence and appends an
// await-condition task. For all other nodes it returns an empty plan that
// resolves trivially.
func buildPreInitPlan(node *seiv1alpha1.SeiNode, planner NodePlanner) *seiv1alpha1.TaskPlan {
	if !needsPreInit(node) {
		return &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive}
	}
	plan := planner.BuildPlan(node)
	plan.Tasks = append(plan.Tasks, seiv1alpha1.PlannedTask{
		Type:   taskAwaitCondition,
		Status: seiv1alpha1.PlannedTaskPending,
	})
	return plan
}

// awaitConditionTask builds the sidecar task for the await-condition step.
func awaitConditionTask(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	snap := snapshotSourceFor(node)
	var targetHeight int64
	if snap != nil && snap.S3 != nil {
		targetHeight = snap.S3.TargetHeight
	}
	return sidecar.AwaitConditionTask{
		Condition:    sidecar.ConditionHeight,
		TargetHeight: targetHeight,
		Action:       sidecar.ActionSIGTERM,
	}
}
