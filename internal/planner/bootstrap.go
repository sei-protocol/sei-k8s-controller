package planner

import (
	"fmt"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	defaultSnapshotUploadCron = "0 0 * * *"
	defaultResultExportCron   = "*/10 * * * *"
	resultExportBucket        = "sei-node-mvp"
	resultExportRegion        = "eu-central-1"
	resultExportPrefix        = "shadow-results/"
)

// BuildPreInitPlan constructs the task plan for the PreInit Job. For
// snapshot-bootstrap nodes, it builds the standard bootstrap sequence. For
// all other nodes it returns an empty plan that resolves trivially.
func BuildPreInitPlan(node *seiv1alpha1.SeiNode, planner NodePlanner) *seiv1alpha1.TaskPlan {
	if !NeedsPreInit(node) {
		return &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive, Tasks: []seiv1alpha1.PlannedTask{}}
	}
	return planner.BuildPlan(node)
}

// BuildPostBootstrapInitPlan constructs a reduced InitPlan for nodes that
// completed a PreInit Job. The PVC already contains blockchain data synced
// to the target height.
func BuildPostBootstrapInitPlan(node *seiv1alpha1.SeiNode, configApplyParams *task.ConfigApplyParams) *seiv1alpha1.TaskPlan {
	peers := PeersFor(node)
	attempt := 0

	prog := []string{TaskConfigureGenesis, TaskConfigApply}
	if len(peers) > 0 {
		prog = append(prog, TaskDiscoverPeers)
	}
	prog = append(prog, TaskConfigValidate, TaskMarkReady)

	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		tasks[i] = buildPlannedTask(node, taskType, attempt, paramsForTaskType(node, taskType, peers, nil, "", configApplyParams))
	}
	return &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive, Tasks: tasks}
}

// BuildGenesisInitPlan constructs the full Init plan for genesis ceremony
// nodes. Per-node artifact generation runs first, then await-genesis-assembly
// blocks until the group controller has assembled and uploaded genesis.json.
func BuildGenesisInitPlan(node *seiv1alpha1.SeiNode) *seiv1alpha1.TaskPlan {
	gc := node.Spec.Validator.GenesisCeremony
	attempt := 0

	prog := []string{
		TaskGenerateIdentity,
		TaskGenerateGentx,
		TaskUploadGenesisArtifacts,
		TaskAwaitGenesisAssembly,
		TaskConfigureGenesis,
		TaskConfigApply,
		TaskDiscoverPeers,
		TaskConfigValidate,
		TaskMarkReady,
	}

	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		tasks[i] = buildPlannedTask(node, taskType, attempt, genesisParamsForTaskType(node, gc, taskType))
	}
	return &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive, Tasks: tasks}
}

func genesisParamsForTaskType(node *seiv1alpha1.SeiNode, gc *seiv1alpha1.GenesisCeremonyNodeConfig, taskType string) any {
	switch taskType {
	case TaskGenerateIdentity:
		return &task.GenerateIdentityParams{
			ChainID: gc.ChainID,
			Moniker: node.Name,
		}
	case TaskGenerateGentx:
		return &task.GenerateGentxParams{
			ChainID:        gc.ChainID,
			StakingAmount:  gc.StakingAmount,
			AccountBalance: gc.AccountBalance,
			GenesisParams:  gc.GenesisParams,
		}
	case TaskUploadGenesisArtifacts:
		return &task.UploadGenesisArtifactsParams{
			S3Bucket: gc.ArtifactS3.Bucket,
			S3Prefix: gc.ArtifactS3.Prefix,
			S3Region: gc.ArtifactS3.Region,
			NodeName: node.Name,
		}
	case TaskAwaitGenesisAssembly:
		return &task.AwaitGenesisAssemblyParams{
			S3Bucket: gc.ArtifactS3.Bucket,
			S3Prefix: gc.ArtifactS3.Prefix,
			S3Region: gc.ArtifactS3.Region,
		}
	case TaskConfigureGenesis:
		s3URI := fmt.Sprintf("s3://%s/%sgenesis.json", gc.ArtifactS3.Bucket, gc.ArtifactS3.Prefix)
		return &task.ConfigureGenesisParams{
			URI:    s3URI,
			Region: gc.ArtifactS3.Region,
		}
	case TaskConfigApply:
		return &task.ConfigApplyParams{
			Mode:      "validator",
			Overrides: mergeOverrides(nil, node.Spec.Overrides),
		}
	case TaskDiscoverPeers:
		return discoverPeersParams(PeersFor(node))
	case TaskConfigValidate:
		return &task.ConfigValidateParams{}
	case TaskMarkReady:
		return &task.MarkReadyParams{}
	default:
		return nil
	}
}

// AwaitConditionParams builds await-condition params from a node's snapshot source.
func AwaitConditionParams(node *seiv1alpha1.SeiNode) (*task.AwaitConditionParams, error) {
	snap := SnapshotSourceFor(node)
	if snap == nil || snap.S3 == nil || snap.S3.TargetHeight <= 0 {
		return nil, fmt.Errorf("node %s/%s has no valid S3 targetHeight", node.Namespace, node.Name)
	}
	return &task.AwaitConditionParams{
		Condition:    sidecar.ConditionHeight,
		TargetHeight: snap.S3.TargetHeight,
		Action:       sidecar.ActionSIGTERM,
	}, nil
}

// SnapshotUploadScheduledTask returns a snapshot-upload task builder if applicable.
func SnapshotUploadScheduledTask(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	sg := SnapshotGeneration(node)
	if sg == nil || sg.Destination == nil || sg.Destination.S3 == nil {
		return nil
	}
	dest := sg.Destination.S3
	cron := defaultSnapshotUploadCron
	return sidecar.SnapshotUploadTask{
		Bucket:   dest.Bucket,
		Prefix:   dest.Prefix,
		Region:   dest.Region,
		Schedule: &sidecar.ScheduleConfig{Cron: &cron},
	}
}

// ResultExportScheduledTask returns a result-export task builder if applicable.
func ResultExportScheduledTask(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	if node.Spec.Replayer == nil || node.Spec.Replayer.ResultExport == nil {
		return nil
	}
	cron := defaultResultExportCron
	return sidecar.ResultExportTask{
		Bucket:   resultExportBucket,
		Prefix:   resultExportPrefix + node.Spec.ChainID + "/",
		Region:   resultExportRegion,
		Schedule: &sidecar.ScheduleConfig{Cron: &cron},
	}
}
