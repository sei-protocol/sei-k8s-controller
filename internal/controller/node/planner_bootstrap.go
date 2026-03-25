package node

import (
	"fmt"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	taskAwaitCondition          = sidecar.TaskTypeAwaitCondition
	taskGenerateIdentity        = sidecar.TaskTypeGenerateIdentity
	taskGenerateGentx           = sidecar.TaskTypeGenerateGentx
	taskUploadGenesisArtifacts  = sidecar.TaskTypeUploadGenesisArtifacts
	taskAwaitGenesisAssembly    = sidecar.TaskTypeAwaitGenesisAssembly
)

// buildPreInitPlan constructs the task plan for the PreInit Job. For
// snapshot-bootstrap nodes, it builds the standard bootstrap sequence. For
// all other nodes it returns an empty plan that resolves trivially.
func buildPreInitPlan(node *seiv1alpha1.SeiNode, planner NodePlanner) *seiv1alpha1.TaskPlan {
	if !needsPreInit(node) {
		return &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive, Tasks: []seiv1alpha1.PlannedTask{}}
	}
	return planner.BuildPlan(node)
}

// buildPostBootstrapInitPlan constructs a reduced InitPlan for nodes that
// completed a PreInit Job. The PVC already contains blockchain data synced
// to the target height, so snapshot-restore and configure-state-sync are
// skipped. The plan applies config for the actual image, discovers peers
// for ongoing operation, and validates.
func buildPostBootstrapInitPlan(node *seiv1alpha1.SeiNode) *seiv1alpha1.TaskPlan {
	peers := peersFor(node)

	prog := []string{taskConfigureGenesis, taskConfigApply}
	if len(peers) > 0 {
		prog = append(prog, taskDiscoverPeers)
	}
	prog = append(prog, taskConfigValidate, taskMarkReady)

	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, t := range prog {
		tasks[i] = seiv1alpha1.PlannedTask{Type: t, Status: seiv1alpha1.PlannedTaskPending}
	}
	return &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive, Tasks: tasks}
}

// buildGenesisInitPlan constructs the full Init plan for genesis ceremony
// nodes. Per-node artifact generation runs first, then await-genesis-assembly
// blocks until the group controller has assembled and uploaded genesis.json.
// After that the standard configure/apply/peers/validate/ready sequence runs.
func buildGenesisInitPlan() *seiv1alpha1.TaskPlan {
	prog := []string{
		taskGenerateIdentity,
		taskGenerateGentx,
		taskUploadGenesisArtifacts,
		taskAwaitGenesisAssembly,
		taskConfigureGenesis,
		taskConfigApply,
		taskDiscoverPeers,
		taskConfigValidate,
		taskMarkReady,
	}
	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, t := range prog {
		tasks[i] = seiv1alpha1.PlannedTask{Type: t, Status: seiv1alpha1.PlannedTaskPending}
	}
	return &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive, Tasks: tasks}
}

// awaitConditionTask builds the sidecar task for the await-condition step.
func awaitConditionTask(node *seiv1alpha1.SeiNode) (sidecar.TaskBuilder, error) {
	snap := snapshotSourceFor(node)
	if snap == nil || snap.S3 == nil || snap.S3.TargetHeight <= 0 {
		return nil, fmt.Errorf("awaitConditionTask: node %s/%s has no valid S3 targetHeight", node.Namespace, node.Name)
	}
	return sidecar.AwaitConditionTask{
		Condition:    sidecar.ConditionHeight,
		TargetHeight: snap.S3.TargetHeight,
		Action:       sidecar.ActionSIGTERM,
	}, nil
}
