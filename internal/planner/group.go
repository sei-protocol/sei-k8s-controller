package planner

import (
	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	"k8s.io/apimachinery/pkg/api/resource"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	"github.com/sei-protocol/sei-k8s-controller/internal/task/export"
)

const (
	groupAssemblyMaxRetries = 180
	TaskAssembleGenesisFork = sidecar.TaskTypeAssembleGenesisFork
)

type genesisGroupPlanner struct{}

// exporterPVCSize sizes the fork-genesis exporter PVC. The bootstrap Job
// writes seid's data dir up to halt-height into this PVC, so the size must
// hold the full state at that height — not just the exported JSON. 500Gi
// is enough for low-state test chains; pacific-1-scale chains will need a
// per-SND override before the fork ceremony runs there.
func exporterPVCSize() (resource.Quantity, error) {
	return resource.ParseQuantity("500Gi")
}

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

	// Select assembler task based on whether this is a fork ceremony.
	// Validate at planner-time so bech32 / shape errors hit
	// `kubectl describe seinodedeployment` rather than a sidecar Job pod.
	assembleTaskType := TaskAssembleGenesis
	var assembleParams any

	if hasCondition(group, seiv1alpha1.ConditionForkGenesisCeremonyNeeded) && group.Spec.Genesis.Fork != nil {
		assembleTaskType = TaskAssembleGenesisFork
		p := &task.AssembleForkGenesisParams{
			SourceChainID:  group.Spec.Genesis.Fork.SourceChainID,
			ChainID:        group.Spec.Genesis.ChainID,
			AccountBalance: group.Spec.Genesis.AccountBalance,
			Namespace:      group.Namespace,
			Nodes:          nodeParams,
			Accounts:       accounts,
		}
		if err := p.Validate(); err != nil {
			return nil, err
		}
		assembleParams = p
	} else {
		p := &task.AssembleAndUploadGenesisParams{
			AccountBalance: group.Spec.Genesis.AccountBalance,
			Namespace:      group.Namespace,
			Nodes:          nodeParams,
			Accounts:       accounts,
		}
		if err := p.Validate(); err != nil {
			return nil, err
		}
		assembleParams = p
	}

	var tasks []seiv1alpha1.PlannedTask

	// For fork ceremonies, prepend the SND-driven exporter sub-plan: PVC,
	// bootstrap Job, await, export Job, await, teardown.
	if hasCondition(group, seiv1alpha1.ConditionForkGenesisCeremonyNeeded) && group.Spec.Genesis.Fork != nil {
		ns := group.Namespace
		root := group.Name + "-exporter"
		bootstrapJob := root + "-bootstrap"
		exportJob := root + "-export"
		serviceName := root

		size, err := exporterPVCSize()
		if err != nil {
			return nil, err
		}

		ensurePVC, err := buildPlannedTask(planID, export.TaskTypeEnsurePVC, planIndex,
			&export.EnsureExporterPVCParams{
				PVCName:   export.PVCName(group.Name),
				Namespace: ns,
				Size:      size,
			})
		if err != nil {
			return nil, err
		}
		planIndex++

		applyBootstrap, err := buildPlannedTask(planID, export.TaskTypeApplyBootstrapJob, planIndex,
			&export.ApplyBootstrapJobParams{Namespace: ns})
		if err != nil {
			return nil, err
		}
		planIndex++

		awaitBootstrap, err := buildPlannedTask(planID, export.TaskTypeAwaitBootstrapJob, planIndex,
			&export.AwaitJobParams{JobName: bootstrapJob, Namespace: ns})
		if err != nil {
			return nil, err
		}
		planIndex++

		applyExport, err := buildPlannedTask(planID, export.TaskTypeApplyExportJob, planIndex,
			&export.ApplyExportJobParams{Namespace: ns})
		if err != nil {
			return nil, err
		}
		planIndex++

		awaitExport, err := buildPlannedTask(planID, export.TaskTypeAwaitExportJob, planIndex,
			&export.AwaitJobParams{JobName: exportJob, Namespace: ns})
		if err != nil {
			return nil, err
		}
		planIndex++

		teardown, err := buildPlannedTask(planID, export.TaskTypeTeardownExporter, planIndex,
			&export.TeardownExporterParams{
				PVCName:      export.PVCName(group.Name),
				BootstrapJob: bootstrapJob,
				ExportJob:    exportJob,
				ServiceName:  serviceName,
				Namespace:    ns,
			})
		if err != nil {
			return nil, err
		}
		planIndex++

		tasks = append(tasks, ensurePVC, applyBootstrap, awaitBootstrap, applyExport, awaitExport, teardown)
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
