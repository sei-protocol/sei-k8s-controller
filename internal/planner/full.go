package planner

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

type fullNodePlanner struct {
}

func (p *fullNodePlanner) Mode() string { return string(seiconfig.ModeFull) }

func (p *fullNodePlanner) Validate(node *seiv1alpha1.SeiNode) error {
	if node.Spec.FullNode == nil {
		return fmt.Errorf("fullNode sub-spec is nil")
	}
	if snap := node.Spec.FullNode.Snapshot; snap != nil && snap.BootstrapImage != "" {
		if snap.S3 == nil || snap.S3.TargetHeight <= 0 {
			return fmt.Errorf("fullNode: bootstrapImage requires s3 with targetHeight > 0")
		}
	}
	if err := validateSnapshotGeneration(node.Spec.FullNode.SnapshotGeneration); err != nil {
		return fmt.Errorf("fullNode: %w", err)
	}
	return nil
}

func (p *fullNodePlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		return buildRunningPlan(node)
	}
	fn := node.Spec.FullNode
	params := &task.ConfigApplyParams{
		Mode:      string(seiconfig.ModeFull),
		Overrides: mergeOverrides(mergeOverrides(commonOverrides(node), p.controllerOverrides(node)), node.Spec.Overrides),
	}
	if NeedsBootstrap(node) {
		return buildBootstrapPlan(node, node.Spec.Peers, fn.Snapshot, params)
	}
	return buildBasePlan(node, node.Spec.Peers, fn.Snapshot, params)
}

func (p *fullNodePlanner) controllerOverrides(node *seiv1alpha1.SeiNode) map[string]string {
	sg := node.Spec.FullNode.SnapshotGeneration
	if sg == nil || sg.Tendermint == nil {
		return nil
	}
	return seiconfig.SnapshotGenerationOverrides(sg.Tendermint.KeepRecent)
}
