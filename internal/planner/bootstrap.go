package planner

import (
	"github.com/google/uuid"
	seiconfig "github.com/sei-protocol/sei-config"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// buildBootstrapPlan constructs a unified Plan for nodes that need a
// bootstrap Job. The plan includes controller-side tasks for
// Job/Service lifecycle, sidecar tasks that run on the bootstrap pod, and
// post-bootstrap config tasks that run on the production StatefulSet pod.
func buildBootstrapPlan(
	node *seiv1alpha1.SeiNode,
	peers []seiv1alpha1.PeerSource,
	snap *seiv1alpha1.SnapshotSource,
	configApplyParams *task.ConfigApplyParams,
) (*seiv1alpha1.TaskPlan, error) {
	planID := uuid.New().String()
	planIndex := 0

	jobName := task.BootstrapJobName(node)
	serviceName := node.Name

	bootstrapProg, err := buildSidecarProgression(snap, peers)
	if err != nil {
		return nil, err
	}
	postProg, err := buildPostBootstrapProgression(node, peers)
	if err != nil {
		return nil, err
	}
	tasks := make([]seiv1alpha1.PlannedTask, 0, 2+len(bootstrapProg)+2+len(postProg))

	appendTask := func(taskType string, params any) error {
		t, err := buildPlannedTask(planID, taskType, planIndex, params)
		if err != nil {
			return err
		}
		tasks = append(tasks, t)
		planIndex++
		return nil
	}

	// Phase 0: Ensure the data PVC exists (needed by both bootstrap Job and StatefulSet)
	if err := appendTask(task.TaskTypeEnsureDataPVC,
		&task.EnsureDataPVCParams{NodeName: node.Name, Namespace: node.Namespace}); err != nil {
		return nil, err
	}

	if needsValidateSigningKey(node) {
		if err := appendTask(task.TaskTypeValidateSigningKey,
			validateSigningKeyParams(node)); err != nil {
			return nil, err
		}
	}
	if needsValidateNodeKey(node) {
		if err := appendTask(task.TaskTypeValidateNodeKey,
			validateNodeKeyParams(node)); err != nil {
			return nil, err
		}
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
		if err := appendTask(taskType, paramsForTaskType(node, taskType, snap, configApplyParams)); err != nil {
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

	// Phase 4: Create production StatefulSet and Service (after bootstrap teardown frees the PVC)
	if err := appendTask(task.TaskTypeApplyStatefulSet,
		&task.ApplyStatefulSetParams{NodeName: node.Name, Namespace: node.Namespace}); err != nil {
		return nil, err
	}
	if err := appendTask(task.TaskTypeApplyService,
		&task.ApplyServiceParams{NodeName: node.Name, Namespace: node.Namespace}); err != nil {
		return nil, err
	}

	// Phase 5: Post-bootstrap config on StatefulSet pod
	for _, taskType := range postProg {
		if err := appendTask(taskType, paramsForTaskType(node, taskType, nil, configApplyParams)); err != nil {
			return nil, err
		}
	}

	return &seiv1alpha1.TaskPlan{
		ID:          planID,
		Phase:       seiv1alpha1.TaskPlanActive,
		Tasks:       tasks,
		TargetPhase: seiv1alpha1.PhaseRunning,
		FailedPhase: seiv1alpha1.PhaseFailed,
	}, nil
}

// buildPostBootstrapProgression returns the sidecar task sequence for the
// production StatefulSet after bootstrap teardown. This is intentionally
// a separate, hand-written progression — it runs on the production pod
// after bootstrap teardown and only includes the config tasks needed to
// prepare the already-restored data directory for production use.
func buildPostBootstrapProgression(node *seiv1alpha1.SeiNode, peers []seiv1alpha1.PeerSource) ([]string, error) {
	prog := []string{TaskConfigureGenesis, TaskConfigApply}
	if len(peers) > 0 {
		prog = append(prog, TaskDiscoverPeers)
	}
	prog = append(prog, TaskConfigValidate, TaskMarkReady)
	if sg := SnapshotGeneration(node); sg != nil && sg.Tendermint != nil && sg.Tendermint.Publish != nil {
		return insertBefore(prog, TaskMarkReady, TaskSnapshotUpload)
	}
	return prog, nil
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

// buildGenesisPlan constructs the full plan for genesis ceremony
// nodes. Per-node artifact generation and upload runs first, then
// configure-genesis retries until the group controller has assembled and
// uploaded genesis.json to S3.
func buildGenesisPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	configApplyParams := &task.ConfigApplyParams{
		Mode:      string(seiconfig.ModeValidator),
		Overrides: mergeOverrides(commonOverrides(node), node.Spec.Overrides),
	}

	prog := []string{
		task.TaskTypeEnsureDataPVC,
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		TaskGenerateIdentity,
		TaskGenerateGentx,
		TaskUploadGenesisArtifacts,
		TaskConfigureGenesis,
		TaskConfigApply,
		TaskSetGenesisPeers,
		TaskConfigValidate,
		TaskMarkReady,
	}

	planID := uuid.New().String()
	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		t, err := buildPlannedTask(planID, taskType, i, paramsForTaskType(node, taskType, nil, configApplyParams))
		if err != nil {
			return nil, err
		}
		if taskType == TaskConfigureGenesis {
			t.MaxRetries = genesisConfigureMaxRetries
		}
		tasks[i] = t
	}
	return &seiv1alpha1.TaskPlan{
		ID:          planID,
		Phase:       seiv1alpha1.TaskPlanActive,
		Tasks:       tasks,
		TargetPhase: seiv1alpha1.PhaseRunning,
		FailedPhase: seiv1alpha1.PhaseFailed,
	}, nil
}
