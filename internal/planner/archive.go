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
	return buildBasePlan(node, node.Spec.Peers, p.snapshotSource(), &task.ConfigApplyParams{
		Mode:      string(seiconfig.ModeArchive),
		Overrides: mergeOverrides(mergeOverrides(commonOverrides(node), p.controllerOverrides(node)), node.Spec.Overrides),
	})
}

func (p *archiveNodePlanner) snapshotSource() *seiv1alpha1.SnapshotSource {
	return &seiv1alpha1.SnapshotSource{StateSync: &seiv1alpha1.StateSyncSource{}}
}

func (p *archiveNodePlanner) controllerOverrides(node *seiv1alpha1.SeiNode) map[string]string {
	overrides := map[string]string{
		keyConcurrencyWorkers: defaultConcurrencyWorkers,
	}
	sg := node.Spec.Archive.SnapshotGeneration
	if sg != nil {
		overrides[keySnapshotInterval] = strconv.FormatInt(defaultSnapshotInterval, 10)
		overrides[keySnapshotKeepRecent] = strconv.FormatInt(int64(sg.KeepRecent), 10)
	}
	return overrides
}
