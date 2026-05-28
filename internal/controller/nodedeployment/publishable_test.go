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

func TestPublishableHostname_Table(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		sndName          string
		namespace        string
		templateChainID  string
		genesisChainID   string
		ordinal          int
		publishableDomain string
		want             string
	}{
		{
			name:             "atlantic-2 ordinal 0",
			sndName:          "atlantic-2",
			namespace:        "sei-test-1",
			templateChainID:  "atlantic-2",
			ordinal:          0,
			publishableDomain: "prod.platform.sei.io",
			want:             "atlantic-2-0-p2p.atlantic-2.prod.platform.sei.io",
		},
		{
			name:             "ordinal 5 of multi-replica fleet",
			sndName:          "rpc",
			namespace:        "pacific-1",
			templateChainID:  "pacific-1",
			ordinal:          5,
			publishableDomain: "prod.platform.sei.io",
			want:             "rpc-5-p2p.pacific-1.prod.platform.sei.io",
		},
		{
			name:             "genesis-only chainID (validator SND)",
			sndName:          "newchain-validators",
			namespace:        "ceremony",
			templateChainID:  "",
			genesisChainID:   "newchain-1",
			ordinal:          1,
			publishableDomain: "test.platform.sei.io",
			want:             "newchain-validators-1-p2p.newchain-1.test.platform.sei.io",
		},
		{
			name:             "empty gateway public domain returns empty",
			sndName:          "atlantic-2",
			namespace:        "sei-test-1",
			templateChainID:  "atlantic-2",
			ordinal:          0,
			publishableDomain: "",
			want:             "",
		},
		{
			name:             "empty chain ID returns empty",
			sndName:          "atlantic-2",
			namespace:        "sei-test-1",
			templateChainID:  "",
			ordinal:          0,
			publishableDomain: "prod.platform.sei.io",
			want:             "",
		},
		{
			name:             "chainID with uppercase fails the DNS-1123 belt-and-suspenders",
			sndName:          "atlantic-2",
			namespace:        "sei-test-1",
			templateChainID:  "Atlantic-2",
			ordinal:          0,
			publishableDomain: "prod.platform.sei.io",
			want:             "",
		},
		{
			name:             "chainID with dot fails the DNS-1123 belt-and-suspenders",
			sndName:          "atlantic-2",
			namespace:        "sei-test-1",
			templateChainID:  "atlantic.2",
			ordinal:          0,
			publishableDomain: "prod.platform.sei.io",
			want:             "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			snd := &seiv1alpha1.SeiNodeDeployment{
				ObjectMeta: metav1.ObjectMeta{Name: tc.sndName, Namespace: tc.namespace},
				Spec: seiv1alpha1.SeiNodeDeploymentSpec{
					Template: seiv1alpha1.SeiNodeTemplate{
						Spec: seiv1alpha1.SeiNodeSpec{ChainID: tc.templateChainID},
					},
				},
			}
			if tc.genesisChainID != "" {
				snd.Spec.Genesis = &seiv1alpha1.GenesisCeremonyConfig{ChainID: tc.genesisChainID}
			}
			r := &SeiNodeDeploymentReconciler{PublishableDomain:tc.publishableDomain}
			g.Expect(r.publishableHostname(snd, tc.ordinal)).To(Equal(tc.want))
		})
	}
}

func TestPublishableExternalAddress_AppendsP2PPort(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "atlantic-2", Namespace: "sei-test-1"},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Template: seiv1alpha1.SeiNodeTemplate{
				Spec: seiv1alpha1.SeiNodeSpec{ChainID: "atlantic-2"},
			},
		},
	}
	r := &SeiNodeDeploymentReconciler{PublishableDomain:"prod.platform.sei.io"}
	got := r.publishableExternalAddress(snd, 0)
	g.Expect(got).To(Equal("atlantic-2-0-p2p.atlantic-2.prod.platform.sei.io:26656"))
}

func TestPublishableExternalAddress_EmptyWhenHostnameRejected(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "atlantic-2", Namespace: "sei-test-1"},
	}
	r := &SeiNodeDeploymentReconciler{PublishableDomain:"prod.platform.sei.io"}
	g.Expect(r.publishableExternalAddress(snd, 0)).To(BeEmpty())
}

func TestPublishableServiceName(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "atlantic-2", Namespace: "sei-test-1"},
	}
	g.Expect(publishableServiceName(snd, 0)).To(Equal("atlantic-2-0-p2p"))
	g.Expect(publishableServiceName(snd, 7)).To(Equal("atlantic-2-7-p2p"))
}

