package seinetwork

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
		if validatorLost(network) {
			if err := r.abandonPlanForLostValidator(ctx, network); err != nil {
				return ctrl.Result{}, err
			}
			return planner.ResultRequeueImmediate, nil
		}
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

// failPlan handles a failed plan. Every SeiNetwork runs exactly one
// network-level plan — the genesis ceremony — so a failing plan is the
// ceremony failing. The failure is recorded on GenesisCeremonyComplete
// (False/CeremonyFailed), a stable condition that survives the per-reconcile
// phase recomputation in computeGroupPhase; the phase itself is derived from
// that condition (GroupPhaseFailed) rather than written here, since
// updateStatus recomputes Status.Phase later in the same reconcile. All
// mutations are in-memory.
func (r *SeiNetworkReconciler) failPlan(ctx context.Context, network *seiv1alpha1.SeiNetwork) {
	logger := log.FromContext(ctx)

	setCondition(network, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionFalse,
		"CeremonyFailed", "genesis ceremony plan failed")

	network.Status.Plan = nil
	clearPlanInProgress(network, "PlanFailed", "Plan failed")

	r.Recorder.Event(network, corev1.EventTypeWarning, "PlanFailed", "Plan failed")
	logger.Info("plan failed")
}

// validatorLost reports whether a child of the active ceremony plan no longer
// exists. Creates are gated while PlanInProgress=True and replicas are fixed
// once the ceremony starts, so fewer incumbents than replicas under an active
// plan means a founding validator was deleted mid-ceremony.
func validatorLost(network *seiv1alpha1.SeiNetwork) bool {
	return int32(len(network.Status.IncumbentNodes)) < network.Spec.Replicas
}

// abandonPlanForLostValidator drops the active ceremony plan so the gate
// reopens and the missing child is recreated. The ceremony's tasks address the
// founding set by name and would otherwise retry forever against a node the
// gate never lets come back.
//
// When every survivor is a child this network minted, the survivors are
// deleted too so the whole set is recreated and the ceremony rebuilt over it.
// Recreating only the lost node is not enough: its replacement carries a fresh
// identity and gentx, so the reassembled genesis differs from the one the
// survivors already fetched — assemble-genesis and configure-genesis are
// marker-guarded on the sidecar's data PVC and never redo their work — and
// the set would split across two genesis hashes. Deleting a child makes its
// SeiNode finalizer remove that PVC, markers included, and a set the ceremony
// has not finished minting holds no chain state worth keeping.
//
// A survivor that predates the network was adopted, not minted: a Retain
// teardown released it with its consensus identity and chain data, and the
// recreated network runs a ceremony over it that the markers turn into a
// no-op. That identity cannot be regenerated, so an adopted set is never torn
// down — the plan is abandoned, the loss is surfaced, and the lost node is
// recreated to join the existing chain. Status mutations are in-memory; child
// deletes go to the API server.
func (r *SeiNetworkReconciler) abandonPlanForLostValidator(ctx context.Context, network *seiv1alpha1.SeiNetwork) error {
	survivors, err := r.listChildSeiNodes(ctx, network)
	if err != nil {
		return err
	}

	minted := true
	for i := range survivors {
		if survivors[i].CreationTimestamp.Before(&network.CreationTimestamp) {
			minted = false
			break
		}
	}

	var msg string
	deleted := 0
	if minted {
		for i := range survivors {
			node := &survivors[i]
			if !node.DeletionTimestamp.IsZero() {
				continue
			}
			if err := r.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting founding SeiNode %s after validator loss: %w", node.Name, err)
			}
			deleted++
			r.Recorder.Eventf(network, corev1.EventTypeWarning, ReasonFoundingSetTornDown,
				"Deleted founding SeiNode %s: the genesis ceremony (plan %s) restarts over a recreated set",
				node.Name, network.Status.Plan.ID)
		}
		msg = fmt.Sprintf("%d of %d founding validators present; plan %s abandoned, the set is torn down and the genesis ceremony restarts once it is recreated",
			len(network.Status.IncumbentNodes), network.Spec.Replicas, network.Status.Plan.ID)
	} else {
		msg = fmt.Sprintf("%d of %d validators present; plan %s abandoned, adopted validators are kept and the missing node is recreated",
			len(network.Status.IncumbentNodes), network.Spec.Replicas, network.Status.Plan.ID)
	}

	setCondition(network, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionFalse,
		ReasonValidatorLost, msg)

	network.Status.Plan = nil
	clearPlanInProgress(network, ReasonValidatorLost, "Plan abandoned: a founding validator was deleted during the genesis ceremony")

	r.Recorder.Event(network, corev1.EventTypeWarning, ReasonValidatorLost, msg)
	log.FromContext(ctx).Info("plan abandoned: validator lost during genesis ceremony",
		"incumbents", len(network.Status.IncumbentNodes), "replicas", network.Spec.Replicas,
		"adoptedSet", !minted, "survivorsDeleted", deleted)
	return nil
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
