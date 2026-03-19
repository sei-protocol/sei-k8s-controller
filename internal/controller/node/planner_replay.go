package node

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

type replayerPlanner struct{}

func (p *replayerPlanner) Mode() string { return string(seiconfig.ModeArchive) }

func (p *replayerPlanner) Validate(node *seiv1alpha1.SeiNode) error {
	if node.Spec.Replayer == nil {
		return fmt.Errorf("replayer sub-spec is nil")
	}
	snap := node.Spec.Replayer.Snapshot
	if snap.S3 == nil {
		return fmt.Errorf("replayer requires an S3 snapshot source")
	}
	if snap.S3.TargetHeight <= 0 {
		return fmt.Errorf("replayer: s3.targetHeight must be > 0")
	}
	if len(node.Spec.Replayer.Peers) == 0 {
		return fmt.Errorf("replayer requires at least one peer source for block sync")
	}
	return nil
}

func (p *replayerPlanner) BuildPlan(node *seiv1alpha1.SeiNode) *seiv1alpha1.TaskPlan {
	return buildPlan(node, node.Spec.Replayer.Peers, &node.Spec.Replayer.Snapshot)
}

func (p *replayerPlanner) BuildTask(node *seiv1alpha1.SeiNode, taskType string) sidecar.TaskBuilder {
	if taskType == taskConfigApply {
		return sidecar.ConfigApplyTask{
			Intent: seiconfig.ConfigIntent{
				Mode:      seiconfig.ModeArchive,
				Overrides: mergeOverrides(nil, node.Spec.Overrides),
			},
		}
	}
	return buildSharedTask(node, node.Spec.Replayer.Peers, &node.Spec.Replayer.Snapshot, taskType)
}
