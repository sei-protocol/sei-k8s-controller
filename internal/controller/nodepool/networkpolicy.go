package nodepool

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	seiv1alpha1 "github.com/sei-protocol/sei-node-controller/api/v1alpha1"
)

func generateNetworkPolicy(sn *seiv1alpha1.SeiNodePool) *networkingv1.NetworkPolicy {
	labels := resourceLabels(sn)
	protocol := corev1.ProtocolTCP

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sn.Name,
			Namespace: sn.Namespace,
			Labels:    labels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: labels,
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: labels,
							},
						},
					},
					Ports: networkPolicyPorts(protocol),
				},
			},
		},
	}
}

func networkPolicyPorts(protocol corev1.Protocol) []networkingv1.NetworkPolicyPort {
	ports := []int32{8545, 8546, 26656, 26657, 26660}
	result := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, p := range ports {
		port := intstr.FromInt32(p)
		result = append(result, networkingv1.NetworkPolicyPort{
			Protocol: &protocol,
			Port:     &port,
		})
	}
	return result
}
