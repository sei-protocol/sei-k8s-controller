package noderesource

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

func withSidecarTLS(node *seiv1alpha1.SeiNode) *seiv1alpha1.SeiNode {
	node.Spec.Sidecar.TLS = &seiv1alpha1.SidecarTLSSpec{
		SecretName: node.Name + "-tls",
	}
	return node
}

func TestSidecarTLSEnabled(t *testing.T) {
	g := NewWithT(t)
	g.Expect(SidecarTLSEnabled(newGenesisNode("a", "default"))).To(BeFalse())
	g.Expect(SidecarTLSEnabled(withSidecarTLS(newGenesisNode("a", "default")))).To(BeTrue())

	node := newGenesisNode("a", "default")
	node.Spec.Sidecar = nil
	g.Expect(SidecarTLSEnabled(node)).To(BeFalse())
}

func TestPodSpec_NoProxyContainerWithoutTLS(t *testing.T) {
	g := NewWithT(t)
	sts := mustGenerateStatefulSet(t, newGenesisNode("a", "default"), platformtest.Config())
	g.Expect(findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameRBACProxy)).To(BeNil())
}

func TestPodSpec_ProxyContainerWhenTLSSet(t *testing.T) {
	g := NewWithT(t)
	node := withSidecarTLS(newGenesisNode("a", "default"))
	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	proxy := findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameRBACProxy)
	g.Expect(proxy).NotTo(BeNil())
	g.Expect(proxy.Image).To(Equal(platformtest.Config().KubeRBACProxyImage))
	g.Expect(*proxy.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))
	g.Expect(proxy.Args).To(ContainElement("--secure-listen-address=0.0.0.0:8443"))
	g.Expect(proxy.Args).To(ContainElement("--upstream=http://127.0.0.1:7777/"))
	g.Expect(proxy.Args).To(ContainElement("--tls-reload-interval=30s"))

	// --ignore-paths is the only flag that bypasses authn+authz; the
	// config-file's allowedPaths gates at the authz layer (still requires
	// authentication, which kubelet probes lack).
	var ignorePaths string
	for _, a := range proxy.Args {
		if v, ok := strings.CutPrefix(a, "--ignore-paths="); ok {
			ignorePaths = v
		}
	}
	g.Expect(ignorePaths).NotTo(BeEmpty(), "--ignore-paths required for kubelet probe bypass")
	for _, p := range bypassPaths() {
		g.Expect(ignorePaths).To(ContainSubstring(p))
	}

	g.Expect(proxy.SecurityContext).NotTo(BeNil())
	g.Expect(*proxy.SecurityContext.RunAsNonRoot).To(BeTrue())
}

func TestPodSpec_ProxyHasProbes(t *testing.T) {
	g := NewWithT(t)
	node := withSidecarTLS(newGenesisNode("a", "default"))
	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	proxy := findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameRBACProxy)
	g.Expect(proxy.StartupProbe).NotTo(BeNil())
	g.Expect(proxy.StartupProbe.TCPSocket).NotTo(BeNil(),
		"TCP startup probe — TLS handshake shouldn't gate while the cert is first-issuing")
	g.Expect(proxy.LivenessProbe.HTTPGet.Scheme).To(Equal(corev1.URISchemeHTTPS))
	g.Expect(proxy.LivenessProbe.HTTPGet.Path).To(Equal("/v0/livez"))
	g.Expect(proxy.ReadinessProbe.HTTPGet.Scheme).To(Equal(corev1.URISchemeHTTPS))
	g.Expect(proxy.ReadinessProbe.HTTPGet.Path).To(Equal("/v0/healthz"))
}

func TestPodSpec_SidecarGetsTrustedHeaderEnvWhenTLSSet(t *testing.T) {
	g := NewWithT(t)
	sts := mustGenerateStatefulSet(t, withSidecarTLS(newGenesisNode("a", "default")), platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameSidecar)
	g.Expect(envValue(sidecar.Env, "SEI_SIDECAR_AUTHN_MODE")).To(Equal("trusted-header"))
}

func TestPodSpec_SidecarNoTrustedHeaderEnvWithoutTLS(t *testing.T) {
	g := NewWithT(t)
	sts := mustGenerateStatefulSet(t, newGenesisNode("a", "default"), platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameSidecar)
	g.Expect(envValue(sidecar.Env, "SEI_SIDECAR_AUTHN_MODE")).To(Equal(""))
}

