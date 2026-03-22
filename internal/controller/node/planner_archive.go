package node

import (
	"fmt"
	"strconv"

	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

type archiveNodePlanner struct {
	snapshotRegion string
}

func (p *archiveNodePlanner) Mode() string { return string(seiconfig.ModeArchive) }

func (p *archiveNodePlanner) Validate(node *seiv1alpha1.SeiNode) error {
	if node.Spec.Archive == nil {
		return fmt.Errorf("archive sub-spec is nil")
	}
	return nil
}

func (p *archiveNodePlanner) BuildPlan(node *seiv1alpha1.SeiNode) *seiv1alpha1.TaskPlan {
	return buildPlan(node.Spec.Archive.Peers, p.snapshotSource(node))
}

func (p *archiveNodePlanner) BuildTask(node *seiv1alpha1.SeiNode, taskType string) (sidecar.TaskBuilder, error) {
	if taskType == taskConfigApply {
		return p.buildConfigApply(node), nil
	}
	return buildSharedTask(node, node.Spec.Archive.Peers, p.snapshotSource(node), taskType, p.snapshotRegion)
}

func (p *archiveNodePlanner) snapshotSource(node *seiv1alpha1.SeiNode) *seiv1alpha1.SnapshotSource {
	if node.Spec.Archive.StateSync == nil {
		return nil
	}
	return &seiv1alpha1.SnapshotSource{StateSync: node.Spec.Archive.StateSync}
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
