package planner

import (
	"fmt"
	"slices"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
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
) (*seiv1alpha1.TaskPlan, error) {
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

	appendTask := func(taskType string, params any) error {
		t, err := buildPlannedTask(node, taskType, nextAttempt(taskType), params)
		if err != nil {
			return err
		}
		tasks = append(tasks, t)
		return nil
	}

	// Phase 1: Deploy bootstrap infrastructure
	if err := appendTask(task.TaskTypeDeployBootstrapSvc,
		&task.DeployBootstrapServiceParams{ServiceName: serviceName, Namespace: node.Namespace}); err != nil {
		return nil, err
	}
	if err := appendTask(task.TaskTypeDeployBootstrapJob,
		&task.DeployBootstrapJobParams{JobName: jobName, Namespace: node.Namespace}); err != nil {
		return nil, err
	}

	// Phase 2: Sidecar tasks on bootstrap pod (same progression as base, minus mark-ready)
	for _, taskType := range bootstrapProg {
		if err := appendTask(taskType, paramsForTaskType(node, taskType, peers, snap, snapshotRegion, configApplyParams)); err != nil {
			return nil, err
		}
	}

	// Phase 3: Wait for seid to reach halt-height, then tear down
	if err := appendTask(task.TaskTypeAwaitBootstrapComplete,
		&task.AwaitBootstrapCompleteParams{JobName: jobName, Namespace: node.Namespace}); err != nil {
		return nil, err
	}
	if err := appendTask(task.TaskTypeTeardownBootstrap,
		&task.TeardownBootstrapParams{JobName: jobName, ServiceName: serviceName, Namespace: node.Namespace}); err != nil {
		return nil, err
	}

	// Phase 4: Post-bootstrap config on StatefulSet pod
	for _, taskType := range postProg {
		if err := appendTask(taskType, paramsForTaskType(node, taskType, peers, nil, "", configApplyParams)); err != nil {
			return nil, err
		}
	}

	return &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive, Tasks: tasks}, nil
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

	return prog
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
			return t.Status == seiv1alpha1.TaskComplete
		}
	}
	return true
}

// genesisConfigureMaxRetries is the number of times configure-genesis can
// retry before the plan is marked failed. At the default 10s poll interval
// this gives ~30 minutes for the group controller to assemble and upload
// genesis.json.
const genesisConfigureMaxRetries = 180

// buildGenesisInitPlan constructs the full Init plan for genesis ceremony
// nodes. Per-node artifact generation and upload runs first, then
// configure-genesis retries until the group controller has assembled and
// uploaded genesis.json to S3.
func buildGenesisInitPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	gc := node.Spec.Validator.GenesisCeremony
	attempt := 0

	prog := []string{
		TaskGenerateIdentity,
		TaskGenerateGentx,
		TaskUploadGenesisArtifacts,
		TaskConfigureGenesis,
		TaskConfigApply,
		TaskSetGenesisPeers,
		TaskConfigValidate,
		TaskMarkReady,
	}

	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		t, err := buildPlannedTask(node, taskType, attempt, genesisParamsForTaskType(node, gc, taskType))
		if err != nil {
			return nil, err
		}
		if taskType == TaskConfigureGenesis {
			t.MaxRetries = genesisConfigureMaxRetries
		}
		tasks[i] = t
	}
	return &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive, Tasks: tasks}, nil
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
	case TaskSetGenesisPeers:
		return &task.SetGenesisPeersParams{
			S3Bucket: gc.ArtifactS3.Bucket,
			S3Key:    gc.ArtifactS3.Prefix + "peers.json",
			S3Region: gc.ArtifactS3.Region,
		}
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
	snap := node.Spec.SnapshotSource()
	if snap == nil || snap.S3 == nil || snap.S3.TargetHeight <= 0 {
		return nil, fmt.Errorf("node %s/%s has no valid S3 targetHeight", node.Namespace, node.Name)
	}
	return &task.AwaitConditionParams{
		Condition:    sidecar.ConditionHeight,
		TargetHeight: snap.S3.TargetHeight,
		Action:       sidecar.ActionSIGTERM,
	}, nil
}

// SnapshotUploadMonitorTask returns a snapshot-upload TaskRequest if applicable.
// The sidecar handler runs in a loop at its configured interval (SEI_SNAPSHOT_UPLOAD_INTERVAL).
// The controller submits this once and tracks it as a monitor task.
func SnapshotUploadMonitorTask(node *seiv1alpha1.SeiNode) *sidecar.TaskRequest {
	sg := SnapshotGeneration(node)
	if sg == nil || sg.Destination == nil || sg.Destination.S3 == nil {
		return nil
	}
	dest := sg.Destination.S3
	req := sidecar.SnapshotUploadTask{
		Bucket: dest.Bucket,
		Prefix: dest.Prefix,
		Region: dest.Region,
	}.ToTaskRequest()
	return &req
}

// ResultExportMonitorTask builds a TaskRequest for result-export comparison
// mode. The sidecar compares local block results against the canonical RPC
// and completes on app-hash divergence. Returns nil when the node has no
// result-export config.
func ResultExportMonitorTask(node *seiv1alpha1.SeiNode, platformCfg platform.Config) *sidecar.TaskRequest {
	if node.Spec.Replayer == nil || node.Spec.Replayer.ResultExport == nil {
		return nil
	}
	re := node.Spec.Replayer.ResultExport
	req := sidecar.ResultExportTask{
		Bucket:       platformCfg.ResultExportBucket,
		Prefix:       platformCfg.ResultExportPrefix + node.Spec.ChainID + "/",
		Region:       platformCfg.ResultExportRegion,
		CanonicalRPC: re.CanonicalRPC,
	}.ToTaskRequest()
	return &req
}
