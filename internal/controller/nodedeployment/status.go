package nodedeployment

import (
	"context"
	"fmt"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

	// Update ObservedGeneration and TemplateHash when no rollout or plan is active.
	// During rollout/plan execution, these are updated by completePlan.
	if !hasConditionTrue(group, seiv1alpha1.ConditionPlanInProgress) &&
		!hasConditionTrue(group, seiv1alpha1.ConditionRolloutInProgress) {
		group.Status.ObservedGeneration = group.Generation
		group.Status.TemplateHash = templateHash(&group.Spec.Template.Spec)
	}
	group.Status.Replicas = group.Spec.Replicas
	group.Status.ReadyReplicas = readyReplicas
	group.Status.Nodes = nodeStatuses

	reconcileRolloutStatus(group, nodes)

	group.Status.Phase = computeGroupPhase(group, readyReplicas, group.Spec.Replicas, nodes)

	group.Status.NetworkingStatus = r.buildNetworkingStatus(group)

	setNodesReadyCondition(group, readyReplicas, group.Spec.Replicas, nodes)

	return r.Status().Patch(ctx, group, statusBase)
}

func reconcileRolloutStatus(group *seiv1alpha1.SeiNodeDeployment, nodes []seiv1alpha1.SeiNode) {
	if group.Status.Rollout == nil || group.Status.Rollout.Strategy != seiv1alpha1.UpdateStrategyInPlace {
		return
	}

	nodePhaseMap := make(map[string]seiv1alpha1.SeiNodePhase, len(nodes))
	for i := range nodes {
		nodePhaseMap[nodes[i].Name] = nodes[i].Status.Phase
	}

	allReady := true
	for i := range group.Status.Rollout.Nodes {
		rn := &group.Status.Rollout.Nodes[i]
		phase := nodePhaseMap[rn.Name]
		rn.Phase = phase
		rn.Ready = phase == seiv1alpha1.PhaseRunning
		if !rn.Ready {
			allReady = false
		}
	}

	if allReady && !hasConditionTrue(group, seiv1alpha1.ConditionPlanInProgress) {
		group.Status.TemplateHash = group.Status.Rollout.TargetHash
		group.Status.ObservedGeneration = group.Generation
		group.Status.Rollout = nil
		setCondition(group, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionFalse,
			"RolloutComplete", "All nodes converged")
		return
	}
}

func computeGroupPhase(group *seiv1alpha1.SeiNodeDeployment, ready, desired int32, nodes []seiv1alpha1.SeiNode) seiv1alpha1.SeiNodeDeploymentPhase {
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
