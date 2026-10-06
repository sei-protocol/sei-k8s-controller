package planner

import (
	"slices"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// mountedConfigWriters are the sidecar tasks that write config.toml or
// app.toml. None of them may run on a pod that mounts those files from the
// operator's ConfigMaps.
//
// config-apply, config-patch, configure-state-sync and
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
	TaskConfigPatch,
	TaskConfigureStateSync,
	TaskSetGenesisPeers,
	TaskGenerateIdentity,
}

// MountsNodeConfig reports whether this node's pod carries the ConfigMap
// mounts. spec.nodeConfig is fixed at creation, so the spec alone answers it.
func MountsNodeConfig(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.NodeConfig != nil
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
// confidently wrong is worse than no verdict. The files are the operator's.
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
func mountedConfigWriterInPlan(node *seiv1alpha1.SeiNode, plan *seiv1alpha1.TaskPlan) string {
	if !MountsNodeConfig(node) || plan == nil {
		return ""
	}
	for _, t := range plan.Tasks {
		if slices.Contains(mountedConfigWriters, t.Type) {
			return t.Type
		}
	}
	return ""
}

// staticConfigPlanner plans a node whose config.toml and app.toml come from
// operator-supplied ConfigMaps.
//
// It wraps the node's mode planner rather than replacing it, so the mode keeps
// its own Validate and its own init plan, which withoutManagedConfigTasks
// strips for it. The node shapes whose configuration seid only learns at run
// time are refused by the CRD. What the wrapper owns is the Running arm, which
// the mode planners route through assembleUpdatePlan — an assembler that
// force-inserts config-apply whenever the configValues baseline is unobserved,
// which on one of these nodes is always.
type staticConfigPlanner struct {
	base     NodePlanner
	platform platform.Config
}

func (p *staticConfigPlanner) Mode() string { return p.base.Mode() }

func (p *staticConfigPlanner) Validate(node *seiv1alpha1.SeiNode) error {
	return p.base.Validate(node)
}

// BuildPlan delegates every arm but Running to the mode planner. A node created
// with a maintenance hold initializes but does not start: the init plan ends
// with the hold in effect instead of mark-ready (spec 010 Req 2.9).
func (p *staticConfigPlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		return p.buildRunningPlan(node)
	}
	plan, err := p.base.BuildPlan(node)
	if err != nil || plan == nil || node.Spec.HoldRequested() == "" {
		return plan, err
	}
	if err := parkInsteadOfRelease(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// buildRunningPlan returns the next plan for a Running node, or nil if none is
// needed. The order is the safety order:
//
//  1. a pending data reset (spec 009), whose plan also waits for any rollout,
//     so it serves a reset commit that changes the template too;
//  2. a change to the maintenance hold (spec 010);
//  3. pod-template drift;
//  4. a readiness reapproval, never while a hold is requested.
//
// While a hold is requested no plan releases seid: the reset plan parks
// instead, and the update plan carries no mark-ready. There is no configValues
// arm: the CRD rejects configValues alongside nodeConfig.
func (p *staticConfigPlanner) buildRunningPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	held := node.Spec.HoldRequested() != ""
	if task.DataResetPending(node) {
		plan, err := buildDataResetPlan(node)
		if err != nil {
			return nil, err
		}
		if held {
			if err := parkInsteadOfRelease(plan); err != nil {
				return nil, err
			}
		}
		markDataResetStarted(node)
		return plan, nil
	}
	if plan, err := buildHoldPlan(node, node.Spec.HoldRequested(), node.Status.MaintenanceHold); err != nil || plan != nil {
		return plan, err
	}
	if podTemplateDrifted(node, p.platform) {
		plan, err := p.buildUpdatePlan(node)
		if err != nil {
			return nil, err
		}
		if held {
			withoutMarkReady(plan)
		}
		setNodeUpdateCondition(node, metav1.ConditionTrue, "UpdateStarted", podTemplateDriftMessage(node, p.platform))
		return plan, nil
	}
	if sidecarNeedsReapproval(node) && !held {
		return buildMarkReadyPlan(node)
	}
	return nil, nil
}

// buildUpdatePlan follows the roll the StatefulSet controller performs. The
// StatefulSet is RollingUpdate for these nodes, so the template change
// apply-statefulset writes replaces the pod by itself; there is no
// replace-pod. observe-image stamps the rolled images once the rollout lands,
// and mark-ready opens the new sidecar's gate. The key-validation gates lead,
// as they do in every mode's update plan, so a missing Secret fails
// controller-side rather than as a kubelet mount error on the recreated pod.
func (p *staticConfigPlanner) buildUpdatePlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	prog := make([]string, 0, 7)
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
		task.TaskTypeObserveImage,
		TaskMarkReady,
	)
	return assembleStaticUpdatePlan(node, prog)
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
