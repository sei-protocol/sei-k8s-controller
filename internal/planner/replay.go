package planner

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

type replayerPlanner struct {
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
	if len(node.Spec.Peers) == 0 {
		return fmt.Errorf("replayer requires at least one peer source for block sync")
	}
	if err := validateResultExport(node.Spec.Replayer.ResultExport); err != nil {
		return fmt.Errorf("replayer: %w", err)
	}
	return nil
}

func (p *replayerPlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		return p.buildRunningPlan(node)
	}
	intent := &seiconfig.ConfigIntent{
		Mode:      seiconfig.ModeFull,
		Overrides: mergeOverrides(mergeOverrides(commonOverrides(node), p.controllerOverrides()), node.Spec.Overrides),
	}
	if NeedsBootstrap(node) {
		return buildBootstrapPlan(node, node.Spec.Peers, &node.Spec.Replayer.Snapshot, intent)
	}
	return buildBasePlan(node, node.Spec.Peers, &node.Spec.Replayer.Snapshot, intent)
}

// buildRunningPlan returns the update plan for a Running replayer node.
// Same shape as full and archive.
func (p *replayerPlanner) buildRunningPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if imageDrifted(node) {
		setNodeUpdateCondition(node, metav1.ConditionTrue, "UpdateStarted", imageDriftMessage(node))
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

func (p *replayerPlanner) controllerOverrides() map[string]string {
	return map[string]string{
		keySCAsyncCommitBuffer:       "100",
		keySCSnapshotKeepRecent:      "2",
		keySCSnapshotMinTimeInterval: "3600",
	}
}
