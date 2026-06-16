package seinetwork

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func (r *SeiNetworkReconciler) updateStatus(ctx context.Context, network *seiv1alpha1.SeiNetwork, statusBase client.Patch) error {
	nodes, err := r.listChildSeiNodes(ctx, network)
	if err != nil {
		return err
	}

	var readyReplicas, upToDateReplicas int32
	nodeStatuses := make([]seiv1alpha1.GroupNodeStatus, 0, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		if node.Status.Phase == seiv1alpha1.PhaseRunning {
			readyReplicas++
		}
		if node.Status.CurrentImage == network.Spec.Image {
			upToDateReplicas++
		}
		nodeStatuses = append(nodeStatuses, seiv1alpha1.GroupNodeStatus{
			Name:         node.Name,
			Phase:        node.Status.Phase,
			CurrentImage: node.Status.CurrentImage,
		})
	}

	// ObservedGeneration tracks "controller has processed this spec" and
	// must advance on every reconcile that runs to completion, including
	// paused ones — generation-drift consumers (kubectl wait, ArgoCD,
	// Flux) depend on it. It defers to completePlan during plan execution.
	if !hasConditionTrue(network, seiv1alpha1.ConditionPlanInProgress) {
		network.Status.ObservedGeneration = network.Generation
	}
	network.Status.Replicas = network.Spec.Replicas
	network.Status.ReadyReplicas = readyReplicas
	network.Status.UpToDateReplicas = upToDateReplicas
	network.Status.Nodes = nodeStatuses
	network.Status.PerPodServices = populatePerPodServices(log.FromContext(ctx), nodes)
	network.Status.Endpoints = composeEndpoints(network)

	network.Status.Phase = computeGroupPhase(network, readyReplicas, network.Spec.Replicas, nodes)

	setNodesReadyCondition(network, readyReplicas, network.Spec.Replicas, nodes)
	setRolloutInProgressCondition(network, upToDateReplicas, network.Spec.Replicas, len(nodes))

	return r.Status().Patch(ctx, network, statusBase)
}

// setRolloutInProgressCondition stamps the DERIVED RolloutInProgress
// projection. True when a child's reported image lags spec.image (mid-roll or
// wedged on a bad tag); False/AllUpToDate at steady state. No plan or revision
// tracking owns this — it is pure computation from the child snapshot. Before
// any children exist there is nothing to roll, so it reads False/AllUpToDate.
func setRolloutInProgressCondition(network *seiv1alpha1.SeiNetwork, upToDate, desired int32, childCount int) {
	if childCount > 0 && upToDate < desired {
		setCondition(network, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
			"ImageRolling", fmt.Sprintf("%d/%d replicas on spec.image", upToDate, desired))
		return
	}
	setCondition(network, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionFalse,
		"AllUpToDate", fmt.Sprintf("%d/%d replicas on spec.image", upToDate, desired))
}

func computeGroupPhase(network *seiv1alpha1.SeiNetwork, ready, desired int32, nodes []seiv1alpha1.SeiNode) seiv1alpha1.SeiNetworkPhase {
	if network.Spec.Paused {
		return seiv1alpha1.GroupPhasePaused
	}
	if hasConditionTrue(network, seiv1alpha1.ConditionPlanInProgress) {
		return seiv1alpha1.GroupPhaseInitializing
	}

	if len(nodes) == 0 {
		return seiv1alpha1.GroupPhasePending
	}
	if ready == desired {
		return seiv1alpha1.GroupPhaseReady
	}

	var failedCount int32
	for i := range nodes {
		if nodes[i].Status.Phase == seiv1alpha1.PhaseFailed {
			failedCount++
		}
	}

	if failedCount > 0 {
		if failedCount == int32(len(nodes)) {
			return seiv1alpha1.GroupPhaseFailed
		}
		if ready > 0 {
			return seiv1alpha1.GroupPhaseDegraded
		}
	}
	return seiv1alpha1.GroupPhaseInitializing
}

