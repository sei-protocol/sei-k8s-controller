package nodedeployment

import (
	"testing"

	. "github.com/onsi/gomega"
	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

const (
	testChainAtlantic   = "atlantic-2"
	testP2PEndpointProd = "prod.platform.sei.io"
)

func TestP2PEndpointHostname_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		sndName           string
		templateChainID   string
		genesisChainID    string
		ordinal           int
		p2pEndpointDomain string
		want              string
	}{
		{
			name:              "atlantic-2 ordinal 0",
			sndName:           testChainAtlantic,
			templateChainID:   testChainAtlantic,
			ordinal:           0,
			p2pEndpointDomain: testP2PEndpointProd,
			want:              "atlantic-2-0-p2p.atlantic-2.prod.platform.sei.io",
		},
		{
			name:              "ordinal 5 of multi-replica fleet",
			sndName:           "validators",
			templateChainID:   testNamespace,
			ordinal:           5,
			p2pEndpointDomain: testP2PEndpointProd,
			want:              "validators-5-p2p.pacific-1.prod.platform.sei.io",
		},
		{
			name:              "genesis-only chainID (validator SND)",
			sndName:           "newchain-validators",
			genesisChainID:    "newchain-1",
			ordinal:           1,
			p2pEndpointDomain: "test.platform.sei.io",
			want:              "newchain-validators-1-p2p.newchain-1.test.platform.sei.io",
		},
		{
			name:              "empty publishable domain returns empty",
			sndName:           testChainAtlantic,
			templateChainID:   testChainAtlantic,
			ordinal:           0,
			p2pEndpointDomain: "",
			want:              "",
		},
		{
			name:              "empty chain ID returns empty",
			sndName:           testChainAtlantic,
			ordinal:           0,
			p2pEndpointDomain: testP2PEndpointProd,
			want:              "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			snd := &seiv1alpha1.SeiNodeDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: tc.sndName},
				Spec: seiv1alpha1.SeiNodeDeploymentSpec{
					Template: seiv1alpha1.SeiNodeTemplate{
						Spec: seiv1alpha1.SeiNodeSpec{ChainID: tc.templateChainID},
					},
				},
			}
			if tc.genesisChainID != "" {
				snd.Spec.Genesis = &seiv1alpha1.GenesisCeremonyConfig{ChainID: tc.genesisChainID}
			}
			r := &SeiNodeDeploymentReconciler{P2PEndpointDomain: tc.p2pEndpointDomain}
			g.Expect(r.p2pEndpointHostname(snd, tc.ordinal)).To(Equal(tc.want))
		})
	}
}

func TestP2PEndpointExternalAddress_AppendsP2PPort(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: testChainAtlantic},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Template: seiv1alpha1.SeiNodeTemplate{
				Spec: seiv1alpha1.SeiNodeSpec{ChainID: testChainAtlantic},
			},
		},
	}
	r := &SeiNodeDeploymentReconciler{P2PEndpointDomain: testP2PEndpointProd}
	g.Expect(r.p2pEndpointAddress(snd, 0)).To(Equal("atlantic-2-0-p2p.atlantic-2.prod.platform.sei.io:26656"))
}

func TestP2PEndpointExternalAddress_EmptyWhenHostnameRejected(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: testChainAtlantic},
	}
	r := &SeiNodeDeploymentReconciler{P2PEndpointDomain: testP2PEndpointProd}
	g.Expect(r.p2pEndpointAddress(snd, 0)).To(BeEmpty())
}

func TestP2PEndpointServiceName(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: testChainAtlantic},
	}
	g.Expect(p2pEndpointServiceName(snd, 0)).To(Equal("atlantic-2-0-p2p"))
	g.Expect(p2pEndpointServiceName(snd, 7)).To(Equal("atlantic-2-7-p2p"))
}

func TestGeneratePublishableService_Annotations(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: testChainAtlantic, Namespace: "sei-test-1"},
	}
	svc := generateP2PEndpointService(snd, 0, "atlantic-2-0-p2p.atlantic-2.prod.platform.sei.io", NLBTargetTypeIP)

	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"external-dns.alpha.kubernetes.io/hostname",
		"atlantic-2-0-p2p.atlantic-2.prod.platform.sei.io",
	))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-type", "external"))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-scheme", "internet-facing"))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-nlb-target-type", "ip"))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-attributes",
		"load_balancing.cross_zone.enabled=true"))

	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
	g.Expect(svc.Spec.ExternalTrafficPolicy).To(Equal(corev1.ServiceExternalTrafficPolicyTypeLocal))
	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(seiconfig.PortP2P))
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(noderesource.NodeLabel, "atlantic-2-0"))

	// target-type=ip → NodePort allocation explicitly disabled (preserve
	// the 30000-32767 range; pod IP is the NLB target, no NodePort hop).
	g.Expect(svc.Spec.AllocateLoadBalancerNodePorts).NotTo(BeNil())
	g.Expect(*svc.Spec.AllocateLoadBalancerNodePorts).To(BeFalse())
}

func TestGeneratePublishableService_InstanceTargetType(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: testChainAtlantic, Namespace: "sei-test-1"},
	}
	svc := generateP2PEndpointService(snd, 0, "atlantic-2-0-p2p.atlantic-2.harbor.platform.sei.io", NLBTargetTypeInstance)

	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-nlb-target-type", "instance"))

	// target-type=instance needs a NodePort for the NLB→node→pod hop.
	// Leaving the field nil lets kube default to true.
	g.Expect(svc.Spec.AllocateLoadBalancerNodePorts).To(BeNil())
}
