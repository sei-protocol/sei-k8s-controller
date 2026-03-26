package planner

import (
	"fmt"
	"slices"

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

// buildBootstrapPlan constructs a unified InitPlan for nodes that need a
// bootstrap Job. The plan includes controller-side tasks for
// Job/Service lifecycle, sidecar tasks that run on the bootstrap pod, and
// post-bootstrap config tasks that run on the production StatefulSet pod.
func buildBootstrapPlan(
	node *seiv1alpha1.SeiNode,
	peers []seiv1alpha1.PeerSource,
	snap *seiv1alpha1.SnapshotSource,
	snapshotRegion string,
	configApplyParams *task.ConfigApplyParams,
) *seiv1alpha1.TaskPlan {
	attempts := map[string]int{}
	nextAttempt := func(taskType string) int {
		a := attempts[taskType]
		attempts[taskType] = a + 1
		return a
	}

	jobName := task.BootstrapJobName(node)
	serviceName := node.Name

	bootstrapProg := buildBootstrapProgression(peers, snap)
	postProg := buildPostBootstrapProgression(peers)
	tasks := make([]seiv1alpha1.PlannedTask, 0, 2+len(bootstrapProg)+2+len(postProg))

	// Phase 1: Deploy bootstrap infrastructure
	tasks = append(tasks, buildPlannedTask(node, task.TaskTypeDeployBootstrapSvc, nextAttempt(task.TaskTypeDeployBootstrapSvc),
		&task.DeployBootstrapServiceParams{ServiceName: serviceName, Namespace: node.Namespace}))
	tasks = append(tasks, buildPlannedTask(node, task.TaskTypeDeployBootstrapJob, nextAttempt(task.TaskTypeDeployBootstrapJob),
		&task.DeployBootstrapJobParams{JobName: jobName, Namespace: node.Namespace}))

	// Phase 2: Sidecar tasks on bootstrap pod (same progression as base, minus mark-ready)
	for _, taskType := range bootstrapProg {
		tasks = append(tasks, buildPlannedTask(node, taskType, nextAttempt(taskType),
			paramsForTaskType(node, taskType, peers, snap, snapshotRegion, configApplyParams)))
	}

	// Phase 3: Wait for seid to reach halt-height, then tear down
	tasks = append(tasks, buildPlannedTask(node, task.TaskTypeAwaitBootstrapComplete, nextAttempt(task.TaskTypeAwaitBootstrapComplete),
		&task.AwaitBootstrapCompleteParams{JobName: jobName, Namespace: node.Namespace}))
	tasks = append(tasks, buildPlannedTask(node, task.TaskTypeTeardownBootstrap, nextAttempt(task.TaskTypeTeardownBootstrap),
		&task.TeardownBootstrapParams{JobName: jobName, ServiceName: serviceName, Namespace: node.Namespace}))

	// Phase 4: Post-bootstrap config on StatefulSet pod
	for _, taskType := range postProg {
		tasks = append(tasks, buildPlannedTask(node, taskType, nextAttempt(taskType),
			paramsForTaskType(node, taskType, peers, nil, "", configApplyParams)))
	}

	return &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive, Tasks: tasks}
}

// buildBootstrapProgression returns the sidecar task sequence for the
// bootstrap Job phase (everything except mark-ready).
func buildBootstrapProgression(peers []seiv1alpha1.PeerSource, snap *seiv1alpha1.SnapshotSource) []string {
	mode := bootstrapMode(snap)
	prog := slices.Clone(baseProgression[mode])

	prog = insertBefore(prog, TaskConfigApply, TaskConfigureGenesis)
	if len(peers) > 0 {
		prog = insertBefore(prog, TaskConfigValidate, TaskDiscoverPeers)
	}
	if snap != nil {
		prog = insertBefore(prog, TaskConfigValidate, TaskConfigureStateSync)
	}

	// Remove mark-ready — the bootstrap pod isn't the production pod
	return slices.DeleteFunc(prog, func(t string) bool { return t == TaskMarkReady })
}

// buildPostBootstrapProgression returns the sidecar task sequence for the
// production StatefulSet after bootstrap teardown.
func buildPostBootstrapProgression(peers []seiv1alpha1.PeerSource) []string {
	prog := []string{TaskConfigureGenesis, TaskConfigApply}
	if len(peers) > 0 {
		prog = append(prog, TaskDiscoverPeers)
	}
	prog = append(prog, TaskConfigValidate, TaskMarkReady)
	return prog
}

// IsBootstrapComplete checks whether the teardown-bootstrap task in a plan
// is marked Complete, indicating bootstrap infrastructure has been removed.
func IsBootstrapComplete(plan *seiv1alpha1.TaskPlan) bool {
	if plan == nil {
		return false
	}
	for _, t := range plan.Tasks {
		if t.Type == task.TaskTypeTeardownBootstrap {
			return t.Status == seiv1alpha1.PlannedTaskComplete
		}
	}
	return true
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
