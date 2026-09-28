package planner

import (
	"github.com/google/uuid"
	seiconfig "github.com/sei-protocol/sei-config"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

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
	configIntent := &seiconfig.ConfigIntent{
		Mode:      seiconfig.ModeValidator,
		Overrides: mergeOverrides(commonOverrides(node), node.Spec.Overrides),
	}

	prog := []string{
		task.TaskTypeEnsureDataPVC,
		task.TaskTypeApplyRBACProxyConfig,
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
		t, err := buildPlannedTask(planID, taskType, i, paramsForTaskType(node, taskType, nil, configIntent))
		if err != nil {
			return nil, err
		}
		tasks[i] = t
	}
	return withConfigValues(&seiv1alpha1.TaskPlan{
		ID:          planID,
		Phase:       seiv1alpha1.TaskPlanActive,
		Tasks:       tasks,
		TargetPhase: seiv1alpha1.PhaseRunning,
		FailedPhase: seiv1alpha1.PhaseFailed,
	}, node)
}
