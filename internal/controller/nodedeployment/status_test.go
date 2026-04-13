package nodedeployment

import (
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const testOldHash = "oldhash1234"

func emptyGroup() *seiv1alpha1.SeiNodeDeployment {
	return &seiv1alpha1.SeiNodeDeployment{}
}

func TestComputeGroupPhase_NoNodes(t *testing.T) {
	g := NewWithT(t)
	phase := computeGroupPhase(emptyGroup(), 0, 3, nil)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhasePending))
}

func TestComputeGroupPhase_AllReady(t *testing.T) {
	g := NewWithT(t)
	nodes := makeNodes(3, seiv1alpha1.PhaseRunning)
	phase := computeGroupPhase(emptyGroup(), 3, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseReady))
}

func TestComputeGroupPhase_Initializing(t *testing.T) {
	g := NewWithT(t)
	nodes := []seiv1alpha1.SeiNode{
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseInitializing}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhasePending}},
	}
	phase := computeGroupPhase(emptyGroup(), 1, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseInitializing))
}

func TestComputeGroupPhase_Degraded(t *testing.T) {
	g := NewWithT(t)
	nodes := []seiv1alpha1.SeiNode{
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseFailed}},
	}
	phase := computeGroupPhase(emptyGroup(), 2, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseDegraded))
}

func TestComputeGroupPhase_SomeFailedSomeInitializing(t *testing.T) {
	g := NewWithT(t)
	nodes := []seiv1alpha1.SeiNode{
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseFailed}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseInitializing}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhasePending}},
	}
	phase := computeGroupPhase(emptyGroup(), 0, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseInitializing),
		"should be Initializing when some nodes are still progressing, not Failed")
}

func TestComputeGroupPhase_AllFailed(t *testing.T) {
	g := NewWithT(t)
	nodes := makeNodes(2, seiv1alpha1.PhaseFailed)
	phase := computeGroupPhase(emptyGroup(), 0, 2, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseFailed))
}

func TestComputeGroupPhase_Upgrading(t *testing.T) {
	g := NewWithT(t)
	group := emptyGroup()
	group.Status.Rollout = &seiv1alpha1.RolloutStatus{
		Strategy:          seiv1alpha1.UpdateStrategyBlueGreen,
		TargetHash:        "abc123",
		IncumbentRevision: "1",
		EntrantRevision:   "2",
	}
	setPlanInProgress(group, "Deployment", "deploying")
	nodes := makeNodes(3, seiv1alpha1.PhaseRunning)
	phase := computeGroupPhase(group, 3, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseUpgrading))
}

func TestComputeGroupPhase_PlanInProgress_Genesis(t *testing.T) {
	g := NewWithT(t)
	group := emptyGroup()
	setPlanInProgress(group, "GenesisAssembly", "assembling")
	nodes := makeNodes(3, seiv1alpha1.PhasePending)
	phase := computeGroupPhase(group, 0, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseInitializing))
}

func makeNodes(n int, phase seiv1alpha1.SeiNodePhase) []seiv1alpha1.SeiNode {
	nodes := make([]seiv1alpha1.SeiNode, n)
	for i := range n {
		nodes[i].Status.Phase = phase
	}
	return nodes
}

// --- NetworkingStatus ---

func TestReconcileRolloutStatus_InPlace_AllReady(t *testing.T) {
	g := NewWithT(t)
	group := emptyGroup()
	group.Generation = 2
	group.Status.Rollout = &seiv1alpha1.RolloutStatus{
		Strategy:   seiv1alpha1.UpdateStrategyInPlace,
		TargetHash: "newhash1234",
		StartedAt:  metav1.Now(),
		Nodes: []seiv1alpha1.RolloutNodeStatus{
			{Name: "node-0"},
			{Name: "node-1"},
		},
	}
	group.Status.TemplateHash = testOldHash

	nodes := []seiv1alpha1.SeiNode{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}, Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}, Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}},
	}

	reconcileRolloutStatus(group, nodes)

	g.Expect(group.Status.Rollout).To(BeNil())
	g.Expect(group.Status.TemplateHash).To(Equal("newhash1234"))
	g.Expect(group.Status.ObservedGeneration).To(Equal(int64(2)))

	cond := apimeta.FindStatusCondition(group.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("RolloutComplete"))
}

