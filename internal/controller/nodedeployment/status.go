package nodegroup

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

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

	// Update ObservedGeneration and TemplateHash when no plan is active.
	// During plan execution, ObservedGeneration is updated by completePlan.
	if !hasConditionTrue(group, seiv1alpha1.ConditionPlanInProgress) {
		group.Status.ObservedGeneration = group.Generation
		group.Status.TemplateHash = templateHash(&group.Spec.Template.Spec)
	}
	group.Status.Replicas = group.Spec.Replicas
	group.Status.ReadyReplicas = readyReplicas
	group.Status.Nodes = nodeStatuses

	group.Status.Phase = computeGroupPhase(group, readyReplicas, group.Spec.Replicas, nodes)

	svc, svcErr := r.fetchExternalService(ctx, group)
	group.Status.NetworkingStatus = buildNetworkingStatus(group, svc)

	setNodesReadyCondition(group, readyReplicas, group.Spec.Replicas, nodes)
	setExternalServiceCondition(group, svc, svcErr)

	return r.Status().Patch(ctx, group, statusBase)
}

func computeGroupPhase(group *seiv1alpha1.SeiNodeDeployment, ready, desired int32, nodes []seiv1alpha1.SeiNode) seiv1alpha1.SeiNodeDeploymentPhase {
	if hasConditionTrue(group, seiv1alpha1.ConditionPlanInProgress) {
		if group.Status.Deployment != nil {
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

// fetchExternalService returns the external Service if networking is configured.
// Returns (nil, nil) when not configured or NotFound, and (nil, err) on
// transient API errors so callers can surface an accurate condition.
func (r *SeiNodeDeploymentReconciler) fetchExternalService(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) (*corev1.Service, error) {
	if group.Spec.Networking == nil || group.Spec.Networking.Service == nil {
		return nil, nil
	}
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: externalServiceName(group), Namespace: group.Namespace}, svc)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		log.FromContext(ctx).Error(err, "fetching external Service for status")
		return nil, err
	}
	return svc, nil
}

func buildNetworkingStatus(group *seiv1alpha1.SeiNodeDeployment, svc *corev1.Service) *seiv1alpha1.NetworkingStatus {
	if group.Spec.Networking == nil || group.Spec.Networking.Service == nil {
		return nil
	}
	svcName := externalServiceName(group)
	status := &seiv1alpha1.NetworkingStatus{ExternalServiceName: svcName}
	if svc != nil && len(svc.Status.LoadBalancer.Ingress) > 0 {
		status.LoadBalancerIngress = svc.Status.LoadBalancer.Ingress
	}
	return status
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

func setExternalServiceCondition(group *seiv1alpha1.SeiNodeDeployment, svc *corev1.Service, fetchErr error) {
	if group.Spec.Networking == nil || group.Spec.Networking.Service == nil {
		return
	}

	if fetchErr != nil {
		setCondition(group, seiv1alpha1.ConditionExternalServiceReady, metav1.ConditionFalse,
			"FetchError", fmt.Sprintf("Unable to fetch external Service: %v", fetchErr))
		return
	}

	if svc == nil {
		setCondition(group, seiv1alpha1.ConditionExternalServiceReady, metav1.ConditionFalse,
			"ServiceNotFound", "External Service not yet created")
		return
	}

	if svc.Spec.Type == corev1.ServiceTypeLoadBalancer && len(svc.Status.LoadBalancer.Ingress) == 0 {
		setCondition(group, seiv1alpha1.ConditionExternalServiceReady, metav1.ConditionFalse,
			"LoadBalancerPending", "Waiting for load balancer provisioning")
		return
	}

	setCondition(group, seiv1alpha1.ConditionExternalServiceReady, metav1.ConditionTrue,
		"ServiceReady", fmt.Sprintf("External Service %s is ready", svc.Name))
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
