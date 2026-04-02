package planner

import (
	"fmt"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	groupAssemblyMaxRetries = 60
	TaskAssembleGenesisFork = sidecar.TaskTypeAssembleGenesisFork
)

type genesisGroupPlanner struct{}

// BuildPlan constructs a TaskPlan for the SeiNodeGroup that:
//  1. Assembles all per-node genesis artifacts into a final genesis.json
//     (retried until the sidecar succeeds).
//  2. Collects node IDs and sets persistent_peers on each child node.
//  3. Waits for all child SeiNodes to reach PhaseRunning.
func (p *genesisGroupPlanner) BuildPlan(
	group *seiv1alpha1.SeiNodeGroup,
) (*seiv1alpha1.TaskPlan, error) {
	incumbentNodes := group.Status.IncumbentNodes

	nodeParams := make([]task.GenesisNodeParam, len(incumbentNodes))
	for i, name := range incumbentNodes {
		nodeParams[i] = task.GenesisNodeParam{Name: name}
	}

	// Select assembler task based on whether this is a fork ceremony.
	assembleTaskType := TaskAssembleGenesis
	var assembleParams any

	if hasCondition(group, seiv1alpha1.ConditionForkGenesisCeremonyNeeded) && group.Spec.Genesis.Fork != nil {
		assembleTaskType = TaskAssembleGenesisFork
		assembleParams = &task.AssembleForkGenesisParams{
			SourceChainID:  group.Spec.Genesis.Fork.SourceChainID,
			ChainID:        group.Spec.Genesis.ChainID,
			AccountBalance: group.Spec.Genesis.AccountBalance,
			Namespace:      group.Namespace,
			Nodes:          nodeParams,
		}
	} else {
		assembleParams = &task.AssembleAndUploadGenesisParams{
			AccountBalance: group.Spec.Genesis.AccountBalance,
			Namespace:      group.Namespace,
			Nodes:          nodeParams,
		}
	}

	assembleTask, err := buildGroupPlannedTask(group.Name, assembleTaskType, assembleParams)
	if err != nil {
		return nil, err
	}
	assembleTask.MaxRetries = groupAssemblyMaxRetries

	collectPeersParams := &task.CollectAndSetPeersParams{
		GroupName: group.Name,
		Namespace: group.Namespace,
		NodeNames: incumbentNodes,
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

	var tasks []seiv1alpha1.PlannedTask

	// For fork ceremonies, prepend exporter lifecycle tasks.
	if hasCondition(group, seiv1alpha1.ConditionForkGenesisCeremonyNeeded) && group.Spec.Genesis.Fork != nil {
		fork := group.Spec.Genesis.Fork
		exporterName := fmt.Sprintf("%s-exporter", group.Name)

		createExporter, err := buildGroupPlannedTask(group.Name, task.TaskTypeCreateExporter,
			&task.CreateExporterParams{
				GroupName:     group.Name,
				ExporterName:  exporterName,
				Namespace:     group.Namespace,
				SourceChainID: fork.SourceChainID,
				SourceImage:   fork.SourceImage,
				ExportHeight:  fork.ExportHeight,
			})
		if err != nil {
			return nil, err
		}

		awaitExporter, err := buildGroupPlannedTask(group.Name, task.TaskTypeAwaitExporterRunning,
			&task.AwaitExporterRunningParams{
				ExporterName: exporterName,
				Namespace:    group.Namespace,
			})
		if err != nil {
			return nil, err
		}

		submitExport, err := buildGroupPlannedTask(group.Name, task.TaskTypeSubmitExportState,
			&task.SubmitExportStateParams{
				ExporterName:  exporterName,
				Namespace:     group.Namespace,
				ExportHeight:  group.Spec.Genesis.Fork.ExportHeight,
				SourceChainID: group.Spec.Genesis.Fork.SourceChainID,
			})
		if err != nil {
			return nil, err
		}

		teardownExporter, err := buildGroupPlannedTask(group.Name, task.TaskTypeTeardownExporter,
			&task.TeardownExporterParams{
				ExporterName: exporterName,
				Namespace:    group.Namespace,
			})
		if err != nil {
			return nil, err
		}

		tasks = append(tasks, createExporter, awaitExporter, submitExport, teardownExporter)
	}

	tasks = append(tasks, assembleTask, collectPeersTask, awaitTask)

	return &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: tasks,
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