func TestGeneratePublishableService_Shape(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "atlantic-2", Namespace: "sei-test-1"},
	}
	host := "atlantic-2-0-p2p.atlantic-2.prod.platform.sei.io"

	svc := generatePublishableService(snd, 0, host)

	g.Expect(svc.Name).To(Equal("atlantic-2-0-p2p"))
	g.Expect(svc.Namespace).To(Equal("sei-test-1"))

	// Labels: SND group label, ordinal label, and the new component label
	// so cleanup queries can target publishable Services without
	// entangling other group-owned objects.
	g.Expect(svc.Labels).To(HaveKeyWithValue(groupLabel, "atlantic-2"))
	g.Expect(svc.Labels).To(HaveKeyWithValue(groupOrdinalLabel, "0"))
	g.Expect(svc.Labels).To(HaveKeyWithValue(publishableComponentLabel, publishableComponentValue))

	// Annotations: external-dns hostname pins the vanity → NLB CNAME;
	// AWS LBC annotations select the NLB shape; managed-by is operator
	// guidance, matching the HTTPRoute precedent.
	g.Expect(svc.Annotations).To(HaveKeyWithValue(externalDNSHostnameAnnotation, host))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(awsLBTypeAnnotation, awsLBTypeValue))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(awsLBSchemeAnnotation, awsLBSchemeValue))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(awsLBTargetTypeAnnotation, awsLBTargetTypeValue))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(awsLBCrossZoneAnnotation, awsLBCrossZoneAttributeStr))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(managedByAnnotation, controllerName))

	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
	g.Expect(svc.Spec.ExternalTrafficPolicy).To(Equal(corev1.ServiceExternalTrafficPolicyTypeLocal))
	g.Expect(svc.Spec.AllocateLoadBalancerNodePorts).NotTo(BeNil())
	g.Expect(*svc.Spec.AllocateLoadBalancerNodePorts).To(BeFalse())

	// Selector matches the child SeiNode's pod via the `sei.io/node`
	// label the SeiNode controller stamps on its StatefulSet template.
	// Using that label (rather than the SND group label) ensures the
	// NLB has exactly one healthy target — the per-replica pod —
	// instead of round-robining across siblings.
	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{
		noderesource.NodeLabel: "atlantic-2-0",
	}))

	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(seiconfig.PortP2P))
	g.Expect(svc.Spec.Ports[0].Name).To(Equal("p2p"))
	g.Expect(svc.Spec.Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))
}

func TestEffectiveChainID_GenesisFallback(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Template: seiv1alpha1.SeiNodeTemplate{
				Spec: seiv1alpha1.SeiNodeSpec{ChainID: ""},
			},
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{ChainID: "newchain-1"},
		},
	}
	g.Expect(effectiveChainID(snd)).To(Equal("newchain-1"))
}

func TestEffectiveChainID_TemplateWins(t *testing.T) {
	g := NewWithT(t)
	snd := &seiv1alpha1.SeiNodeDeployment{
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Template: seiv1alpha1.SeiNodeTemplate{
				Spec: seiv1alpha1.SeiNodeSpec{ChainID: "pacific-1"},
			},
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{ChainID: "should-be-ignored"},
		},
	}
	g.Expect(effectiveChainID(snd)).To(Equal("pacific-1"))
}

func TestExternalAddressEqual_NormalizesNilAndEmpty(t *testing.T) {
	g := NewWithT(t)
	empty := ""
	val := "host:26656"
	other := "other:26656"
	g.Expect(externalAddressEqual(nil, nil)).To(BeTrue())
	g.Expect(externalAddressEqual(nil, &empty)).To(BeTrue())
	g.Expect(externalAddressEqual(&empty, nil)).To(BeTrue())
	g.Expect(externalAddressEqual(&val, &val)).To(BeTrue())
	g.Expect(externalAddressEqual(&val, &other)).To(BeFalse())
	g.Expect(externalAddressEqual(nil, &val)).To(BeFalse())
	g.Expect(externalAddressEqual(&val, nil)).To(BeFalse())
}

func TestNetworkingConfig_TCPEnabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  *seiv1alpha1.NetworkingConfig
		want bool
	}{
		{name: "nil receiver", cfg: nil, want: false},
		{name: "legacy empty struct does not enable TCP", cfg: &seiv1alpha1.NetworkingConfig{}, want: false},
		{name: "HTTP-only does not enable TCP", cfg: &seiv1alpha1.NetworkingConfig{HTTP: &seiv1alpha1.HTTPConfig{}}, want: false},
		{name: "explicit TCP enables TCP", cfg: &seiv1alpha1.NetworkingConfig{TCP: &seiv1alpha1.TCPConfig{}}, want: true},
		{name: "HTTP and TCP both set", cfg: &seiv1alpha1.NetworkingConfig{HTTP: &seiv1alpha1.HTTPConfig{}, TCP: &seiv1alpha1.TCPConfig{}}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := NewWithT(t)
			g.Expect(tc.cfg.TCPEnabled()).To(Equal(tc.want))
		})
	}
}