func TestReconcileRolloutStatus_InPlace_Partial(t *testing.T) {
	g := NewWithT(t)
	group := emptyGroup()
	group.Status.Rollout = &seiv1alpha1.RolloutStatus{
		Strategy:   seiv1alpha1.UpdateStrategyInPlace,
		TargetHash: "newhash1234",
		StartedAt:  metav1.Now(),
		Nodes: []seiv1alpha1.RolloutNodeStatus{
			{Name: "node-0"},
			{Name: "node-1"},
		},
	}
	group.Status.TemplateHash = testOldHash

	nodes := []seiv1alpha1.SeiNode{
		{ObjectMeta: metav1.ObjectMeta{Name: "node-0"}, Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}},
		{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}, Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseInitializing}},
	}

	reconcileRolloutStatus(group, nodes)

	g.Expect(group.Status.Rollout).NotTo(BeNil())
	g.Expect(group.Status.TemplateHash).To(Equal(testOldHash))
	g.Expect(group.Status.Rollout.Nodes[0].Ready).To(BeTrue())
	g.Expect(group.Status.Rollout.Nodes[1].Ready).To(BeFalse())
}

func TestComputeGroupPhase_RolloutInProgress(t *testing.T) {
	g := NewWithT(t)
	group := emptyGroup()
	setCondition(group, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
		"TemplateChanged", "hash changed")
	nodes := makeNodes(3, seiv1alpha1.PhaseRunning)
	phase := computeGroupPhase(group, 3, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseUpgrading))
}

func TestBuildNetworkingStatus_FullMode_DualDomain(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	r := &SeiNodeDeploymentReconciler{
		GatewayDomain:       "prod.platform.sei.io",
		GatewayPublicDomain: "platform.sei.io",
	}
	status := r.buildNetworkingStatus(group)

	g.Expect(status).NotTo(BeNil())
	g.Expect(status.Routes).To(HaveLen(8))

	hostnames := make([]string, len(status.Routes))
	for i, r := range status.Routes {
		hostnames[i] = r.Hostname
	}
	g.Expect(hostnames).To(ContainElements(
		"pacific-1-wave-evm.prod.platform.sei.io",
		"pacific-1-wave-evm.pacific-1.platform.sei.io",
		"pacific-1-wave-rpc.prod.platform.sei.io",
		"pacific-1-wave-rpc.pacific-1.platform.sei.io",
	))

	for _, rs := range status.Routes {
		g.Expect(rs.Protocol).NotTo(BeEmpty())
	}
}

func TestBuildNetworkingStatus_SingleDomain(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	r := &SeiNodeDeploymentReconciler{
		GatewayDomain: "dev.platform.sei.io",
	}
	status := r.buildNetworkingStatus(group)

	g.Expect(status).NotTo(BeNil())
	g.Expect(status.Routes).To(HaveLen(4))
	for _, rs := range status.Routes {
		g.Expect(rs.Hostname).To(HaveSuffix(".dev.platform.sei.io"))
	}
}

func TestBuildNetworkingStatus_ValidatorMode_Nil(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-val", "pacific-1")
	group.Spec.Template.Spec.FullNode = nil
	group.Spec.Template.Spec.Validator = &seiv1alpha1.ValidatorSpec{}

	r := &SeiNodeDeploymentReconciler{
		GatewayDomain:       "prod.platform.sei.io",
		GatewayPublicDomain: "platform.sei.io",
	}
	status := r.buildNetworkingStatus(group)
	g.Expect(status).To(BeNil())
}

func TestBuildNetworkingStatus_ProtocolValues(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	r := &SeiNodeDeploymentReconciler{
		GatewayDomain: "prod.platform.sei.io",
	}
	status := r.buildNetworkingStatus(group)

	protocols := make([]string, len(status.Routes))
	for i, rs := range status.Routes {
		protocols[i] = rs.Protocol
	}
	g.Expect(protocols).To(ConsistOf("evm", "rpc", "rest", "grpc"))
}
