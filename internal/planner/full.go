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

// BuildPlan dispatches between startup (init/bootstrap) and existing-resource
// (day-2) shapes. Reconciler/executor just call this and get a plan; the
// startup-vs-day-2 distinction stays inside the planner.
func (p *fullNodePlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		return p.buildRunningPlan(node)
	}
	fn := node.Spec.FullNode
	intent := &seiconfig.ConfigIntent{
		Mode:      seiconfig.ModeFull,
		Overrides: mergeOverrides(mergeOverrides(commonOverrides(node), p.controllerOverrides(node)), node.Spec.Overrides),
	}
	if NeedsBootstrap(node) {
		return buildBootstrapPlan(node, node.Spec.Peers, fn.Snapshot, intent)
	}
	return buildBasePlan(node, node.Spec.Peers, fn.Snapshot, intent)
}

// buildRunningPlan returns the day-2 plan for a Running full node, or
// nil if no drift. Image drift queues a config-patch + pod-cycle plan;
// sidecar reapproval queues a one-task mark-ready plan.
//
// The day-2 patch stamps only the keys the controller directly owns
// (currently p2p.external-address for publishable P2P). TaskConfigPatch
// is a generic TOML merge — no sei-config involvement, no forced overrides,
// no mode-defaulted backfill. TaskConfigValidate after the patch is the
// parser-level gate.
//
// Note: pelletier/go-toml/v2 does not preserve comments or key ordering
// on re-encode, so the first day-2 patch erases operator-added comments.
func (p *fullNodePlanner) buildRunningPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
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
		return assembleDay2Plan(node, prog, externalAddressPatch(node))
	}
	if sidecarNeedsReapproval(node) {
		return buildMarkReadyPlan(node)
	}
	return nil, nil
}

func (p *fullNodePlanner) controllerOverrides(node *seiv1alpha1.SeiNode) map[string]string {
	sg := node.Spec.FullNode.SnapshotGeneration
	if sg == nil || sg.Tendermint == nil {
		return nil
	}
	return seiconfig.SnapshotGenerationOverrides(sg.Tendermint.KeepRecent)
}
