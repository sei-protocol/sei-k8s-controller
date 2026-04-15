package planner

import (
	"fmt"
	"strconv"

	seiconfig "github.com/sei-protocol/sei-config"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

type archiveNodePlanner struct {
}

func (p *archiveNodePlanner) Mode() string { return string(seiconfig.ModeArchive) }

func (p *archiveNodePlanner) Validate(node *seiv1alpha1.SeiNode) error {
	if node.Spec.Archive == nil {
		return fmt.Errorf("archive sub-spec is nil")
	}
	return nil
}

func (p *archiveNodePlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if NeedsNodeUpdate(node) {
		return buildNodeUpdatePlan(node, p.configApplyParams(node))
	}
	if node.Status.Phase != "" && node.Status.Phase != seiv1alpha1.PhasePending {
		return nil, nil
	}
	return buildBasePlan(node, node.Spec.Peers, nil, p.configApplyParams(node))
}

func (p *archiveNodePlanner) configApplyParams(node *seiv1alpha1.SeiNode) *task.ConfigApplyParams {
	return &task.ConfigApplyParams{
		Mode:      string(seiconfig.ModeArchive),
		Overrides: mergeOverrides(mergeOverrides(commonOverrides(node), p.controllerOverrides(node)), node.Spec.Overrides),
	}
}

func (p *archiveNodePlanner) controllerOverrides(node *seiv1alpha1.SeiNode) map[string]string {
	sg := node.Spec.Archive.SnapshotGeneration
	if sg == nil {
		return nil
	}
	return map[string]string{
		seiconfig.KeySnapshotInterval:   strconv.FormatInt(seiconfig.DefaultSnapshotInterval, 10),
		seiconfig.KeySnapshotKeepRecent: strconv.FormatInt(int64(sg.KeepRecent), 10),
	}
}
