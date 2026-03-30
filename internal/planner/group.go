package planner

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const groupAssemblyMaxRetries = 60

type genesisGroupPlanner struct{}

// BuildPlan constructs a TaskPlan for the SeiNodeGroup that:
//  1. Assembles all per-node genesis artifacts into a final genesis.json
//     (retried until the sidecar succeeds).
//  2. Waits for all child SeiNodes to reach PhaseRunning, confirming
//     they picked up the genesis.
func (p *genesisGroupPlanner) BuildPlan(
	group *seiv1alpha1.SeiNodeGroup,
	_ []seiv1alpha1.SeiNode,
) (*seiv1alpha1.TaskPlan, error) {
	s3 := groupGenesisS3(group)
	incumbentNodes := group.Status.IncumbentNodes

	nodeParams := make([]task.GenesisNodeParam, len(incumbentNodes))
	for i, name := range incumbentNodes {
		nodeParams[i] = task.GenesisNodeParam{Name: name}
	}

	assembleParams := &task.AssembleAndUploadGenesisParams{
		S3Bucket:       s3.Bucket,
		S3Prefix:       s3.Prefix,
		S3Region:       s3.Region,
		ChainID:        group.Spec.Genesis.ChainID,
		AccountBalance: group.Spec.Genesis.AccountBalance,
		Namespace:      group.Namespace,
		Nodes:          nodeParams,
	}

	assembleTask, err := buildGroupPlannedTask(group.Name, TaskAssembleGenesis, assembleParams)
	if err != nil {
		return nil, err
	}
	assembleTask.MaxRetries = groupAssemblyMaxRetries

	collectPeersParams := &task.CollectAndSetPeersParams{
		GroupName: group.Name,
		Namespace: group.Namespace,
		NodeNames: incumbentNodes,
		S3Bucket:  s3.Bucket,
		S3Prefix:  s3.Prefix,
		S3Region:  s3.Region,
	}
	collectPeersTask, err := buildGroupPlannedTask(group.Name, task.TaskTypeCollectAndSetPeers, collectPeersParams)
	if err != nil {
		return nil, err
	}

	awaitParams := &task.AwaitNodesRunningParams{
		GroupName: group.Name,
		Namespace: group.Namespace,
		Expected:  len(incumbentNodes),
		NodeNames: incumbentNodes,
	}
	awaitTask, err := buildGroupPlannedTask(group.Name, TaskAwaitNodesRunning, awaitParams)
	if err != nil {
		return nil, err
	}

	return &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{assembleTask, collectPeersTask, awaitTask},
	}, nil
}

// buildGroupPlannedTask is the group-level equivalent of buildPlannedTask.
func buildGroupPlannedTask(groupName, taskType string, params any) (seiv1alpha1.PlannedTask, error) {
	id := task.DeterministicTaskID(groupName, taskType, 0)
	p, err := marshalParams(params)
	if err != nil {
		return seiv1alpha1.PlannedTask{}, fmt.Errorf("task %s: %w", taskType, err)
	}
	return seiv1alpha1.PlannedTask{
		Type:   taskType,
		ID:     id,
		Status: seiv1alpha1.TaskPending,
		Params: p,
	}, nil
}

// groupGenesisS3 returns the S3 destination for a group's genesis artifacts.
func groupGenesisS3(group *seiv1alpha1.SeiNodeGroup) seiv1alpha1.GenesisS3Destination {
	gc := group.Spec.Genesis
	if gc.GenesisS3 != nil {
		dest := *gc.GenesisS3
		if dest.Prefix == "" {
			dest.Prefix = fmt.Sprintf("%s/%s/", gc.ChainID, group.Name)
		}
		return dest
	}
	return seiv1alpha1.GenesisS3Destination{
		Bucket: "sei-genesis-ceremony-artifacts",
		Prefix: fmt.Sprintf("%s/%s/", gc.ChainID, group.Name),
		Region: "eu-central-1",
	}
}
