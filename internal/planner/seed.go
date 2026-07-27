package planner

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

type seedPlanner struct {
	platform platform.Config
}

func (p *seedPlanner) Mode() string { return string(seiconfig.ModeSeed) }

func (p *seedPlanner) Validate(node *seiv1alpha1.SeiNode) error {
	if node.Spec.Seed == nil {
		return fmt.Errorf("seed sub-spec is nil")
	}
	// CEL already requires the field and a Secret variant, but an in-memory
	// spec that never went through admission can still reach here. A seed with
	// no pinned identity would silently generate a fresh NodeID on the data
	// volume, so fail loudly rather than serve an unstable address.
	if s := node.Spec.NodeKeySecret(); s == nil || s.SecretName == "" {
		return fmt.Errorf("seed: nodeKey.secret.secretName is required — a seed's NodeID is published and must not be regenerated")
	}
	return nil
}

// BuildPlan drives a seed through the plain genesis progression. A seed never
// bootstraps from a snapshot — it stores no chain state to restore — so the nil
// SnapshotSource selects the genesis task sequence and, via
// needsStateSyncWitnesses, omits ConfigureStateSync entirely.
func (p *seedPlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		return p.buildRunningPlan(node)
	}
	// No per-mode controller overrides: every seed-specific config key comes
	// from sei-config's applySeedOverrides, resolved sidecar-side off
	// ConfigIntent.Mode. commonOverrides still supplies the shared keys.
	intent := &seiconfig.ConfigIntent{
		Mode:      seiconfig.ModeSeed,
		Overrides: mergeOverrides(commonOverrides(node), node.Spec.Overrides),
	}
	return buildBasePlan(node, nil, intent)
}

// buildRunningPlan returns the update plan for a Running seed. Same shape as
// the other modes, with the node-key gate ahead of any StatefulSet mutation so
// a missing or malformed identity Secret fails controller-side rather than as a
// kubelet volume-mount error on the recreated pod.
func (p *seedPlanner) buildRunningPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if imageDrifted(node) || sidecarImageDrifted(node, p.platform) {
		setNodeUpdateCondition(node, metav1.ConditionTrue, "UpdateStarted", imageDriftMessage(node, p.platform))
		prog := make([]string, 0, 8)
		if needsValidateNodeKey(node) {
			prog = append(prog, task.TaskTypeValidateNodeKey)
		}
		prog = append(prog,
			task.TaskTypeApplyStatefulSet,
			task.TaskTypeApplyService,
			TaskConfigPatch,
			TaskConfigValidate,
			task.TaskTypeReplacePod,
			task.TaskTypeObserveImage,
			TaskMarkReady,
		)
		return assembleUpdatePlan(node, prog, p2pConfigPatch(node))
	}
	if sidecarNeedsReapproval(node) {
		return buildMarkReadyPlan(node)
	}
	return nil, nil
}
