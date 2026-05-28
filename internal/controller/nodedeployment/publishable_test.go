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
	testPublishableProd = "prod.platform.sei.io"
)

func TestPublishableHostname_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		sndName           string
		templateChainID   string
		genesisChainID    string
		ordinal           int
		publishableDomain string
		want              string
	}{
		{
			name:              "atlantic-2 ordinal 0",
			sndName:           testChainAtlantic,
			templateChainID:   testChainAtlantic,
			ordinal:           0,
			publishableDomain: testPublishableProd,
			want:              "atlantic-2-0-p2p.atlantic-2.prod.platform.sei.io",
		},
		{
			name:              "ordinal 5 of multi-replica fleet",
			sndName:           "validators",
			templateChainID:   testNamespace,
			ordinal:           5,
			publishableDomain: testPublishableProd,
			want:              "validators-5-p2p.pacific-1.prod.platform.sei.io",
		},
		{
			name:              "genesis-only chainID (validator SND)",
			sndName:           "newchain-validators",
			genesisChainID:    "newchain-1",
			ordinal:           1,
			publishableDomain: "test.platform.sei.io",
			want:              "newchain-validators-1-p2p.newchain-1.test.platform.sei.io",
		},
		{
			name:              "empty publishable domain returns empty",
			sndName:           testChainAtlantic,
			templateChainID:   testChainAtlantic,
			ordinal:           0,
			publishableDomain: "",
			want:              "",
		},
		{
			name:              "empty chain ID returns empty",
			sndName:           testChainAtlantic,
			ordinal:           0,
			publishableDomain: testPublishableProd,
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
			r := &SeiNodeDeploymentReconciler{PublishableDomain: tc.publishableDomain}
			g.Expect(r.publishableHostname(snd, tc.ordinal)).To(Equal(tc.want))
		})
	}
}

func TestPublishableExternalAddress_AppendsP2PPort(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: testChainAtlantic},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Template: seiv1alpha1.SeiNodeTemplate{
				Spec: seiv1alpha1.SeiNodeSpec{ChainID: testChainAtlantic},
			},
		},
	}
	r := &SeiNodeDeploymentReconciler{PublishableDomain: testPublishableProd}
	g.Expect(r.publishableExternalAddress(snd, 0)).To(Equal("atlantic-2-0-p2p.atlantic-2.prod.platform.sei.io:26656"))
}

func TestPublishableExternalAddress_EmptyWhenHostnameRejected(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: testChainAtlantic},
	}
	r := &SeiNodeDeploymentReconciler{PublishableDomain: testPublishableProd}
	g.Expect(r.publishableExternalAddress(snd, 0)).To(BeEmpty())
}

func TestPublishableServiceName(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: testChainAtlantic},
	}
	g.Expect(publishableServiceName(snd, 0)).To(Equal("atlantic-2-0-p2p"))
	g.Expect(publishableServiceName(snd, 7)).To(Equal("atlantic-2-7-p2p"))
}

func TestGeneratePublishableService_Annotations(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: testChainAtlantic, Namespace: "sei-test-1"},
	}
	svc := generatePublishableService(snd, 0, "atlantic-2-0-p2p.atlantic-2.prod.platform.sei.io")

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
}
