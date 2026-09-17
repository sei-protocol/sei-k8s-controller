package planner

import (
	"fmt"
	"slices"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

// mountedConfigWriters are the sidecar tasks that write config.toml or
// app.toml. None of them may run on a pod that mounts those files from the
// operator's ConfigMaps.
//
// config-apply, config-reload, config-patch, configure-state-sync and
// set-genesis-peers all commit with os.Rename. A rename onto a mounted path
// from another container succeeds and detaches the mount, after which seid
// reads the writer's file instead of the operator's. generate-identity
// truncates rather than renames, which leaves the mount in place but fails
// against a read-only one.
//
// The list is maintained by hand against the sidecar's task handlers; nothing
// checks it automatically, because the sidecar is a separate module.
var mountedConfigWriters = []string{
	TaskConfigApply,
	client.TaskTypeConfigReload,
	TaskConfigPatch,
	TaskConfigureStateSync,
	TaskSetGenesisPeers,
	TaskGenerateIdentity,
}

// MountsNodeConfig reports whether this node's pod carries the ConfigMap
// mounts, or is about to. The spec describes the pod the controller wants and
// the stamp describes the pod that exists; a task that renames a mounted file
// is a hazard under either, so every refusal keys on the union.
//
// Reverting a node to controller-managed config is the case that needs the
// stamp: the spec no longer names a ConfigMap while the live pod still mounts
// one, and the mode planner's own update plan submits config-patch before it
// replaces the pod.
func MountsNodeConfig(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.NodeConfig != nil || node.Status.CurrentNodeConfig != nil
}

// withoutManagedConfigTasks removes the config tasks from a progression built
// for a node whose config.toml and app.toml the controller does not own. It
// runs where the progression is assembled; mountedConfigWriterInPlan checks
// the finished plan whichever builder produced it.
//
// The writers go because they detach the mount. config-validate goes because
// it reports on a file the operator wrote, through sei-config's legacy reader,
// which falls back to mode "full" when app.toml carries no [sei] mode — so on
// a validator it passes a config seid will refuse. A verdict that can be
// confidently wrong is worse than no verdict. replace-pod parses both files
// before it deletes anything, and that check is the one that matters.
func withoutManagedConfigTasks(node *seiv1alpha1.SeiNode, prog []string) []string {
	if !MountsNodeConfig(node) {
		return prog
	}
	return slices.DeleteFunc(slices.Clone(prog), func(taskType string) bool {
		return taskType == TaskConfigValidate || slices.Contains(mountedConfigWriters, taskType)
	})
}

// mountedConfigWriterInPlan returns the first task in the plan that would
// write config.toml or app.toml on a pod that mounts them, or "" when the plan
// is safe. ResolvePlan refuses a plan it names, so the invariant is guarded
// once no matter which builder produced the plan.
//
// A revert plan is the one case where a writer belongs: it replaces the pod
// with one the template no longer gives the mounts, so everything after that
// replace-pod runs against a plain file. Nothing else is exempt. A bootstrap
// Job pod carries no mount of its own, but it holds the same PVC as the
// production pod, and a rename from there detaches the production pod's mount
// just as silently; staticConfigPlanner.Validate refuses that combination.
func mountedConfigWriterInPlan(node *seiv1alpha1.SeiNode, plan *seiv1alpha1.TaskPlan) string {
	if !MountsNodeConfig(node) || plan == nil {
		return ""
	}
	lastMountedTask := len(plan.Tasks)
	if revertingNodeConfig(node) {
		for i, t := range plan.Tasks {
			if t.Type == task.TaskTypeReplacePod {
				lastMountedTask = i
				break
			}
		}
	}
	for _, t := range plan.Tasks[:lastMountedTask] {
		if slices.Contains(mountedConfigWriters, t.Type) {
			return t.Type
		}
	}
	return ""
}

// revertingNodeConfig reports whether the operator has taken the ConfigMaps
// away from a node whose pod still mounts them.
func revertingNodeConfig(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.NodeConfig == nil && node.Status.CurrentNodeConfig != nil
}

// staticConfigPlanner plans a node whose config.toml and app.toml come from
// operator-supplied ConfigMaps.
//
// It wraps the node's mode planner rather than replacing it, so the mode keeps
// its own Validate and its own init plan, which withoutManagedConfigTasks
// strips for it. The bootstrap and genesis-ceremony progressions are not
// filtered at all — Validate refuses both shapes, and mountedConfigWriterInPlan
// refuses any plan that slips through. What the wrapper owns is the Running arm, which
// the mode planners route through assembleUpdatePlan — an assembler that
// force-inserts config-apply whenever the configValues baseline is unobserved,
// which on one of these nodes is always.
type staticConfigPlanner struct {
	base     NodePlanner
	platform platform.Config
}

func (p *staticConfigPlanner) Mode() string { return p.base.Mode() }

// Validate runs the mode's own checks first, then refuses the node shapes
// whose configuration seid can only learn at run time. Each of them reaches
// config.toml through a task this planner removes, so the ConfigMap would
// silently win and the node would start on configuration nobody intended.
func (p *staticConfigPlanner) Validate(node *seiv1alpha1.SeiNode) error {
	if err := p.base.Validate(node); err != nil {
		return err
	}
	if isGenesisCeremonyNode(node) {
		return fmt.Errorf("nodeConfig is not supported on a genesis-ceremony validator: " +
			"the founding validator set is assembled during the ceremony and written by set-genesis-peers, " +
			"so it cannot be in a ConfigMap written beforehand")
	}
	if NeedsBootstrap(node) {
		return fmt.Errorf("nodeConfig is not supported with a bootstrap Job: " +
			"the Job pod holds the same data volume as the production pod and rewrites config.toml there, " +
			"which detaches the production pod's mount and leaves seid reading the Job's file")
	}
	if snap := node.Spec.SnapshotSource(); snap != nil && snap.StateSync != nil {
		return fmt.Errorf("nodeConfig is not supported with a state-sync snapshot source: " +
			"configure-state-sync discovers the trust height and hash from live witnesses at run time, " +
			"so they cannot be in a ConfigMap written beforehand")
	}
	if node.Spec.Consensus.IsAutobahn() {
		return fmt.Errorf("nodeConfig is not supported under consensus engine Autobahn: " +
			"the engine's config.toml keys are controller-derived and reach the node through the overlay this planner removes")
	}
	return nil
}

// BuildPlan delegates every arm but Running to the mode planner.
func (p *staticConfigPlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		return p.buildRunningPlan(node)
	}
	return p.base.BuildPlan(node)
}

