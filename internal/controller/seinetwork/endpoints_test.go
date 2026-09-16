package seinetwork

import (
	"testing"

	. "github.com/onsi/gomega"
	seiconfig "github.com/sei-protocol/sei-config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	testNamespace       = "pacific-1"
	testInternalSvcName = "pacific-1-wave-internal"
)

func internalServiceStatus() *seiv1alpha1.InternalServiceStatus {
	return &seiv1alpha1.InternalServiceStatus{
		Name:      testInternalSvcName,
		Namespace: testNamespace,
		Ports: seiv1alpha1.InternalServicePorts{
			Rpc:     seiconfig.PortRPC,
			EvmHttp: seiconfig.PortEVMHTTP,
			Rest:    seiconfig.PortREST,
		},
	}
}

func perPodService(name string) seiv1alpha1.PerPodServiceStatus {
	return seiv1alpha1.PerPodServiceStatus{
		Name:      name,
		Namespace: testNamespace,
		Ports:     seiv1alpha1.PerPodServicePorts{EvmHttp: seiconfig.PortEVMHTTP, EvmWs: seiconfig.PortEVMWS},
	}
}

func evmOnlyNetwork() *seiv1alpha1.SeiNetwork {
	group := &seiv1alpha1.SeiNetwork{}
	group.Spec.ExecutionEngine = &seiv1alpha1.ExecutionEngineSpec{Mode: seiv1alpha1.ExecutionEngineEvmOnly}
	return group
}

// servingChild is an EVM-only child SeiNode whose listener answers, as the
// node controller publishes it: JSON-RPC only.
func servingChild(name string) seiv1alpha1.SeiNode {
	child := childWithoutEndpoint(name)
	child.Status.Endpoint = &seiv1alpha1.NodeEndpointStatus{
		EvmJsonRpc: "http://" + name + "." + testNamespace + ".svc:8545",
	}
	return child
}

