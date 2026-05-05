package nodedeployment

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
	got := composeEndpoints(&seiv1alpha1.SeiNodeDeployment{})
	g.Expect(got).To(BeNil())
}

func TestComposeEndpoints_EvmJsonRpcAggregateThenPerPod(t *testing.T) {
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNodeDeployment{}
	group.Status.InternalService = internalServiceStatus()
	group.Status.PerPodServices = []seiv1alpha1.PerPodServiceStatus{
		perPodServiceStatus("pacific-1-wave-0"),
		perPodServiceStatus("pacific-1-wave-1"),
		perPodServiceStatus("pacific-1-wave-2"),
	}

	got := composeEndpoints(group)

	g.Expect(got).NotTo(BeNil())
	// 1 aggregate + N per-pod
	g.Expect(got.EvmJsonRpc).To(HaveLen(4))
	g.Expect(got.EvmJsonRpc[0]).To(Equal("http://pacific-1-wave-internal.pacific-1.svc:8545"))
	g.Expect(got.EvmJsonRpc[1]).To(Equal("http://pacific-1-wave-0.pacific-1.svc:8545"))
	g.Expect(got.EvmJsonRpc[2]).To(Equal("http://pacific-1-wave-1.pacific-1.svc:8545"))
	g.Expect(got.EvmJsonRpc[3]).To(Equal("http://pacific-1-wave-2.pacific-1.svc:8545"))
}

func TestComposeEndpoints_EvmWsPerPodOnlyInOrder(t *testing.T) {
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNodeDeployment{}
	group.Status.InternalService = internalServiceStatus()
	group.Status.PerPodServices = []seiv1alpha1.PerPodServiceStatus{
		perPodServiceStatus("pacific-1-wave-0"),
		perPodServiceStatus("pacific-1-wave-1"),
		perPodServiceStatus("pacific-1-wave-2"),
	}

	got := composeEndpoints(group)

	g.Expect(got).NotTo(BeNil())
	// Per-pod-only (no aggregate): exactly N entries.
	g.Expect(got.EvmWs).To(HaveLen(3))
	g.Expect(got.EvmWs[0]).To(Equal("ws://pacific-1-wave-0.pacific-1.svc:8546"))
	g.Expect(got.EvmWs[1]).To(Equal("ws://pacific-1-wave-1.pacific-1.svc:8546"))
	g.Expect(got.EvmWs[2]).To(Equal("ws://pacific-1-wave-2.pacific-1.svc:8546"))
}

func TestComposeEndpoints_TendermintAggregateOnlyToday(t *testing.T) {
	// PerPodServicePorts has no Rpc/Rest fields, so per-pod entries cannot be appended.
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNodeDeployment{}
	group.Status.InternalService = internalServiceStatus()
	group.Status.PerPodServices = []seiv1alpha1.PerPodServiceStatus{
		perPodServiceStatus("pacific-1-wave-0"),
		perPodServiceStatus("pacific-1-wave-1"),
	}

	got := composeEndpoints(group)

	g.Expect(got).NotTo(BeNil())
	g.Expect(got.TendermintRpc).To(ConsistOf("http://pacific-1-wave-internal.pacific-1.svc:26657"))
	g.Expect(got.TendermintRest).To(ConsistOf("http://pacific-1-wave-internal.pacific-1.svc:1317"))
}

func TestComposeEndpoints_PerPodOrderMirrorsStatus(t *testing.T) {
	// Inputs in non-sorted order; composeEndpoints must preserve it (no re-sort).
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNodeDeployment{}
	group.Status.InternalService = internalServiceStatus()
	group.Status.PerPodServices = []seiv1alpha1.PerPodServiceStatus{
		perPodServiceStatus("pacific-1-wave-2"),
		perPodServiceStatus("pacific-1-wave-0"),
		perPodServiceStatus("pacific-1-wave-5"),
	}

	got := composeEndpoints(group)

	g.Expect(got.EvmJsonRpc).To(Equal([]string{
		"http://pacific-1-wave-internal.pacific-1.svc:8545",
		"http://pacific-1-wave-2.pacific-1.svc:8545",
		"http://pacific-1-wave-0.pacific-1.svc:8545",
		"http://pacific-1-wave-5.pacific-1.svc:8545",
	}))
	g.Expect(got.EvmWs).To(Equal([]string{
		"ws://pacific-1-wave-2.pacific-1.svc:8546",
		"ws://pacific-1-wave-0.pacific-1.svc:8546",
		"ws://pacific-1-wave-5.pacific-1.svc:8546",
	}))
}

func TestComposeEndpoints_AggregateOnlyBeforePerPodObserved(t *testing.T) {
	// Transient early-reconcile state: internal Service applied before any per-pod Service.
	g := NewWithT(t)
	group := &seiv1alpha1.SeiNodeDeployment{}
	group.Status.InternalService = internalServiceStatus()

	got := composeEndpoints(group)

	g.Expect(got).NotTo(BeNil())
	g.Expect(got.EvmJsonRpc).To(ConsistOf("http://pacific-1-wave-internal.pacific-1.svc:8545"))
	g.Expect(got.EvmWs).To(BeEmpty())
	g.Expect(got.TendermintRpc).To(HaveLen(1))
	g.Expect(got.TendermintRest).To(HaveLen(1))
}
