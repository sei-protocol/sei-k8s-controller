package nodedeployment

import (
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

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
		TargetHash: "abc123",
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

func TestSetPausedCondition(t *testing.T) {
	cases := []struct {
		name       string
		paused     bool
		seedExist  *metav1.Condition
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "spec.paused=true writes True/Paused",
			paused:     true,
			wantStatus: metav1.ConditionTrue,
			wantReason: "Paused",
		},
		{
			name:       "spec.paused=false writes False/NotPaused",
			paused:     false,
			wantStatus: metav1.ConditionFalse,
			wantReason: "NotPaused",
		},
		{
			// setPausedCondition is fully derived from spec — a stale
			// True flips back to False once spec.paused clears.
			name:   "stale True flips to False when spec clears",
			paused: false,
			seedExist: &metav1.Condition{
				Type:   seiv1alpha1.ConditionPaused,
				Status: metav1.ConditionTrue,
				Reason: "Paused",
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "NotPaused",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			group := newTestGroup("archive-rpc", "sei")
			group.Spec.Paused = tc.paused
			if tc.seedExist != nil {
				group.Status.Conditions = append(group.Status.Conditions, *tc.seedExist)
			}

			r := &SeiNodeDeploymentReconciler{Recorder: record.NewFakeRecorder(10)}
			r.setPausedCondition(group)

			cond := apimeta.FindStatusCondition(group.Status.Conditions, seiv1alpha1.ConditionPaused)
			g.Expect(cond).NotTo(BeNil(), "ConditionPaused must be present after setPausedCondition")
			g.Expect(cond.Status).To(Equal(tc.wantStatus))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}
}

func TestSeedAlwaysPresentConditions(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*seiv1alpha1.SeiNodeDeployment)
		condType   string
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "PlanInProgress seeds False/NotStarted on a fresh SND",
			mutate:     func(g *seiv1alpha1.SeiNodeDeployment) {},
			condType:   seiv1alpha1.ConditionPlanInProgress,
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonNotStarted,
		},
		{
			name:       "RolloutInProgress seeds False/NotStarted on a fresh SND",
			mutate:     func(g *seiv1alpha1.SeiNodeDeployment) {},
			condType:   seiv1alpha1.ConditionRolloutInProgress,
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonNotStarted,
		},
		{
			// The seed runs before transition paths in the same
			// reconcile, so a True write from startPlan must not be
			// re-seeded on the next pass.
			name: "PlanInProgress=True is preserved",
			mutate: func(g *seiv1alpha1.SeiNodeDeployment) {
				setCondition(g, seiv1alpha1.ConditionPlanInProgress, metav1.ConditionTrue, "PlanStarted", "")
			},
			condType:   seiv1alpha1.ConditionPlanInProgress,
			wantStatus: metav1.ConditionTrue,
			wantReason: "PlanStarted",
		},
		{
			// The seed only fires on absence, so the transition reason
			// from completePlan (False/PlanComplete) stays put.
			name: "PlanInProgress=False/PlanComplete is preserved",
			mutate: func(g *seiv1alpha1.SeiNodeDeployment) {
				setCondition(g, seiv1alpha1.ConditionPlanInProgress, metav1.ConditionFalse, "PlanComplete", "")
			},
			condType:   seiv1alpha1.ConditionPlanInProgress,
			wantStatus: metav1.ConditionFalse,
			wantReason: "PlanComplete",
		},
		{
			name: "RolloutInProgress=False/RolloutComplete is preserved",
			mutate: func(g *seiv1alpha1.SeiNodeDeployment) {
				setCondition(g, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionFalse, "RolloutComplete", "")
			},
			condType:   seiv1alpha1.ConditionRolloutInProgress,
			wantStatus: metav1.ConditionFalse,
			wantReason: "RolloutComplete",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			group := newTestGroup("archive-rpc", "sei")
			tc.mutate(group)

			r := &SeiNodeDeploymentReconciler{Recorder: record.NewFakeRecorder(10)}
			r.seedAlwaysPresentConditions(group)

			cond := apimeta.FindStatusCondition(group.Status.Conditions, tc.condType)
			g.Expect(cond).NotTo(BeNil(), "%s must be present after seeding", tc.condType)
			g.Expect(cond.Status).To(Equal(tc.wantStatus))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}
}
