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
// It drives an active plan to completion, or builds a new plan if the
// planner determines one is needed.
func (r *SeiNodeDeploymentReconciler) reconcilePlan(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, statusBase client.Patch) (ctrl.Result, error) {
	// Drive active plan.
	if group.Status.Plan != nil && group.Status.Plan.Phase == seiv1alpha1.TaskPlanActive {
		return r.drivePlan(ctx, group, statusBase)
	}

	// No active plan — ask the planner if one is needed.
	p, err := planner.ForGroup(group)
	if err != nil || p == nil {
		return ctrl.Result{}, nil
	}

	plan, err := p.BuildPlan(group)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building plan: %w", err)
	}

	return r.startPlan(ctx, group, statusBase, plan)
}

func (r *SeiNodeDeploymentReconciler) drivePlan(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, statusBase client.Patch) (ctrl.Result, error) {
	result, err := r.PlanExecutor.ExecutePlan(ctx, group, group.Status.Plan)
	if err != nil {
		return result, err
	}

	switch group.Status.Plan.Phase {
	case seiv1alpha1.TaskPlanComplete:
		return r.completePlan(ctx, group, statusBase)
	case seiv1alpha1.TaskPlanFailed:
		return r.failPlan(ctx, group, statusBase)
	}

	return result, nil
}

func (r *SeiNodeDeploymentReconciler) startPlan(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, statusBase client.Patch, plan *seiv1alpha1.TaskPlan) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	group.Status.Plan = plan
	setPlanInProgress(group, "PlanStarted", "Plan execution started")

	if err := r.Status().Patch(ctx, group, statusBase); err != nil {
		return ctrl.Result{}, fmt.Errorf("writing plan status: %w", err)
	}

	r.Recorder.Event(group, corev1.EventTypeNormal, "PlanStarted", "Plan execution started")
	logger.Info("plan started", "tasks", len(plan.Tasks))

	return planner.ResultRequeueImmediate, nil
}

func (r *SeiNodeDeploymentReconciler) completePlan(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, statusBase client.Patch) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	isDeploymentPlan := group.Status.Deployment != nil

	if isDeploymentPlan {
		group.Status.ObservedGeneration = group.Generation
		if err := r.reconcileNetworking(ctx, group); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling networking after deployment: %w", err)
		}
		group.Status.Deployment = nil
	}

	if group.Spec.Genesis != nil && !isDeploymentPlan {
		setCondition(group, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionTrue,
			"CeremonyComplete", "Genesis ceremony completed")
	}

	group.Status.Plan = nil
	clearPlanInProgress(group, "PlanComplete", "Plan completed successfully")

	r.Recorder.Event(group, corev1.EventTypeNormal, "PlanComplete", "Plan completed successfully")
	logger.Info("plan completed")

	if err := r.updateStatus(ctx, group, statusBase); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after plan completion: %w", err)
	}
	return planner.ResultRequeueImmediate, nil
}

func (r *SeiNodeDeploymentReconciler) failPlan(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, statusBase client.Patch) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	group.Status.Phase = seiv1alpha1.GroupPhaseDegraded
	group.Status.Plan = nil
	group.Status.Deployment = nil
	clearPlanInProgress(group, "PlanFailed", "Plan failed")

	r.Recorder.Event(group, corev1.EventTypeWarning, "PlanFailed", "Plan failed")
	logger.Info("plan failed")

	if err := r.updateStatus(ctx, group, statusBase); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after plan failure: %w", err)
	}
	return ctrl.Result{}, nil
}

func setPlanInProgress(group *seiv1alpha1.SeiNodeDeployment, reason, message string) {
	apimeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionPlanInProgress,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: group.Generation,
	})
}

func clearPlanInProgress(group *seiv1alpha1.SeiNodeDeployment, reason, message string) {
	apimeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionPlanInProgress,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: group.Generation,
	})
}
