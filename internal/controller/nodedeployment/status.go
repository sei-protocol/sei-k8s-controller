package nodedeployment

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logr "sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func (r *SeiNodeDeploymentReconciler) updateStatus(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, statusBase client.Patch) error {
	nodes, err := r.listChildSeiNodes(ctx, group)
	if err != nil {
		return err
	}

	var readyReplicas int32
	nodeStatuses := make([]seiv1alpha1.GroupNodeStatus, 0, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		if node.Status.Phase == seiv1alpha1.PhaseRunning {
			readyReplicas++
		}
		nodeStatuses = append(nodeStatuses, seiv1alpha1.GroupNodeStatus{
			Name:  node.Name,
			Phase: node.Status.Phase,
		})
	}

	// ObservedGeneration tracks "controller has processed this spec" and
	// must advance on every reconcile that runs to completion, including
	// paused ones — generation-drift consumers (kubectl wait, ArgoCD,
	// Flux) depend on it. TemplateHash is what defers template edits, so
	// pause holds only that. Both still defer to completePlan during
	// rollout/plan execution.
	if !hasConditionTrue(group, seiv1alpha1.ConditionPlanInProgress) &&
		!hasConditionTrue(group, seiv1alpha1.ConditionRolloutInProgress) {
		group.Status.ObservedGeneration = group.Generation
		if !group.Spec.Paused {
			group.Status.TemplateHash = templateHash(&group.Spec.Template.Spec)
		}
	}
	group.Status.Replicas = group.Spec.Replicas
	group.Status.ReadyReplicas = readyReplicas
	group.Status.Nodes = nodeStatuses
	group.Status.PerPodServices = populatePerPodServices(logr.FromContext(ctx), nodes)
	group.Status.Endpoints = composeEndpoints(group)

	group.Status.Phase = computeGroupPhase(group, readyReplicas, group.Spec.Replicas, nodes)

	group.Status.NetworkingStatus = r.buildNetworkingStatus(group)

	setNodesReadyCondition(group, readyReplicas, group.Spec.Replicas, nodes)

	return r.Status().Patch(ctx, group, statusBase)
}

func computeGroupPhase(group *seiv1alpha1.SeiNodeDeployment, ready, desired int32, nodes []seiv1alpha1.SeiNode) seiv1alpha1.SeiNodeDeploymentPhase {
	if group.Spec.Paused {
		return seiv1alpha1.GroupPhasePaused
	}
	if hasConditionTrue(group, seiv1alpha1.ConditionRolloutInProgress) {
		return seiv1alpha1.GroupPhaseUpgrading
	}
	if hasConditionTrue(group, seiv1alpha1.ConditionPlanInProgress) {
		if group.Status.Rollout != nil {
			return seiv1alpha1.GroupPhaseUpgrading
		}
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

func (r *SeiNodeDeploymentReconciler) buildNetworkingStatus(group *seiv1alpha1.SeiNodeDeployment) *seiv1alpha1.NetworkingStatus {
	if group.Spec.Networking == nil {
		return nil
	}
	routes := resolveEffectiveRoutes(group, r.GatewayDomain, r.GatewayPublicDomain)
	if len(routes) == 0 {
		return nil
	}
	var rs []seiv1alpha1.RouteStatus
	for _, er := range routes {
		for _, h := range er.Hostnames {
			rs = append(rs, seiv1alpha1.RouteStatus{
				Hostname: h,
				Protocol: er.Protocol,
			})
		}
	}
	return &seiv1alpha1.NetworkingStatus{Routes: rs}
}

func setNodesReadyCondition(group *seiv1alpha1.SeiNodeDeployment, ready, desired int32, nodes []seiv1alpha1.SeiNode) {
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

	setCondition(group, seiv1alpha1.ConditionNodesReady, status, reason, message)
}

// seedAlwaysPresentConditions stamps every always-present condition.
// The InProgress seeds fire only when absent so transition paths
// (startPlan, completePlan, etc.) own the reason vocabulary once an
// SND has lifecycle state.
func (r *SeiNodeDeploymentReconciler) seedAlwaysPresentConditions(group *seiv1alpha1.SeiNodeDeployment) {
	r.setGenesisCeremonyCondition(group)
	r.setPausedCondition(group)
	seedConditionIfAbsent(group, seiv1alpha1.ConditionPlanInProgress,
		"NotStarted", "no plan has run yet")
	seedConditionIfAbsent(group, seiv1alpha1.ConditionRolloutInProgress,
		"NotStarted", "no rollout has run yet")
}

// setPausedCondition mirrors spec.paused and emits an event on each
// Paused↔Unpaused transition.
func (r *SeiNodeDeploymentReconciler) setPausedCondition(group *seiv1alpha1.SeiNodeDeployment) {
	prev := apimeta.FindStatusCondition(group.Status.Conditions, seiv1alpha1.ConditionPaused)
	wasPaused := prev != nil && prev.Status == metav1.ConditionTrue

	if group.Spec.Paused {
		setCondition(group, seiv1alpha1.ConditionPaused, metav1.ConditionTrue,
			"Paused", "spec.paused is true; plan-driven orchestration is frozen")
		// Emit on the False→True transition only. An SND created
		// already paused has prev=nil and doesn't need an event — its
		// condition already tells the story.
		if r.Recorder != nil && prev != nil && !wasPaused {
			msg := "operator set spec.paused; controller will not advance plans, rollouts, or template changes until unpaused"
			if group.Status.Plan != nil {
				msg = fmt.Sprintf("operator set spec.paused with active plan %s; plan freezes in place until unpaused", group.Status.Plan.ID)
			}
			r.Recorder.Event(group, corev1.EventTypeNormal, "Paused", msg)
		}
		return
	}
	setCondition(group, seiv1alpha1.ConditionPaused, metav1.ConditionFalse,
		"NotPaused", "spec.paused is unset or false")
	if wasPaused && r.Recorder != nil {
		r.Recorder.Event(group, corev1.EventTypeNormal, "Unpaused",
			"operator cleared spec.paused; controller resumes plan-driven orchestration")
	}
}

// seedConditionIfAbsent writes False/<reason>/<message> only when the
// condition is absent from the SND.
func seedConditionIfAbsent(group *seiv1alpha1.SeiNodeDeployment, condType, reason, message string) {
	if apimeta.FindStatusCondition(group.Status.Conditions, condType) != nil {
		return
	}
	setCondition(group, condType, metav1.ConditionFalse, reason, message)
}

func hasConditionTrue(group *seiv1alpha1.SeiNodeDeployment, condType string) bool { //nolint:unparam // general-purpose utility
	c := apimeta.FindStatusCondition(group.Status.Conditions, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}

func hasConditionReason(group *seiv1alpha1.SeiNodeDeployment, condType, reason string) bool {
	c := apimeta.FindStatusCondition(group.Status.Conditions, condType)
	return c != nil && c.Reason == reason
}

func removeCondition(group *seiv1alpha1.SeiNodeDeployment, condType string) {
	apimeta.RemoveStatusCondition(&group.Status.Conditions, condType)
}

func setCondition(group *seiv1alpha1.SeiNodeDeployment, condType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: group.Generation,
	})
}
