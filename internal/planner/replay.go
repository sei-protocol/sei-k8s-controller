package planner

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

type replayerPlanner struct {
	snapshotBucket string
	snapshotRegion string
}

func (p *replayerPlanner) Mode() string { return string(seiconfig.ModeFull) }

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

func (p *replayerPlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	params := &task.ConfigApplyParams{
		Mode:      string(seiconfig.ModeFull),
		Overrides: mergeOverrides(p.controllerOverrides(), node.Spec.Overrides),
	}
	if NeedsBootstrap(node) {
		return buildBootstrapPlan(node, node.Spec.Replayer.Peers, &node.Spec.Replayer.Snapshot, p.snapshotBucket, p.snapshotRegion, params)
	}
	return buildBasePlan(node, node.Spec.Replayer.Peers, &node.Spec.Replayer.Snapshot, p.snapshotBucket, p.snapshotRegion, params)
}

func (p *replayerPlanner) controllerOverrides() map[string]string {
	return map[string]string{
		keyConcurrencyWorkers: defaultConcurrencyWorkers,
		keyPruning:            valCustom,
		keyPruningKeepRecent:  "86400",
		keyPruningKeepEvery:   "500",
		keyPruningInterval:    "10",

		keySCAsyncCommitBuffer:       "100",
		keySCSnapshotKeepRecent:      "2",
		keySCSnapshotMinTimeInterval: "3600",
	}
}
