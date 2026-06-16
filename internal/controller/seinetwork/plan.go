package seinetwork

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

// reconcilePlan drives an active plan to completion or builds a new
// plan if the planner determines one is needed. All status mutations
// are in-memory; the caller flushes via a single status patch.
// Paused short-circuits entirely; an active plan freezes in place.
func (r *SeiNetworkReconciler) reconcilePlan(ctx context.Context, network *seiv1alpha1.SeiNetwork) (ctrl.Result, error) {
	if network.Spec.Paused {
		return ctrl.Result{}, nil
	}

	// Drive active plan.
	if network.Status.Plan != nil && network.Status.Plan.Phase == seiv1alpha1.TaskPlanActive {
		return r.drivePlan(ctx, network)
	}

	// No active plan — ask the planner if one is needed.
	p, err := planner.ForGroup(network)
	if err != nil || p == nil {
		return ctrl.Result{}, nil
	}

	plan, err := p.BuildPlan(network)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building plan: %w", err)
	}

	r.startPlan(ctx, network, plan)
	return planner.ResultRequeueImmediate, nil
}

func (r *SeiNetworkReconciler) drivePlan(ctx context.Context, network *seiv1alpha1.SeiNetwork) (ctrl.Result, error) {
	result, err := r.PlanExecutor.ExecutePlan(ctx, network, network.Status.Plan)
	if err != nil {
		return result, err
	}

	switch network.Status.Plan.Phase {
	case seiv1alpha1.TaskPlanComplete:
		r.completePlan(ctx, network)
	case seiv1alpha1.TaskPlanFailed:
		r.failPlan(ctx, network)
	}

	return result, nil
}

// startPlan stamps the plan onto the network and sets the PlanInProgress condition.
// All mutations are in-memory.
func (r *SeiNetworkReconciler) startPlan(ctx context.Context, network *seiv1alpha1.SeiNetwork, plan *seiv1alpha1.TaskPlan) {
	logger := log.FromContext(ctx)

	network.Status.Plan = plan
	setPlanInProgress(network, "PlanStarted", "Plan execution started")

	r.Recorder.Event(network, corev1.EventTypeNormal, "PlanStarted", "Plan execution started")
	logger.Info("plan started", "tasks", len(plan.Tasks))
}

// completePlan finalizes a completed plan. Every SeiNetwork runs exactly one
// network-level plan — the genesis ceremony — so a completing plan is the
// ceremony completing. All mutations are in-memory.
func (r *SeiNetworkReconciler) completePlan(ctx context.Context, network *seiv1alpha1.SeiNetwork) {
	logger := log.FromContext(ctx)

	setCondition(network, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionTrue,
		"Complete", "genesis ceremony completed")

	network.Status.Plan = nil
	clearPlanInProgress(network, "PlanComplete", "Plan completed successfully")

	r.Recorder.Event(network, corev1.EventTypeNormal, "PlanComplete", "Plan completed successfully")
	logger.Info("plan completed")
}

// failPlan handles a failed plan. All mutations are in-memory.
func (r *SeiNetworkReconciler) failPlan(ctx context.Context, network *seiv1alpha1.SeiNetwork) {
	logger := log.FromContext(ctx)

	network.Status.Phase = seiv1alpha1.GroupPhaseDegraded
	network.Status.Plan = nil
	clearPlanInProgress(network, "PlanFailed", "Plan failed")

	r.Recorder.Event(network, corev1.EventTypeWarning, "PlanFailed", "Plan failed")
	logger.Info("plan failed")
}

func setPlanInProgress(network *seiv1alpha1.SeiNetwork, reason, message string) {
	apimeta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionPlanInProgress,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: network.Generation,
	})
}

func clearPlanInProgress(network *seiv1alpha1.SeiNetwork, reason, message string) {
	apimeta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionPlanInProgress,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: network.Generation,
	})
}
