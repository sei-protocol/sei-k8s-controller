package nodedeployment

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

// reconcilePlan is the single entry point for plan-driven orchestration.
// It drives an active plan to completion, or builds a new plan if the
// planner determines one is needed. All status mutations are in-memory;
// the caller flushes via a single status patch.
func (r *SeiNodeDeploymentReconciler) reconcilePlan(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) (ctrl.Result, error) {
	// Drive active plan.
	if group.Status.Plan != nil && group.Status.Plan.Phase == seiv1alpha1.TaskPlanActive {
		return r.drivePlan(ctx, group)
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

	r.startPlan(ctx, group, plan)
	return planner.ResultRequeueImmediate, nil
}

func (r *SeiNodeDeploymentReconciler) drivePlan(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) (ctrl.Result, error) {
	result, err := r.PlanExecutor.ExecutePlan(ctx, group, group.Status.Plan)
	if err != nil {
		return result, err
	}

	switch group.Status.Plan.Phase {
	case seiv1alpha1.TaskPlanComplete:
		r.completePlan(ctx, group)
	case seiv1alpha1.TaskPlanFailed:
		r.failPlan(ctx, group)
	}

	return result, nil
}

// startPlan stamps the plan onto the group and sets the PlanInProgress condition.
// All mutations are in-memory.
func (r *SeiNodeDeploymentReconciler) startPlan(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, plan *seiv1alpha1.TaskPlan) {
	logger := log.FromContext(ctx)

	group.Status.Plan = plan
	setPlanInProgress(group, "PlanStarted", "Plan execution started")

	r.Recorder.Event(group, corev1.EventTypeNormal, "PlanStarted", "Plan execution started")
	logger.Info("plan started", "tasks", len(plan.Tasks))
}

// completePlan finalizes a completed plan. All mutations are in-memory.
func (r *SeiNodeDeploymentReconciler) completePlan(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) {
	logger := log.FromContext(ctx)

	isDeploymentPlan := group.Status.Rollout != nil

	if isDeploymentPlan {
		group.Status.ObservedGeneration = group.Generation
		if err := r.reconcileNetworking(ctx, group); err != nil {
			logger.Error(err, "reconciling networking after deployment")
		}
		group.Status.Rollout = nil
		setCondition(group, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionFalse,
			"RolloutComplete", "Deployment completed successfully")
		r.Recorder.Event(group, corev1.EventTypeNormal, "RolloutComplete", "Deployment rollout completed successfully")
	}

	if group.Spec.Genesis != nil && !isDeploymentPlan {
		setCondition(group, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionTrue,
			"CeremonyComplete", "Genesis ceremony completed")
	}

	group.Status.Plan = nil
	clearPlanInProgress(group, "PlanComplete", "Plan completed successfully")

	r.Recorder.Event(group, corev1.EventTypeNormal, "PlanComplete", "Plan completed successfully")
	logger.Info("plan completed")
}

// failPlan handles a failed plan. All mutations are in-memory.
func (r *SeiNodeDeploymentReconciler) failPlan(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) {
	logger := log.FromContext(ctx)

	group.Status.Phase = seiv1alpha1.GroupPhaseDegraded
	group.Status.Plan = nil
	if group.Status.Rollout != nil {
		group.Status.Rollout = nil
		setCondition(group, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionFalse,
			"RolloutFailed", "Deployment plan failed")
	}
	clearPlanInProgress(group, "PlanFailed", "Plan failed")

	r.Recorder.Event(group, corev1.EventTypeWarning, "PlanFailed", "Plan failed")
	logger.Info("plan failed")
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
