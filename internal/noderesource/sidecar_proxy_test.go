package noderesource

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

func TestPodSpec_AlwaysHasProxyContainer(t *testing.T) {
	g := NewWithT(t)
	sts := mustGenerateStatefulSet(t, newGenesisNode("a", "default"), platformtest.Config())

	proxy := findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameRBACProxy)
	g.Expect(proxy).NotTo(BeNil())
	g.Expect(proxy.Image).To(Equal(platformtest.Config().KubeRBACProxyImage))
	g.Expect(*proxy.RestartPolicy).To(Equal(corev1.ContainerRestartPolicyAlways))
	g.Expect(proxy.Args).To(ContainElement("--insecure-listen-address=0.0.0.0:8443"))
	g.Expect(proxy.Args).To(ContainElement("--upstream=http://127.0.0.1:7777/"))

	for _, a := range proxy.Args {
		g.Expect(a).NotTo(HavePrefix("--tls-cert-file="), "TLS args must not appear")
		g.Expect(a).NotTo(HavePrefix("--tls-private-key-file="), "TLS args must not appear")
		g.Expect(a).NotTo(Equal("--tls-reload-interval=30s"), "TLS args must not appear")
		g.Expect(a).NotTo(HavePrefix("--secure-listen-address="), "secure listen replaced by insecure")
	}

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

func TestPodSpec_ProxyHasHTTPProbes(t *testing.T) {
	g := NewWithT(t)
	sts := mustGenerateStatefulSet(t, newGenesisNode("a", "default"), platformtest.Config())

	proxy := findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameRBACProxy)
	g.Expect(proxy.StartupProbe).NotTo(BeNil())
	g.Expect(proxy.StartupProbe.TCPSocket).NotTo(BeNil())
	g.Expect(proxy.LivenessProbe.HTTPGet.Scheme).NotTo(Equal(corev1.URISchemeHTTPS))
	g.Expect(proxy.LivenessProbe.HTTPGet.Path).To(Equal("/v0/livez"))
	g.Expect(proxy.ReadinessProbe.HTTPGet.Scheme).NotTo(Equal(corev1.URISchemeHTTPS))
	g.Expect(proxy.ReadinessProbe.HTTPGet.Path).To(Equal("/v0/healthz"))
}

func TestPodSpec_SidecarAlwaysInTrustedHeaderMode(t *testing.T) {
	g := NewWithT(t)
	sts := mustGenerateStatefulSet(t, newGenesisNode("a", "default"), platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameSidecar)
	g.Expect(envValue(sidecar.Env, "SEI_SIDECAR_AUTHN_MODE")).To(Equal("trusted-header"))
}

func TestPodSpec_SidecarProbesAreNil(t *testing.T) {
	g := NewWithT(t)
	sts := mustGenerateStatefulSet(t, newGenesisNode("a", "default"), platformtest.Config())
	sidecar := findInitContainer(sts.Spec.Template.Spec.InitContainers, containerNameSidecar)
	g.Expect(sidecar.LivenessProbe).To(BeNil())
	g.Expect(sidecar.ReadinessProbe).To(BeNil())
}

func TestServicePorts_AlwaysIncludesAPIPort(t *testing.T) {
	g := NewWithT(t)
	svc := GenerateHeadlessService(newGenesisNode("a", "default"))

	var found bool
	for _, p := range svc.Spec.Ports {
		if p.Name == servicePortNameAPI {
			g.Expect(p.Port).To(Equal(RBACProxyPort))
			found = true
		}
	}
	g.Expect(found).To(BeTrue(), "headless Service must publish the proxy API port")
}

func TestGenerateRBACProxyConfigMap_UsesApiGroup(t *testing.T) {
	g := NewWithT(t)
	cm := GenerateRBACProxyConfigMap(newGenesisNode("a", "default"))
	g.Expect(cm).NotTo(BeNil())
	g.Expect(cm.Data["config.yaml"]).To(ContainSubstring("apiGroup: sei.io"),
		"field name must match kube-rbac-proxy's authz.ResourceAttributes struct (apiGroup, not group)")
}

func TestGenerateStatefulSet_ProxyImageMissing_Errors(t *testing.T) {
	g := NewWithT(t)
	p := platformtest.Config()
	p.KubeRBACProxyImage = ""
	_, err := GenerateStatefulSet(newGenesisNode("a", "default"), p)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("KUBE_RBAC_PROXY_IMAGE"))
}
