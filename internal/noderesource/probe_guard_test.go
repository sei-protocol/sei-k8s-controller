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
