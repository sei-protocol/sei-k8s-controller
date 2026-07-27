package noderesource

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	seiconfig "github.com/sei-protocol/sei-config"

	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

// TestGatedSeidContainer_ProbeContract asserts the rendered gated seid
// container satisfies the workflow-hold probe invariant: no liveness probe and
// no startup probe targeting seid's RPC/gRPC port (a held seid answers neither,
// so such a probe would let the kubelet kill the held pod).
func TestGatedSeidContainer_ProbeContract(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")
	c := buildSidecarMainContainer(node, platformtest.Config())

	g.Expect(ValidateGatedSeidProbes(c)).To(Succeed())
	// The startup probe gates on the sidecar healthz via the proxy port and
	// tolerates a long hold.
	g.Expect(c.LivenessProbe).To(BeNil())
	g.Expect(c.StartupProbe).NotTo(BeNil())
	g.Expect(c.StartupProbe.HTTPGet).NotTo(BeNil())
	g.Expect(c.StartupProbe.HTTPGet.Port.IntVal).To(Equal(RBACProxyPort))
}

func TestValidateGatedSeidProbes_RejectsRPCProbes(t *testing.T) {
	g := NewWithT(t)
	base := buildSidecarMainContainer(newGenesisNode("mynet-0", "default"), platformtest.Config())

	// An RPC-based liveness probe is rejected.
	withLiveness := *base.DeepCopy()
	withLiveness.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(seiconfig.PortRPC)},
	}}
	g.Expect(ValidateGatedSeidProbes(withLiveness)).To(HaveOccurred())

	// An RPC-port startup probe (TCP or HTTP) is rejected.
	withTCPStartup := *base.DeepCopy()
	withTCPStartup.StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(seiconfig.PortRPC)},
	}}
	g.Expect(ValidateGatedSeidProbes(withTCPStartup)).To(HaveOccurred())

	withGRPCStartup := *base.DeepCopy()
	withGRPCStartup.StartupProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(seiconfig.PortGRPC)},
	}}
	g.Expect(ValidateGatedSeidProbes(withGRPCStartup)).To(HaveOccurred())
}

// A seed serves only P2P, so its readiness gates on the transport being bound —
// the failure most worth catching, since a loopback-bound seed boots clean and
// accepts nothing.
func TestSeedContainer_ReadinessProbesP2PNotRPC(t *testing.T) {
	g := NewWithT(t)
	c := buildSidecarMainContainer(nodeForRole(roleSeed), platformtest.Config())

	g.Expect(c.ReadinessProbe).NotTo(BeNil())
	g.Expect(c.ReadinessProbe.HTTPGet).To(BeNil(), "a seed serves no RPC to probe over HTTP")
	g.Expect(c.ReadinessProbe.TCPSocket).NotTo(BeNil())
	g.Expect(c.ReadinessProbe.TCPSocket.Port.IntVal).To(Equal(seiconfig.PortP2P))
	g.Expect(ValidateSeedProbes(nodeForRole(roleSeed), c)).To(Succeed())
}

// Chain-following modes keep the /lag_status readiness gate, which reports sync
// distance rather than mere liveness.
func TestNonSeedContainer_ReadinessProbesLagStatus(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("mynet-0", "default")
	c := buildSidecarMainContainer(node, platformtest.Config())

	g.Expect(c.ReadinessProbe.HTTPGet).NotTo(BeNil())
	g.Expect(c.ReadinessProbe.HTTPGet.Path).To(Equal("/lag_status"))
	g.Expect(c.ReadinessProbe.HTTPGet.Port.IntVal).To(Equal(seiconfig.PortRPC))
	// The seed guard is a no-op for modes that do bind RPC.
	g.Expect(ValidateSeedProbes(node, c)).To(Succeed())
}

// A permanently-failing readiness probe would hold a seed out of Service
// endpoints and any NLB target group fronting it, so the render fails closed.
func TestValidateSeedProbes_RejectsRPCProbes(t *testing.T) {
	seed := nodeForRole(roleSeed)
	base := buildSidecarMainContainer(seed, platformtest.Config())

	rpcProbe := func(port int32) *corev1.Probe {
		return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
		}}
	}

	cases := map[string]func(*corev1.Container){
		"readiness on RPC":  func(c *corev1.Container) { c.ReadinessProbe = rpcProbe(seiconfig.PortRPC) },
		"liveness on RPC":   func(c *corev1.Container) { c.LivenessProbe = rpcProbe(seiconfig.PortRPC) },
		"startup on gRPC":   func(c *corev1.Container) { c.StartupProbe = rpcProbe(seiconfig.PortGRPC) },
		"readiness on gRPC": func(c *corev1.Container) { c.ReadinessProbe = rpcProbe(seiconfig.PortGRPC) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			c := base
			mutate(&c)
			g.Expect(ValidateSeedProbes(seed, c)).NotTo(Succeed())
		})
	}
}
