package planner

import (
	"fmt"
	"strconv"

	seiconfig "github.com/sei-protocol/sei-config"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

type archiveNodePlanner struct {
}

func (p *archiveNodePlanner) Mode() string { return string(seiconfig.ModeArchive) }

func (p *archiveNodePlanner) Validate(node *seiv1alpha1.SeiNode) error {
	if node.Spec.Archive == nil {
		return fmt.Errorf("archive sub-spec is nil")
	}
	if err := validateSnapshotGeneration(node.Spec.Archive.SnapshotGeneration); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	return nil
}

func (p *archiveNodePlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		return buildRunningPlan(node)
	}
	params, err := p.BuildConfigIntent(node)
	if err != nil {
		return nil, err
	}
	return buildBasePlan(node, node.Spec.Peers, nil, params)
}

func (p *archiveNodePlanner) BuildConfigIntent(node *seiv1alpha1.SeiNode) (*seiconfig.ConfigIntent, error) {
	if node.Spec.Archive == nil {
		return nil, fmt.Errorf("archive sub-spec is nil")
	}
	return &seiconfig.ConfigIntent{
		Mode:      seiconfig.ModeArchive,
		Overrides: mergeOverrides(mergeOverrides(commonOverrides(node), p.controllerOverrides(node)), node.Spec.Overrides),
	}, nil
}

func (p *archiveNodePlanner) controllerOverrides(node *seiv1alpha1.SeiNode) map[string]string {
	sg := node.Spec.Archive.SnapshotGeneration
	if sg == nil || sg.Tendermint == nil {
		return nil
	}
	return map[string]string{
		seiconfig.KeySnapshotInterval:   strconv.FormatInt(seiconfig.DefaultSnapshotInterval, 10),
		seiconfig.KeySnapshotKeepRecent: strconv.FormatInt(int64(sg.Tendermint.KeepRecent), 10),
	}
}
