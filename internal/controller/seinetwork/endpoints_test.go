package seinetwork

import (
	"testing"

	. "github.com/onsi/gomega"
	seiconfig "github.com/sei-protocol/sei-config"

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

func perPodServiceStatus(name string) seiv1alpha1.PerPodServiceStatus {
	return seiv1alpha1.PerPodServiceStatus{
		Name:      name,
		Namespace: testNamespace,
		Ports: seiv1alpha1.PerPodServicePorts{
			EvmHttp: seiconfig.PortEVMHTTP,
			EvmWs:   seiconfig.PortEVMWS,
		},
	}
}

func TestComposeEndpoints_NilWhenStatusEmpty(t *testing.T) {
	g := NewWithT(t)
	got := composeEndpoints(&seiv1alpha1.SeiNetwork{})
	g.Expect(got).To(BeNil())
}

func TestComposeEndpoints_TendermintScalarsFromInternalService(t *testing.T) {
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNetwork{}
	group.Status.InternalService = internalServiceStatus()

	got := composeEndpoints(group)

	g.Expect(got).NotTo(BeNil())
	g.Expect(got.TendermintRpc).To(Equal("http://pacific-1-wave-internal.pacific-1.svc:26657"))
	g.Expect(got.TendermintRest).To(Equal("http://pacific-1-wave-internal.pacific-1.svc:1317"))
	g.Expect(got.Nodes).To(BeEmpty())
}

func TestComposeEndpoints_NodesPerPodInOrder(t *testing.T) {
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNetwork{}
	group.Status.InternalService = internalServiceStatus()
	group.Status.PerPodServices = []seiv1alpha1.PerPodServiceStatus{
		perPodServiceStatus("pacific-1-wave-0"),
		perPodServiceStatus("pacific-1-wave-1"),
		perPodServiceStatus("pacific-1-wave-2"),
	}

	got := composeEndpoints(group)

	g.Expect(got).NotTo(BeNil())
	g.Expect(got.Nodes).To(Equal([]seiv1alpha1.NodeEndpoint{
		{
			Name:       "pacific-1-wave-0",
			EvmJsonRpc: "http://pacific-1-wave-0.pacific-1.svc:8545",
			EvmWs:      "ws://pacific-1-wave-0.pacific-1.svc:8546",
		},
		{
			Name:       "pacific-1-wave-1",
			EvmJsonRpc: "http://pacific-1-wave-1.pacific-1.svc:8545",
			EvmWs:      "ws://pacific-1-wave-1.pacific-1.svc:8546",
		},
		{
			Name:       "pacific-1-wave-2",
			EvmJsonRpc: "http://pacific-1-wave-2.pacific-1.svc:8545",
			EvmWs:      "ws://pacific-1-wave-2.pacific-1.svc:8546",
		},
	}))
}

func TestComposeEndpoints_NodesOrderMirrorsStatus(t *testing.T) {
	// Inputs in non-sorted order; composeEndpoints must preserve it (no re-sort).
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNetwork{}
	group.Status.InternalService = internalServiceStatus()
	group.Status.PerPodServices = []seiv1alpha1.PerPodServiceStatus{
		perPodServiceStatus("pacific-1-wave-2"),
		perPodServiceStatus("pacific-1-wave-0"),
		perPodServiceStatus("pacific-1-wave-5"),
	}

	got := composeEndpoints(group)

	names := []string{got.Nodes[0].Name, got.Nodes[1].Name, got.Nodes[2].Name}
	g.Expect(names).To(Equal([]string{"pacific-1-wave-2", "pacific-1-wave-0", "pacific-1-wave-5"}))
}

func TestComposeEndpoints_AggregateOnlyBeforePerPodObserved(t *testing.T) {
	// Transient early-reconcile state: internal Service applied before any per-pod Service.
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNetwork{}
	group.Status.InternalService = internalServiceStatus()

	got := composeEndpoints(group)

	g.Expect(got).NotTo(BeNil())
	g.Expect(got.TendermintRpc).NotTo(BeEmpty())
	g.Expect(got.TendermintRest).NotTo(BeEmpty())
	g.Expect(got.Nodes).To(BeEmpty())
}

func TestComposeEndpoints_PerPodOnlyBeforeInternalObserved(t *testing.T) {
	// Inverse transient: per-pod Services applied before internal Service.
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNetwork{}
	group.Status.PerPodServices = []seiv1alpha1.PerPodServiceStatus{
		perPodServiceStatus("pacific-1-wave-0"),
	}

	got := composeEndpoints(group)

	g.Expect(got).NotTo(BeNil())
	g.Expect(got.TendermintRpc).To(BeEmpty())
	g.Expect(got.TendermintRest).To(BeEmpty())
	g.Expect(got.Nodes).To(HaveLen(1))
	g.Expect(got.Nodes[0].EvmJsonRpc).To(Equal("http://pacific-1-wave-0.pacific-1.svc:8545"))
	g.Expect(got.Nodes[0].EvmWs).To(Equal("ws://pacific-1-wave-0.pacific-1.svc:8546"))
}
