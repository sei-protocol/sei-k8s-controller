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
	return buildPlan(node, node.Spec.Archive.Peers, nil)
}

func (p *archiveNodePlanner) BuildTask(node *seiv1alpha1.SeiNode, taskType string) sidecar.TaskBuilder {
	if taskType == taskConfigApply {
		return p.buildConfigApply(node)
	}
	return buildSharedTask(node, node.Spec.Archive.Peers, nil, taskType)
}

func (p *archiveNodePlanner) buildConfigApply(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	intent := seiconfig.ConfigIntent{
		Mode:      seiconfig.ModeArchive,
		Overrides: mergeOverrides(p.controllerOverrides(node), node.Spec.Overrides),
	}
	return sidecar.ConfigApplyTask{Intent: intent}
}

func (p *archiveNodePlanner) controllerOverrides(node *seiv1alpha1.SeiNode) map[string]string {
	overrides := make(map[string]string)
	sg := node.Spec.Archive.SnapshotGeneration
	if sg != nil {
		overrides["storage.snapshot_interval"] = strconv.FormatInt(defaultSnapshotInterval, 10)
		overrides["storage.snapshot_keep_recent"] = strconv.FormatInt(int64(sg.KeepRecent), 10)
	}
	return overrides
}
