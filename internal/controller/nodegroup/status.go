package nodegroup

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func (r *SeiNodeGroupReconciler) updateStatus(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	patch := client.MergeFrom(group.DeepCopy())

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

	group.Status.ObservedGeneration = group.Generation
	group.Status.Replicas = group.Spec.Replicas
	group.Status.ReadyReplicas = readyReplicas
	group.Status.Nodes = nodeStatuses
	group.Status.Phase = computeGroupPhase(readyReplicas, group.Spec.Replicas, nodes)
	group.Status.NetworkingStatus = r.readNetworkingStatus(ctx, group)

	setNodesReadyCondition(group, readyReplicas, group.Spec.Replicas, nodes)
	setExternalServiceCondition(ctx, r, group)

	return r.Status().Patch(ctx, group, patch)
}

func computeGroupPhase(ready, desired int32, nodes []seiv1alpha1.SeiNode) seiv1alpha1.SeiNodeGroupPhase {
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
		// Some failed, some still initializing
	}
	return seiv1alpha1.GroupPhaseInitializing
}

func (r *SeiNodeGroupReconciler) readNetworkingStatus(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) *seiv1alpha1.NetworkingStatus {
	if group.Spec.Networking == nil || group.Spec.Networking.Service == nil {
		return nil
	}

	svc := &corev1.Service{}
	svcName := externalServiceName(group)
	if err := r.Get(ctx, types.NamespacedName{Name: svcName, Namespace: group.Namespace}, svc); err != nil {
		return &seiv1alpha1.NetworkingStatus{ExternalServiceName: svcName}
	}

	status := &seiv1alpha1.NetworkingStatus{
		ExternalServiceName: svcName,
	}
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		status.LoadBalancerIngress = svc.Status.LoadBalancer.Ingress
	}
	return status
}

func setNodesReadyCondition(group *seiv1alpha1.SeiNodeGroup, ready, desired int32, nodes []seiv1alpha1.SeiNode) {
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
			case seiv1alpha1.PhasePending, seiv1alpha1.PhasePreInitializing, seiv1alpha1.PhaseInitializing:
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

func setExternalServiceCondition(ctx context.Context, r *SeiNodeGroupReconciler, group *seiv1alpha1.SeiNodeGroup) {
	if group.Spec.Networking == nil || group.Spec.Networking.Service == nil {
		return
	}

	svc := &corev1.Service{}
	svcName := externalServiceName(group)
	if err := r.Get(ctx, types.NamespacedName{Name: svcName, Namespace: group.Namespace}, svc); err != nil {
		if apierrors.IsNotFound(err) {
			setCondition(group, seiv1alpha1.ConditionExternalServiceReady, metav1.ConditionFalse,
				"ServiceNotFound", "External Service not yet created")
		}
		return
	}

	if svc.Spec.Type == corev1.ServiceTypeLoadBalancer && len(svc.Status.LoadBalancer.Ingress) == 0 {
		setCondition(group, seiv1alpha1.ConditionExternalServiceReady, metav1.ConditionFalse,
			"LoadBalancerPending", "Waiting for load balancer provisioning")
		return
	}

	setCondition(group, seiv1alpha1.ConditionExternalServiceReady, metav1.ConditionTrue,
		"ServiceReady", fmt.Sprintf("External Service %s is ready", svcName))
}

func setCondition(group *seiv1alpha1.SeiNodeGroup, condType string, status metav1.ConditionStatus, reason, message string) {
	condition := metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: group.Generation,
		LastTransitionTime: metav1.Now(),
	}

	for i, c := range group.Status.Conditions {
		if c.Type == condType {
			if c.Status == status {
				condition.LastTransitionTime = c.LastTransitionTime
			}
			group.Status.Conditions[i] = condition
			return
		}
	}
	group.Status.Conditions = append(group.Status.Conditions, condition)
}
