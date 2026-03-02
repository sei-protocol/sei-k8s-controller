package node

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-node-controller/api/v1alpha1"
)

const (
	ConditionTypeReady       = "Ready"
	ConditionTypeDegraded    = "Degraded"
	ConditionTypeProgressing = "Progressing"
)

const (
	ReasonReconcileSucceeded  = "ReconcileSucceeded"
	ReasonReconcileFailed     = "ReconcileFailed"
	ReasonPodsNotReady        = "PodsNotReady"
	ReasonAllPodsReady        = "AllPodsReady"
	ReasonPeerDiscoveryFailed = "PeerDiscoveryFailed"
)

func setNodeCondition(node *seiv1alpha1.SeiNode, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		ObservedGeneration: node.Generation,
		Message:            message,
	})
}

func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
