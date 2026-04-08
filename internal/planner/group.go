package planner

import (
	"fmt"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	groupAssemblyMaxRetries = 180
	TaskAssembleGenesisFork = sidecar.TaskTypeAssembleGenesisFork
)

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

	var tasks []seiv1alpha1.PlannedTask

	// For fork ceremonies, prepend exporter lifecycle tasks.
	if hasCondition(group, seiv1alpha1.ConditionForkGenesisCeremonyNeeded) && group.Spec.Genesis.Fork != nil {
		fork := group.Spec.Genesis.Fork
		exporterName := fmt.Sprintf("%s-exporter", group.Name)

		createExporter, err := buildPlannedTask(planID, task.TaskTypeCreateExporter, planIndex,
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
		planIndex++

		awaitExporter, err := buildPlannedTask(planID, task.TaskTypeAwaitExporterRunning, planIndex,
			&task.AwaitExporterRunningParams{
				ExporterName: exporterName,
				Namespace:    group.Namespace,
			})
		if err != nil {
			return nil, err
		}
		planIndex++

		submitExport, err := buildPlannedTask(planID, task.TaskTypeSubmitExportState, planIndex,
			&task.SubmitExportStateParams{
				ExporterName:  exporterName,
				Namespace:     group.Namespace,
				ExportHeight:  group.Spec.Genesis.Fork.ExportHeight,
				SourceChainID: group.Spec.Genesis.Fork.SourceChainID,
			})
		if err != nil {
			return nil, err
		}
		planIndex++

		teardownExporter, err := buildPlannedTask(planID, task.TaskTypeTeardownExporter, planIndex,
			&task.TeardownExporterParams{
				ExporterName: exporterName,
				Namespace:    group.Namespace,
			})
		if err != nil {
			return nil, err
		}
		planIndex++

		tasks = append(tasks, createExporter, awaitExporter, submitExport, teardownExporter)
	}

	assembleTask, err := buildPlannedTask(planID, assembleTaskType, planIndex, assembleParams)
	if err != nil {
		return nil, err
	}
	assembleTask.MaxRetries = groupAssemblyMaxRetries
	planIndex++

	collectPeersParams := &task.CollectAndSetPeersParams{
		GroupName: group.Name,
		Namespace: group.Namespace,
		NodeNames: incumbentNodes,
	}
	collectPeersTask, err := buildPlannedTask(planID, task.TaskTypeCollectAndSetPeers, planIndex, collectPeersParams)
	if err != nil {
		return nil, err
	}
	planIndex++

	awaitParams := &task.AwaitNodesRunningParams{
		GroupName: group.Name,
		Namespace: group.Namespace,
		Expected:  len(incumbentNodes),
		NodeNames: incumbentNodes,
	}
	awaitTask, err := buildPlannedTask(planID, TaskAwaitNodesRunning, planIndex, awaitParams)
	if err != nil {
		return nil, err
	}

	tasks = append(tasks, assembleTask, collectPeersTask, awaitTask)

	return &seiv1alpha1.TaskPlan{
		ID:    planID,
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: tasks,
	}, nil
}
