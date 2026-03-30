package nodegroup

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

// reconcilePlan is the single entry point for plan-driven orchestration.
// It drives an active plan to completion, or detects conditions that
// warrant building a new plan.
func (r *SeiNodeGroupReconciler) reconcilePlan(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, statusBase client.Patch) (ctrl.Result, error) {
	// Drive active init plan (genesis assembly).
	if group.Status.InitPlan != nil && group.Status.InitPlan.Phase == seiv1alpha1.TaskPlanActive {
		return r.driveInitPlan(ctx, group, statusBase)
	}

	// Drive active deployment plan.
	if planner.IsDeploymentActive(group) {
		return r.driveDeploymentPlan(ctx, group, statusBase)
	}

	// No active plan — check if one needs to be created.
	if needsGenesisPlan(group) {
		return r.startGenesisPlan(ctx, group, statusBase)
	}

	if planner.NeedsDeployment(group) {
		return r.startDeploymentPlan(ctx, group, statusBase)
	}

	return ctrl.Result{}, nil
}

func needsGenesisPlan(group *seiv1alpha1.SeiNodeGroup) bool {
	if group.Spec.Genesis == nil {
		return false
	}
	if group.Status.InitPlan != nil {
		return false
	}
	if hasConditionTrue(group, seiv1alpha1.ConditionGenesisCeremonyComplete) {
		return false
	}
	return int32(len(group.Status.IncumbentNodes)) >= group.Spec.Replicas
}

func (r *SeiNodeGroupReconciler) driveInitPlan(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, statusBase client.Patch) (ctrl.Result, error) {
	result, err := r.PlanExecutor.ExecutePlan(ctx, group, group.Status.InitPlan)
	if err != nil {
		return result, err
	}

	switch group.Status.InitPlan.Phase {
	case seiv1alpha1.TaskPlanComplete:
		return r.completePlan(ctx, group, statusBase, "InitPlanComplete", "Initialization plan completed")
	case seiv1alpha1.TaskPlanFailed:
		return r.failPlan(ctx, group, statusBase, "InitPlanFailed", "Initialization plan failed")
	}

	return result, nil
}

func (r *SeiNodeGroupReconciler) driveDeploymentPlan(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, statusBase client.Patch) (ctrl.Result, error) {
	deployment := group.Status.Deployment
	result, err := r.PlanExecutor.ExecutePlan(ctx, group, &deployment.Plan)
	if err != nil {
		return result, err
	}

	switch deployment.Plan.Phase {
	case seiv1alpha1.TaskPlanComplete:
		group.Status.ObservedGeneration = group.Generation
		if err := r.reconcileNetworking(ctx, group); err != nil {
			log.FromContext(ctx).Error(err, "reconciling networking after deployment")
		}
		return r.completePlan(ctx, group, statusBase, "DeploymentComplete",
			fmt.Sprintf("Deployment to revision %s completed", deployment.EntrantRevision))
	case seiv1alpha1.TaskPlanFailed:
		return r.failPlan(ctx, group, statusBase, "DeploymentFailed",
			fmt.Sprintf("Deployment to revision %s failed", deployment.EntrantRevision))
	}

	return result, nil
}

func (r *SeiNodeGroupReconciler) startGenesisPlan(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, statusBase client.Patch) (ctrl.Result, error) {
	p, err := planner.ForGroup(group)
	if err != nil {
		return ctrl.Result{}, err
	}
	plan, err := p.BuildPlan(group, nil)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building genesis plan: %w", err)
	}

	group.Status.InitPlan = plan
	setPlanInProgress(group, "GenesisAssembly", "Genesis assembly plan started")

	if err := r.Status().Patch(ctx, group, statusBase); err != nil {
		return ctrl.Result{}, fmt.Errorf("writing genesis plan: %w", err)
	}

	r.Recorder.Event(group, corev1.EventTypeNormal, "GenesisAssemblyStarted", "Starting genesis assembly plan")
	return planner.ResultRequeueImmediate, nil
}

func (r *SeiNodeGroupReconciler) startDeploymentPlan(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, statusBase client.Patch) (ctrl.Result, error) {
	dp, err := planner.ForDeployment(group)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving deployment planner: %w", err)
	}
	plan, err := dp.BuildPlan(group, nil)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building deployment plan: %w", err)
	}

	entrantRevision := planner.EntrantRevision(group)
	incumbentRevision := planner.IncumbentRevision(group)

	group.Status.Deployment = &seiv1alpha1.DeploymentStatus{
		Plan:              *plan,
		IncumbentRevision: incumbentRevision,
		EntrantRevision:   entrantRevision,
		EntrantNodes:      planner.EntrantNodeNames(group),
	}
	group.Status.Phase = seiv1alpha1.GroupPhaseUpgrading
	setPlanInProgress(group, "Deployment", fmt.Sprintf("Deployment to revision %s started", entrantRevision))

	if err := r.Status().Patch(ctx, group, statusBase); err != nil {
		return ctrl.Result{}, fmt.Errorf("writing deployment plan: %w", err)
	}

	r.Recorder.Eventf(group, corev1.EventTypeNormal, "DeploymentStarted",
		"Deployment started: %s → %s", incumbentRevision, entrantRevision)
	return planner.ResultRequeueImmediate, nil
}

func (r *SeiNodeGroupReconciler) completePlan(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, statusBase client.Patch, reason, message string) (ctrl.Result, error) {
	clearPlanInProgress(group, reason, message)

	r.Recorder.Event(group, corev1.EventTypeNormal, reason, message)
	log.FromContext(ctx).Info("plan completed", "reason", reason)

	if err := r.updateStatus(ctx, group, statusBase); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after plan completion: %w", err)
	}
	return planner.ResultRequeueImmediate, nil
}

func (r *SeiNodeGroupReconciler) failPlan(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, statusBase client.Patch, reason, message string) (ctrl.Result, error) {
	group.Status.Phase = seiv1alpha1.GroupPhaseDegraded
	clearPlanInProgress(group, reason, message)

	r.Recorder.Event(group, corev1.EventTypeWarning, reason, message)
	log.FromContext(ctx).Info("plan failed", "reason", reason)

	if err := r.updateStatus(ctx, group, statusBase); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after plan failure: %w", err)
	}
	return ctrl.Result{}, nil
}

func setPlanInProgress(group *seiv1alpha1.SeiNodeGroup, reason, message string) {
	apimeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionPlanInProgress,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: group.Generation,
	})
}

func clearPlanInProgress(group *seiv1alpha1.SeiNodeGroup, reason, message string) {
	apimeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionPlanInProgress,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: group.Generation,
	})
}
