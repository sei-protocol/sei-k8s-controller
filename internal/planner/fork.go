package planner

import (
	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const forkAssemblyMaxRetries = 60

// forkGroupPlanner builds a TaskPlan that forks an existing chain's state
// into a new private network genesis. Phase 1 (stateExport) produces a
// shorter plan; Phase 2 (exportJob) prepends export Job tasks.
type forkGroupPlanner struct{}

func (p *forkGroupPlanner) BuildPlan(
	group *seiv1alpha1.SeiNodeGroup,
) (*seiv1alpha1.TaskPlan, error) {
	genesis := group.Spec.Genesis
	fork := genesis.Fork
	incumbentNodes := group.Status.IncumbentNodes
	ns := group.Namespace

	var tasks []seiv1alpha1.PlannedTask

	// Phase 2: automated export via bootstrap Job.
	if fork.ExportJob != nil {
		jobName := group.Name + "-fork-export"
		serviceName := jobName

		deployTask, err := buildGroupPlannedTask(group.Name, task.TaskTypeDeployForkJob,
			&task.DeployForkJobParams{
				GroupName:     group.Name,
				Namespace:     ns,
				JobName:       jobName,
				ServiceName:   serviceName,
				Image:         fork.ExportJob.Image,
				SourceChainID: fork.SourceChainID,
				SourceHeight:  fork.SourceHeight,
				TargetHeight:  fork.ExportJob.Snapshot.TargetHeight,
			})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, deployTask)

		awaitTask, err := buildGroupPlannedTask(group.Name, task.TaskTypeAwaitForkExport,
			&task.AwaitForkExportParams{
				JobName:   jobName,
				Namespace: ns,
			})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, awaitTask)

		teardownTask, err := buildGroupPlannedTask(group.Name, task.TaskTypeTeardownForkJob,
			&task.TeardownForkJobParams{
				JobName:     jobName,
				ServiceName: serviceName,
				Namespace:   ns,
			})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, teardownTask)
	}

	// Assemble fork genesis — both phases converge here.
	nodeParams := make([]task.GenesisNodeParam, len(incumbentNodes))
	for i, name := range incumbentNodes {
		nodeParams[i] = task.GenesisNodeParam{Name: name}
	}

	mutations := make([]task.GenesisMutationParam, len(fork.Mutations))
	for i, m := range fork.Mutations {
		mutations[i] = task.GenesisMutationParam{
			Type:    string(m.Type),
			Key:     m.Key,
			Value:   m.Value,
			Address: m.Address,
			Balance: m.Balance,
		}
	}

	assembleParams := &task.AssembleForkGenesisParams{
		SourceChainID:  fork.SourceChainID,
		SourceHeight:   fork.SourceHeight,
		NewChainID:     genesis.ChainID,
		AccountBalance: genesis.AccountBalance,
		StakingAmount:  genesis.StakingAmount,
		Namespace:      ns,
		Nodes:          nodeParams,
		Mutations:      mutations,
	}

	if fork.StateExport != nil {
		assembleParams.StateExportBucket = fork.StateExport.Bucket
		assembleParams.StateExportKey = fork.StateExport.Key
		assembleParams.StateExportRegion = fork.StateExport.Region
	}

	assembleTask, err := buildGroupPlannedTask(group.Name, "assemble-fork-genesis", assembleParams)
	if err != nil {
		return nil, err
	}
	assembleTask.MaxRetries = forkAssemblyMaxRetries
	tasks = append(tasks, assembleTask)

	// Collect peers and set on all nodes.
	collectPeersTask, err := buildGroupPlannedTask(group.Name, task.TaskTypeCollectAndSetPeers,
		&task.CollectAndSetPeersParams{
			GroupName: group.Name,
			Namespace: ns,
			NodeNames: incumbentNodes,
		})
	if err != nil {
		return nil, err
	}
	tasks = append(tasks, collectPeersTask)

	// Await all nodes running.
	awaitNodesTask, err := buildGroupPlannedTask(group.Name, TaskAwaitNodesRunning,
		&task.AwaitNodesRunningParams{
			GroupName: group.Name,
			Namespace: ns,
			Expected:  len(incumbentNodes),
			NodeNames: incumbentNodes,
		})
	if err != nil {
		return nil, err
	}
	tasks = append(tasks, awaitNodesTask)

	return &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: tasks,
	}, nil
}
