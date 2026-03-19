package node

import (
	"fmt"
	"strconv"

	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

type fullNodePlanner struct{}

func (p *fullNodePlanner) Mode() string { return string(seiconfig.ModeFull) }

func (p *fullNodePlanner) Validate(node *seiv1alpha1.SeiNode) error {
	if node.Spec.FullNode == nil {
		return fmt.Errorf("fullNode sub-spec is nil")
	}
	return nil
}

func (p *fullNodePlanner) BuildPlan(node *seiv1alpha1.SeiNode) *seiv1alpha1.TaskPlan {
	fn := node.Spec.FullNode
	return buildPlan(node, fn.Peers, fn.Snapshot)
}

func (p *fullNodePlanner) BuildTask(node *seiv1alpha1.SeiNode, taskType string) (sidecar.TaskBuilder, error) {
	if taskType == taskConfigApply {
		return p.buildConfigApply(node), nil
	}
	fn := node.Spec.FullNode
	return buildSharedTask(node, fn.Peers, fn.Snapshot, taskType)
}

func (p *fullNodePlanner) buildConfigApply(node *seiv1alpha1.SeiNode) sidecar.TaskBuilder {
	intent := seiconfig.ConfigIntent{
		Mode:      seiconfig.ModeFull,
		Overrides: mergeOverrides(p.controllerOverrides(node), node.Spec.Overrides),
	}
	return sidecar.ConfigApplyTask{Intent: intent}
}

func (p *fullNodePlanner) controllerOverrides(node *seiv1alpha1.SeiNode) map[string]string {
	overrides := make(map[string]string)
	sg := node.Spec.FullNode.SnapshotGeneration
	if sg != nil {
		overrides["storage.pruning"] = valNothing
		overrides["storage.snapshot_interval"] = strconv.FormatInt(defaultSnapshotInterval, 10)
		overrides["storage.snapshot_keep_recent"] = strconv.FormatInt(int64(sg.KeepRecent), 10)
	}
	return overrides
}