func TestPodSpec_ProxyVolumesPresentWhenTLSSet(t *testing.T) {
	g := NewWithT(t)
	sts := mustGenerateStatefulSet(t, withSidecarTLS(newGenesisNode("a", "default")), platformtest.Config())
	volumeNames := make([]string, 0, len(sts.Spec.Template.Spec.Volumes))
	for _, v := range sts.Spec.Template.Spec.Volumes {
		volumeNames = append(volumeNames, v.Name)
	}
	g.Expect(volumeNames).To(ContainElements(rbacProxyConfigVolumeName, sidecarTLSVolumeName))
}

// Probes on the seictl sidecar are kubelet-driven (use pod IP). In TLS
// mode the sidecar binds loopback, unreachable from kubelet — probes
// move to the proxy container. Pod readiness still gates on the proxy
// probe via the all-containers-ready rule.

func TestSidecarProbes_HTTPOnSidecarPortByDefault(t *testing.T) {
	g := NewWithT(t)
	sts := mustGenerateStatefulSet(t, newGenesisNode("a", "default"), platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameSidecar)
	g.Expect(sidecar.LivenessProbe.HTTPGet.Port.IntValue()).To(Equal(7777))
	g.Expect(sidecar.LivenessProbe.HTTPGet.Scheme).To(BeEquivalentTo(""))
	g.Expect(sidecar.ReadinessProbe.HTTPGet.Port.IntValue()).To(Equal(7777))
}

func TestSidecarProbes_AbsentInTLSMode(t *testing.T) {
	g := NewWithT(t)
	sts := mustGenerateStatefulSet(t, withSidecarTLS(newGenesisNode("a", "default")), platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameSidecar)
	g.Expect(sidecar.StartupProbe).To(BeNil())
	g.Expect(sidecar.LivenessProbe).To(BeNil())
	g.Expect(sidecar.ReadinessProbe).To(BeNil())
}

func TestSeidStartupProbe_TargetsProxyHTTPSInTLSMode(t *testing.T) {
	g := NewWithT(t)
	sts := mustGenerateStatefulSet(t, withSidecarTLS(newGenesisNode("a", "default")), platformtest.Config())
	seid := findContainer(sts.Spec.Template.Spec.Containers, containerNameSeid)
	g.Expect(seid.StartupProbe.HTTPGet.Scheme).To(Equal(corev1.URISchemeHTTPS))
	g.Expect(seid.StartupProbe.HTTPGet.Port.IntValue()).To(Equal(int(RBACProxyPort)))
}

func TestServicePorts_AddsAPIPortWhenTLSSet(t *testing.T) {
	g := NewWithT(t)
	svc := GenerateHeadlessService(withSidecarTLS(newGenesisNode("a", "default")))
	portNames := make([]string, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		portNames = append(portNames, p.Name)
	}
	g.Expect(portNames).To(ContainElement(servicePortNameAPI))
	for _, p := range svc.Spec.Ports {
		if p.Name == servicePortNameAPI {
			g.Expect(p.Port).To(Equal(RBACProxyPort))
		}
	}
}

func TestServicePorts_NoAPIPortWithoutTLS(t *testing.T) {
	g := NewWithT(t)
	svc := GenerateHeadlessService(newGenesisNode("a", "default"))
	for _, p := range svc.Spec.Ports {
		g.Expect(p.Name).NotTo(Equal(servicePortNameAPI))
	}
}

func TestSidecarTLSSecretName_ReadsFromSpec(t *testing.T) {
	g := NewWithT(t)
	g.Expect(SidecarTLSSecretName(newGenesisNode("a", "default"))).To(Equal(""),
		"empty when TLS disabled")

	node := withSidecarTLS(newGenesisNode("a", "default"))
	g.Expect(SidecarTLSSecretName(node)).To(Equal("a-tls"),
		"returns spec.sidecar.tls.secretName when TLS enabled")
}

func TestPodSpec_TLSVolumeUsesSecretNameFromSpec(t *testing.T) {
	g := NewWithT(t)
	node := withSidecarTLS(newGenesisNode("a", "default"))
	node.Spec.Sidecar.TLS.SecretName = "custom-cert-secret"
	sts := mustGenerateStatefulSet(t, node, platformtest.Config())

	var tlsVol *corev1.Volume
	for i := range sts.Spec.Template.Spec.Volumes {
		if sts.Spec.Template.Spec.Volumes[i].Name == sidecarTLSVolumeName {
			tlsVol = &sts.Spec.Template.Spec.Volumes[i]
			break
		}
	}
	g.Expect(tlsVol).NotTo(BeNil())
	g.Expect(tlsVol.Secret).NotTo(BeNil())
	g.Expect(tlsVol.Secret.SecretName).To(Equal("custom-cert-secret"))
}

