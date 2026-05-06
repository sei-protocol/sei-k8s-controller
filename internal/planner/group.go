package planner

import (
	"github.com/google/uuid"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const groupAssemblyMaxRetries = 180

type genesisGroupPlanner struct{}

// BuildPlan constructs a TaskPlan for the SeiNodeDeployment that:
//  1. Assembles all per-node genesis artifacts into a final genesis.json
//     (retried until the sidecar succeeds).
//  2. Collects node IDs and sets persistent_peers on each child node.
//  3. Waits for all child SeiNodes to reach PhaseRunning.
func (p *genesisGroupPlanner) BuildPlan(
	group *seiv1alpha1.SeiNodeDeployment,
) (*seiv1alpha1.TaskPlan, error) {
	planID := uuid.New().String()
	planIndex := 0
	incumbentNodes := group.Status.IncumbentNodes

	nodeParams := make([]task.GenesisNodeParam, len(incumbentNodes))
	for i, name := range incumbentNodes {
		nodeParams[i] = task.GenesisNodeParam{Name: name}
	}

	accounts := make([]task.GenesisAccountEntry, len(group.Spec.Genesis.Accounts))
	for i, a := range group.Spec.Genesis.Accounts {
		accounts[i] = task.GenesisAccountEntry{Address: a.Address, Balance: a.Balance}
	}

	// Validate at planner-time so bech32 / shape errors hit
	// `kubectl describe seinodedeployment` rather than a sidecar Job pod.
	assembleParams := &task.AssembleAndUploadGenesisParams{
		AccountBalance: group.Spec.Genesis.AccountBalance,
		Namespace:      group.Namespace,
		Nodes:          nodeParams,
		Accounts:       accounts,
	}
	if err := assembleParams.Validate(); err != nil {
		return nil, err
	}

	assembleTask, err := buildPlannedTask(planID, TaskAssembleGenesis, planIndex, assembleParams)
	if err != nil {
		return nil, err
	}
	assembleTask.MaxRetries = groupAssemblyMaxRetries
	planIndex++

	collectPeersTask, err := buildPlannedTask(planID, task.TaskTypeCollectAndSetPeers, planIndex,
		&task.CollectAndSetPeersParams{
			GroupName: group.Name,
			Namespace: group.Namespace,
			NodeNames: incumbentNodes,
		})
	if err != nil {
		return nil, err
	}
	planIndex++

	awaitTask, err := buildPlannedTask(planID, TaskAwaitNodesRunning, planIndex,
		&task.AwaitNodesRunningParams{
			GroupName: group.Name,
			Namespace: group.Namespace,
			Expected:  len(incumbentNodes),
			NodeNames: incumbentNodes,
		})
	if err != nil {
		return nil, err
	}

	return &seiv1alpha1.TaskPlan{
		ID:    planID,
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{assembleTask, collectPeersTask, awaitTask},
	}, nil
}
