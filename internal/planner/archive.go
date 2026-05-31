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
	if err := validateSnapshotGeneration(node.Spec.Archive.SnapshotGeneration); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	return nil
}

func (p *archiveNodePlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		return p.buildRunningPlan(node)
	}
	intent := &seiconfig.ConfigIntent{
		Mode:      seiconfig.ModeArchive,
		Overrides: mergeOverrides(mergeOverrides(commonOverrides(node), p.controllerOverrides(node)), node.Spec.Overrides),
	}
	return buildBasePlan(node, node.Spec.Peers, nil, intent)
}

// buildRunningPlan returns the update plan for a Running archive node.
// Same shape as full nodes (no extra validation gates).
func (p *archiveNodePlanner) buildRunningPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if imageDrifted(node) {
		prog := []string{
			task.TaskTypeApplyStatefulSet,
			task.TaskTypeApplyService,
			TaskConfigPatch,
			TaskConfigValidate,
			task.TaskTypeReplacePod,
			task.TaskTypeObserveImage,
			TaskMarkReady,
		}
		return assembleUpdatePlan(node, prog, externalAddressPatch(node))
	}
	if sidecarNeedsReapproval(node) {
		return buildMarkReadyPlan(node)
	}
	return nil, nil
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
