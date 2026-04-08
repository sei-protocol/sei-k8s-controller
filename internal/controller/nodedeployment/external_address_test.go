package nodedeployment

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestHasExternalService(t *testing.T) {
	r := &SeiNodeDeploymentReconciler{}

	t.Run("nil networking", func(t *testing.T) {
		g := NewWithT(t)
		group := newTestGroup("test", "ns")
		g.Expect(r.hasExternalService(group)).To(BeFalse())
	})

	t.Run("nil service", func(t *testing.T) {
		g := NewWithT(t)
		group := newTestGroup("test", "ns")
		group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}
		g.Expect(r.hasExternalService(group)).To(BeFalse())
	})

	t.Run("service configured", func(t *testing.T) {
		g := NewWithT(t)
		group := newTestGroup("test", "ns")
		group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
			Service: &seiv1alpha1.ExternalServiceConfig{Type: corev1.ServiceTypeLoadBalancer},
		}
		g.Expect(r.hasExternalService(group)).To(BeTrue())
	})
}

func TestResolveExternalP2PAddress_FromService(t *testing.T) {
	t.Run("prefers hostname over IP", func(t *testing.T) {
		g := NewWithT(t)
		svc := &corev1.Service{
			Status: corev1.ServiceStatus{
				LoadBalancer: corev1.LoadBalancerStatus{
					Ingress: []corev1.LoadBalancerIngress{
						{Hostname: "p2p.atlantic-2.seinetwork.io", IP: "1.2.3.4"},
					},
				},
			},
		}
		addr := externalAddressFromService(svc)
		g.Expect(addr).To(Equal("p2p.atlantic-2.seinetwork.io:26656"))
	})

	t.Run("falls back to IP", func(t *testing.T) {
		g := NewWithT(t)
		svc := &corev1.Service{
			Status: corev1.ServiceStatus{
				LoadBalancer: corev1.LoadBalancerStatus{
					Ingress: []corev1.LoadBalancerIngress{
						{IP: "10.0.0.1"},
					},
				},
			},
		}
		addr := externalAddressFromService(svc)
		g.Expect(addr).To(Equal("10.0.0.1:26656"))
	})

	t.Run("empty when no ingress", func(t *testing.T) {
		g := NewWithT(t)
		svc := &corev1.Service{}
		addr := externalAddressFromService(svc)
		g.Expect(addr).To(BeEmpty())
	})

	t.Run("empty when nil", func(t *testing.T) {
		g := NewWithT(t)
		addr := externalAddressFromService(nil)
		g.Expect(addr).To(BeEmpty())
	})
}
