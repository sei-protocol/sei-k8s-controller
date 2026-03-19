package node

import (
	"fmt"

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
// It panics if the node lacks a valid S3 target height, since planner
// validation should have rejected such nodes before reaching this point.
func awaitConditionTask(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	snap := snapshotSourceFor(node)
	if snap == nil || snap.S3 == nil || snap.S3.TargetHeight <= 0 {
		panic(fmt.Sprintf("awaitConditionTask: node %s/%s has no valid S3 targetHeight", node.Namespace, node.Name))
	}
	return sidecar.AwaitConditionTask{
		Condition:    sidecar.ConditionHeight,
		TargetHeight: snap.S3.TargetHeight,
		Action:       sidecar.ActionSIGTERM,
	}
}