func setNodesReadyCondition(network *seiv1alpha1.SeiNetwork, ready, desired int32, nodes []seiv1alpha1.SeiNode) {
	status := metav1.ConditionTrue
	reason := "AllNodesReady"
	message := fmt.Sprintf("%d/%d nodes ready", ready, desired)

	if ready < desired {
		status = metav1.ConditionFalse
		initializing := int32(0)
		failed := int32(0)
		for i := range nodes {
			switch nodes[i].Status.Phase {
			case seiv1alpha1.PhaseFailed:
				failed++
			case seiv1alpha1.PhasePending, seiv1alpha1.PhaseInitializing:
				initializing++
			}
		}
		if failed > 0 {
			reason = "NodesFailed"
			message = fmt.Sprintf("%d/%d nodes ready (%d failed, %d initializing)", ready, desired, failed, initializing)
		} else {
			reason = "NodesInitializing"
			message = fmt.Sprintf("%d/%d nodes ready (%d initializing)", ready, desired, initializing)
		}
	}

	setCondition(network, seiv1alpha1.ConditionNodesReady, status, reason, message)
}

// ReasonNotStarted is the seed reason for always-present lifecycle
// conditions before any transition has occurred. Shared across
// PlanInProgress, RolloutInProgress, and GenesisCeremonyComplete.
const ReasonNotStarted = "NotStarted"

// seedAlwaysPresentConditions stamps the seeded always-present conditions.
// The PlanInProgress seed fires only when absent so transition paths
// (startPlan, completePlan, etc.) own the reason vocabulary once a network
// has lifecycle state. RolloutInProgress is NOT seeded here — it is a derived
// projection that updateStatus computes (and always sets) from the child
// snapshot each reconcile, so it is present after the first reconcile.
func (r *SeiNetworkReconciler) seedAlwaysPresentConditions(network *seiv1alpha1.SeiNetwork) {
	r.setGenesisCeremonyCondition(network)
	r.setPausedCondition(network)
	seedConditionIfAbsent(network, seiv1alpha1.ConditionPlanInProgress,
		ReasonNotStarted, "no plan has run yet")
}

// setPausedCondition mirrors spec.paused and emits an event on each
// Paused↔Unpaused transition.
func (r *SeiNetworkReconciler) setPausedCondition(network *seiv1alpha1.SeiNetwork) {
	prev := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionPaused)
	wasPaused := prev != nil && prev.Status == metav1.ConditionTrue

	if network.Spec.Paused {
		setCondition(network, seiv1alpha1.ConditionPaused, metav1.ConditionTrue,
			"Paused", "spec.paused is true; plan-driven orchestration is frozen")
		// Emit on the False→True transition only. A network created
		// already paused has prev=nil and doesn't need an event — its
		// condition already tells the story.
		if r.Recorder != nil && prev != nil && !wasPaused {
			msg := "operator set spec.paused; controller will not advance plans, rollouts, or template changes until unpaused"
			if network.Status.Plan != nil {
				msg = fmt.Sprintf("operator set spec.paused with active plan %s; plan freezes in place until unpaused", network.Status.Plan.ID)
			}
			r.Recorder.Event(network, corev1.EventTypeNormal, "Paused", msg)
		}
		return
	}
	setCondition(network, seiv1alpha1.ConditionPaused, metav1.ConditionFalse,
		"NotPaused", "spec.paused is unset or false")
	if wasPaused && r.Recorder != nil {
		r.Recorder.Event(network, corev1.EventTypeNormal, "Unpaused",
			"operator cleared spec.paused; controller resumes plan-driven orchestration")
	}
}

// seedConditionIfAbsent writes False/<reason>/<message> only when the
// condition is absent from the network.
func seedConditionIfAbsent(network *seiv1alpha1.SeiNetwork, condType, reason, message string) {
	if apimeta.FindStatusCondition(network.Status.Conditions, condType) != nil {
		return
	}
	setCondition(network, condType, metav1.ConditionFalse, reason, message)
}

func hasConditionTrue(network *seiv1alpha1.SeiNetwork, condType string) bool { //nolint:unparam // general-purpose utility
	c := apimeta.FindStatusCondition(network.Status.Conditions, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}

func setCondition(network *seiv1alpha1.SeiNetwork, condType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&network.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: network.Generation,
	})
}