// buildRunningPlan returns the update plan for a Running node, or nil if no
// drift. There is no configValues arm: the CRD rejects configValues alongside
// nodeConfig, so pod replacement is the only config-delivery mechanism here.
func (p *staticConfigPlanner) buildRunningPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if podTemplateDrifted(node, p.platform) {
		plan, err := p.buildUpdatePlan(node)
		if err != nil {
			return nil, err
		}
		setNodeUpdateCondition(node, metav1.ConditionTrue, "UpdateStarted", podTemplateDriftMessage(node, p.platform))
		return plan, nil
	}
	if sidecarNeedsReapproval(node) {
		return buildMarkReadyPlan(node)
	}
	return nil, nil
}

// buildUpdatePlan rolls the pod. Kubelet pins a subPath mount at pod start, so
// replacing the pod is what delivers new config. A revert also restores the
// controller-managed base afterwards; see below. The
// key-validation gates lead, as they do in every mode's update plan, so a
// missing Secret fails controller-side rather than as a kubelet mount error on
// the recreated pod.
func (p *staticConfigPlanner) buildUpdatePlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	prog := make([]string, 0, 9)
	if needsValidateSigningKey(node) {
		prog = append(prog, task.TaskTypeValidateSigningKey)
	}
	if needsValidateNodeKey(node) {
		prog = append(prog, task.TaskTypeValidateNodeKey)
	}
	if needsValidateOperatorKeyring(node) {
		prog = append(prog, task.TaskTypeValidateOperatorKeyring)
	}
	prog = append(prog,
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveImage,
	)
	if !revertingNodeConfig(node) {
		prog = append(prog, TaskMarkReady)
		return assembleStaticUpdatePlan(node, prog)
	}

	// The replacement pod has no mount, so the controller writes the base
	// configuration it never wrote while the ConfigMaps were in place. A node
	// created with nodeConfig has only what `seid init` left on the volume: no
	// mode base, no freeze height, no snapshot-generation keys. Without this
	// the node keeps those defaults and reports success.
	//
	// seid has not started yet. Its container blocks on the sidecar's
	// /v0/healthz, which reports ready only after mark-ready, so the write
	// lands before seid reads the file and no restart is needed.
	prog = append(prog, TaskConfigApply, TaskConfigValidate, TaskMarkReady)
	plan, err := assembleStaticUpdatePlan(node, prog)
	if err != nil {
		return nil, err
	}
	plan.ClearsNodeConfig = true
	// The node is back on the controller-managed path, so it takes the overlay
	// and the observed baseline with it. withConfigValues splices the patch
	// before config-validate, which this progression carries.
	return withConfigValues(plan, node)
}

// assembleStaticUpdatePlan composes the progression into a TaskPlan. It is the
// sibling of assembleUpdatePlan for nodes that carry no configValues: no
// config-apply insertion, no overlay splice, and so no ConfigValuesHash to
// stamp. FailedPhase stays empty so a failure retries on the next reconcile.
func assembleStaticUpdatePlan(node *seiv1alpha1.SeiNode, prog []string) (_ *seiv1alpha1.TaskPlan, retErr error) {
	defer func() {
		if retErr != nil {
			setNodeUpdateCondition(node, metav1.ConditionFalse, reasonUpdatePlanBuildFailed, retErr.Error())
		}
	}()

	planID := uuid.New().String()
	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, taskType := range prog {
		t, err := buildPlannedTask(planID, taskType, i, paramsForUpdateTask(node, taskType, nil))
		if err != nil {
			return nil, err
		}
		tasks[i] = t
	}
	return &seiv1alpha1.TaskPlan{
		ID:          planID,
		Phase:       seiv1alpha1.TaskPlanActive,
		Tasks:       tasks,
		TargetPhase: seiv1alpha1.PhaseRunning,
	}, nil
}
