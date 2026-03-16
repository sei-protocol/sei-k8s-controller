package node

import (
	"strconv"

	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

type archiveNodePlanner struct{}

func (p *archiveNodePlanner) Mode() string { return string(seiconfig.ModeArchive) }

func (p *archiveNodePlanner) Validate(_ *seiv1alpha1.SeiNode) error {
	return nil
}

func (p *archiveNodePlanner) BuildPlan(node *seiv1alpha1.SeiNode) *seiv1alpha1.TaskPlan {
	// Archive always block syncs from genesis — no snapshot restore.
	prog := []string{taskConfigApply, taskConfigValidate, taskMarkReady}

	if node.Spec.Genesis.S3 != nil {
		prog = insertBefore(prog, taskConfigApply, taskConfigureGenesis)
	}
	if node.Spec.Archive.Peers != nil {
		prog = insertBefore(prog, taskConfigValidate, taskDiscoverPeers)
	}

	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		tasks[i] = seiv1alpha1.PlannedTask{
			Type:   taskType,
			Status: seiv1alpha1.PlannedTaskPending,
		}
	}
	return &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: tasks,
	}
}

func (p *archiveNodePlanner) BuildTask(node *seiv1alpha1.SeiNode, taskType string) sidecar.TaskBuilder {
	switch taskType {
	case taskConfigApply:
		return p.buildConfigApply(node)
	case taskDiscoverPeers:
		return discoverPeersFromConfig(node.Spec.Archive.Peers)
	case taskConfigureGenesis:
		return configureGenesisBuilder(node)
	case taskConfigValidate:
		return sidecar.ConfigValidateTask{}
	case taskMarkReady:
		return sidecar.MarkReadyTask{}
	default:
		return sidecar.MarkReadyTask{}
	}
}

func (p *archiveNodePlanner) buildConfigApply(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	intent := seiconfig.ConfigIntent{
		Mode:      seiconfig.ModeArchive,
		Overrides: p.controllerOverrides(node),
	}
	return sidecar.ConfigApplyTask{Intent: intent}
}

// Archive mode defaults already set pruning=nothing, so the controller only
// needs to inject snapshot-interval and keep-recent when generation is enabled.
func (p *archiveNodePlanner) controllerOverrides(node *seiv1alpha1.SeiNode) map[string]string {
	overrides := make(map[string]string)
	sg := node.Spec.Archive.SnapshotGeneration
	if sg != nil {
		overrides["storage.snapshot_interval"] = strconv.FormatInt(defaultSnapshotInterval, 10)
		overrides["storage.snapshot_keep_recent"] = strconv.FormatInt(int64(sg.KeepRecent), 10)
	}
	return overrides
}