func TestGenerateRBACProxyConfigMap_NilWithoutTLS(t *testing.T) {
	g := NewWithT(t)
	g.Expect(GenerateRBACProxyConfigMap(newGenesisNode("a", "default"))).To(BeNil())
}

func TestGenerateRBACProxyConfigMap_UsesGroupNotApiGroup(t *testing.T) {
	g := NewWithT(t)
	cm := GenerateRBACProxyConfigMap(withSidecarTLS(newGenesisNode("validator-3", "sei-validators")))
	g.Expect(cm).NotTo(BeNil())
	g.Expect(cm.Name).To(Equal("validator-3-rbac-proxy-config"))

	cfg := cm.Data["config.yaml"]
	g.Expect(cfg).To(ContainSubstring("group: sei.io"),
		"resourceAttributes uses 'group', not 'apiGroup' — kube-rbac-proxy unmarshals SAR ResourceAttributes, the field name is group")
	g.Expect(cfg).NotTo(ContainSubstring("apiGroup:"),
		"apiGroup would deserialize to empty group and silently fail-closed against the seinodetasks ClusterRole")
	g.Expect(cfg).To(ContainSubstring("resource: seinodetasks"))
	g.Expect(cfg).To(ContainSubstring("namespace: sei-validators"))
	g.Expect(cfg).To(ContainSubstring("name: validator-3"))
	// Bypass paths must NOT appear in the config file — they live on
	// the proxy's --ignore-paths CLI flag (see TestPodSpec_ProxyContainerWhenTLSSet).
	g.Expect(cfg).NotTo(ContainSubstring("allowedPaths:"))
}

func TestGenerateStatefulSet_TLSEnabledButImageMissing_Errors(t *testing.T) {
	g := NewWithT(t)
	cfg := platformtest.Config()
	cfg.KubeRBACProxyImage = ""
	_, err := GenerateStatefulSet(withSidecarTLS(newGenesisNode("a", "default")), cfg)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("SEI_KUBE_RBAC_PROXY_IMAGE"))
}

// TestKeyringInvariant_WithBothFeatures locks the trust-boundary guard
// (assertNoOperatorKeyringOnSeidContainers) under the combination of
// OperatorKeyring AND TLS — the new kube-rbac-proxy container must
// pass the guard.
func TestKeyringInvariant_WithBothFeatures(t *testing.T) {
	g := NewWithT(t)
	node := withSidecarTLS(newGenesisNode("a", "default"))
	node.Spec.Validator.OperatorKeyring = &seiv1alpha1.OperatorKeyringSource{
		Secret: &seiv1alpha1.SecretOperatorKeyringSource{
			SecretName: "a-opk",
			KeyName:    "node_admin",
			PassphraseSecretRef: seiv1alpha1.PassphraseSecretRef{
				SecretName: "a-opk-passphrase",
				Key:        "passphrase",
			},
		},
	}
	_, err := GenerateStatefulSet(node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
}

// TestRollback_RemovesProxyResources locks the symmetric behavior:
// generating with TLS then without TLS produces a clean StatefulSet
// without the proxy container, volumes, env, or Service port.
func TestRollback_RemovesProxyResources(t *testing.T) {
	g := NewWithT(t)
	node := withSidecarTLS(newGenesisNode("a", "default"))
	_ = mustGenerateStatefulSet(t, node, platformtest.Config())

	node.Spec.Sidecar.TLS = nil
	sts := mustGenerateStatefulSet(t, node, platformtest.Config())
	svc := GenerateHeadlessService(node)

	g.Expect(findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameRBACProxy)).To(BeNil())
	for _, v := range sts.Spec.Template.Spec.Volumes {
		g.Expect(v.Name).NotTo(Equal(rbacProxyConfigVolumeName))
		g.Expect(v.Name).NotTo(Equal(sidecarTLSVolumeName))
	}
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameSidecar)
	g.Expect(envValue(sidecar.Env, "SEI_SIDECAR_AUTHN_MODE")).To(Equal(""))
	for _, p := range svc.Spec.Ports {
		g.Expect(p.Name).NotTo(Equal(servicePortNameAPI))
	}
}