func childWithoutEndpoint(name string) seiv1alpha1.SeiNode {
	return seiv1alpha1.SeiNode{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
}

func TestComposeEndpoints_NilWhenStatusEmpty(t *testing.T) {
	g := NewWithT(t)
	got := composeEndpoints(&seiv1alpha1.SeiNetwork{}, nil)
	g.Expect(got).To(BeNil())
}

func TestComposeEndpoints_TendermintScalarsFromInternalService(t *testing.T) {
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNetwork{}
	group.Status.InternalService = internalServiceStatus()

	got := composeEndpoints(group, nil)

	g.Expect(got).NotTo(BeNil())
	g.Expect(got.TendermintRpc).To(Equal("http://pacific-1-wave-internal.pacific-1.svc:26657"))
	g.Expect(got.TendermintRest).To(Equal("http://pacific-1-wave-internal.pacific-1.svc:1317"))
	g.Expect(got.Nodes).To(BeEmpty())
}

// A Default-engine network publishes one entry per per-pod Service, in
// inventory order, regardless of what its children publish.
func TestComposeEndpoints_DefaultEngineNodesFromPerPodServices(t *testing.T) {
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNetwork{}
	group.Status.InternalService = internalServiceStatus()
	group.Status.PerPodServices = []seiv1alpha1.PerPodServiceStatus{
		perPodService("pacific-1-wave-2"),
		perPodService("pacific-1-wave-0"),
	}

	got := composeEndpoints(group, []seiv1alpha1.SeiNode{childWithoutEndpoint("pacific-1-wave-0")})

	g.Expect(got).NotTo(BeNil())
	g.Expect(got.Nodes).To(Equal([]seiv1alpha1.NodeEndpoint{
		{
			Name:       "pacific-1-wave-2",
			EvmJsonRpc: "http://pacific-1-wave-2.pacific-1.svc:8545",
			EvmWs:      "ws://pacific-1-wave-2.pacific-1.svc:8546",
		},
		{
			Name:       "pacific-1-wave-0",
			EvmJsonRpc: "http://pacific-1-wave-0.pacific-1.svc:8545",
			EvmWs:      "ws://pacific-1-wave-0.pacific-1.svc:8546",
		},
	}))
}

func TestComposeEndpoints_DefaultEngineNodesOnlyWhenNoInternalService(t *testing.T) {
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNetwork{}
	group.Status.PerPodServices = []seiv1alpha1.PerPodServiceStatus{perPodService("pacific-1-wave-0")}

	got := composeEndpoints(group, nil)

	g.Expect(got).NotTo(BeNil())
	g.Expect(got.TendermintRpc).To(BeEmpty())
	g.Expect(got.Nodes).To(HaveLen(1))
}

func TestComposeEndpoints_EvmOnlyNodesMirrorChildEndpointsInOrder(t *testing.T) {
	// Inputs in non-sorted order; composeEndpoints must preserve it (no re-sort).
	g := NewWithT(t)
	children := []seiv1alpha1.SeiNode{
		servingChild("pacific-1-wave-2"),
		servingChild("pacific-1-wave-0"),
		servingChild("pacific-1-wave-5"),
	}

	got := composeEndpoints(evmOnlyNetwork(), children)

	g.Expect(got).NotTo(BeNil())
	g.Expect(got.Nodes).To(Equal([]seiv1alpha1.NodeEndpoint{
		{Name: "pacific-1-wave-2", EvmJsonRpc: "http://pacific-1-wave-2.pacific-1.svc:8545"},
		{Name: "pacific-1-wave-0", EvmJsonRpc: "http://pacific-1-wave-0.pacific-1.svc:8545"},
		{Name: "pacific-1-wave-5", EvmJsonRpc: "http://pacific-1-wave-5.pacific-1.svc:8545"},
	}))
}

// An EVM-only child whose listener is not serving publishes no endpoint and
// gets no entry, even though its per-pod Service exists. The Service is
// inventory, not a serving signal.
func TestComposeEndpoints_EvmOnlyOmitsChildrenWithoutEndpoint(t *testing.T) {
	g := NewWithT(t)
	group := evmOnlyNetwork()
	group.Status.PerPodServices = []seiv1alpha1.PerPodServiceStatus{
		perPodService("pacific-1-wave-0"),
		perPodService("pacific-1-wave-1"),
		perPodService("pacific-1-wave-2"),
	}
	empty := childWithoutEndpoint("pacific-1-wave-2")
	empty.Status.Endpoint = &seiv1alpha1.NodeEndpointStatus{}
	children := []seiv1alpha1.SeiNode{
		servingChild("pacific-1-wave-0"),
		childWithoutEndpoint("pacific-1-wave-1"),
		empty,
	}

	got := composeEndpoints(group, children)

	g.Expect(got).NotTo(BeNil())
	g.Expect(got.Nodes).To(HaveLen(1))
	g.Expect(got.Nodes[0].Name).To(Equal("pacific-1-wave-0"))
}

// An EvmOnly network's children run with CometBFT RPC and REST disabled, so
// the InternalService ports are dead and the aggregate scalars stay empty.
func TestComposeEndpoints_EvmOnlyNetworkOmitsTendermintScalars(t *testing.T) {
	g := NewWithT(t)
	group := evmOnlyNetwork()
	group.Status.InternalService = internalServiceStatus()

	g.Expect(composeEndpoints(group, nil)).To(BeNil(), "no serving child, no dead aggregate URL")

	got := composeEndpoints(group, []seiv1alpha1.SeiNode{servingChild("pacific-1-wave-0")})
	g.Expect(got).NotTo(BeNil())
	g.Expect(got.TendermintRpc).To(BeEmpty())
	g.Expect(got.TendermintRest).To(BeEmpty())
	g.Expect(got.Nodes).To(HaveLen(1))

	legacy := &seiv1alpha1.SeiNetwork{}
	legacy.Spec.Consensus = &seiv1alpha1.NetworkConsensusSpec{ConsensusSpec: seiv1alpha1.ConsensusSpec{
		Engine:  seiv1alpha1.ConsensusEngineAutobahn,
		EvmOnly: true, //nolint:staticcheck // deliberately exercising the deprecated field's compatibility path
	}}
	legacy.Status.InternalService = internalServiceStatus()
	g.Expect(composeEndpoints(legacy, nil)).To(BeNil(), "deprecated bool resolves to the same gate")
}

func TestComposeEndpoints_EvmOnlyNilWhenOnlyServicesObserved(t *testing.T) {
	// Per-pod Services exist but no child has reached a serving endpoint yet.
	g := NewWithT(t)
	group := evmOnlyNetwork()
	group.Status.PerPodServices = []seiv1alpha1.PerPodServiceStatus{perPodService("pacific-1-wave-0")}

	got := composeEndpoints(group, []seiv1alpha1.SeiNode{childWithoutEndpoint("pacific-1-wave-0")})

	g.Expect(got).To(BeNil())
}
