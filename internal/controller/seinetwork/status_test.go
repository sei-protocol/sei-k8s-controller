package seinetwork

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func emptyNetwork() *seiv1alpha1.SeiNetwork {
	return &seiv1alpha1.SeiNetwork{}
}

func TestComputeGroupPhase_NoNodes(t *testing.T) {
	g := NewWithT(t)
	phase := computeGroupPhase(emptyNetwork(), 0, 3, nil)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhasePending))
}

func TestComputeGroupPhase_AllReady(t *testing.T) {
	g := NewWithT(t)
	nodes := makeNodes(3, seiv1alpha1.PhaseRunning)
	phase := computeGroupPhase(emptyNetwork(), 3, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseReady))
}

func TestComputeGroupPhase_Initializing(t *testing.T) {
	g := NewWithT(t)
	nodes := []seiv1alpha1.SeiNode{
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseInitializing}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhasePending}},
	}
	phase := computeGroupPhase(emptyNetwork(), 1, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseInitializing))
}

func TestComputeGroupPhase_Degraded(t *testing.T) {
	g := NewWithT(t)
	nodes := []seiv1alpha1.SeiNode{
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseFailed}},
	}
	phase := computeGroupPhase(emptyNetwork(), 2, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseDegraded))
}

func TestComputeGroupPhase_SomeFailedSomeInitializing(t *testing.T) {
	g := NewWithT(t)
	nodes := []seiv1alpha1.SeiNode{
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseFailed}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseInitializing}},
		{Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhasePending}},
	}
	phase := computeGroupPhase(emptyNetwork(), 0, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseInitializing),
		"should be Initializing when some nodes are still progressing, not Failed")
}

func TestComputeGroupPhase_AllFailed(t *testing.T) {
	g := NewWithT(t)
	nodes := makeNodes(2, seiv1alpha1.PhaseFailed)
	phase := computeGroupPhase(emptyNetwork(), 0, 2, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseFailed))
}

func TestComputeGroupPhase_PlanInProgress_Genesis(t *testing.T) {
	g := NewWithT(t)
	network := emptyNetwork()
	setPlanInProgress(network, "GenesisAssembly", "assembling")
	nodes := makeNodes(3, seiv1alpha1.PhasePending)
	phase := computeGroupPhase(network, 0, 3, nodes)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseInitializing))
}

// A latched CeremonyFailed condition drives GroupPhaseFailed even when the
// child snapshot would otherwise resolve to a non-failed phase (here: no
// children → Pending). This is what keeps a failed genesis ceremony visible
// after updateStatus recomputes the phase.
func TestComputeGroupPhase_CeremonyFailed_OverridesChildDerivedPhase(t *testing.T) {
	g := NewWithT(t)
	network := emptyNetwork()
	setCondition(network, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionFalse,
		"CeremonyFailed", "genesis ceremony plan failed")
	// No children — child-derived logic alone would return Pending.
	phase := computeGroupPhase(network, 0, 3, nil)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseFailed))
}

// Regression for "Failed plan phase overwritten": failPlan records the failure
// on a condition, then both the per-reconcile condition seed
// (seedAlwaysPresentConditions) AND the phase recomputation
// (computeGroupPhase, as run by updateStatus) must preserve it. Before the fix
// failPlan wrote Status.Phase=Degraded directly, which updateStatus clobbered
// back to Pending, masking the failure.
func TestFailedCeremony_SurfacesFailedPhase_AcrossUpdateStatus(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Status.Plan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanFailed}
	setPlanInProgress(network, "Genesis", "assembling")

	r := newPlanTestReconciler(t, network)

	// Reconcile N: plan fails.
	r.failPlan(ctx, network)

	// Reconcile N+1 begins with the condition seed running first.
	r.seedAlwaysPresentConditions(network)
	genesisCond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionGenesisCeremonyComplete)
	g.Expect(genesisCond).NotTo(BeNil())
	g.Expect(genesisCond.Reason).To(Equal("CeremonyFailed"),
		"the seed must not reset a latched CeremonyFailed back to NotStarted")

	// updateStatus recomputes the phase from the (no) children — it must
	// still surface Failed, driven by the latched condition.
	phase := computeGroupPhase(network, 0, network.Spec.Replicas, nil)
	g.Expect(phase).To(Equal(seiv1alpha1.GroupPhaseFailed),
		"a failed genesis ceremony must surface GroupPhaseFailed, not be masked by the child-derived phase")
}

func makeNodes(n int, phase seiv1alpha1.SeiNodePhase) []seiv1alpha1.SeiNode {
	nodes := make([]seiv1alpha1.SeiNode, n)
	for i := range n {
		nodes[i].Status.Phase = phase
	}
	return nodes
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
			network := newTestNetwork(testNetworkName, testGroupNS)
			network.Spec.Paused = tc.paused
			if tc.seedExist != nil {
				network.Status.Conditions = append(network.Status.Conditions, *tc.seedExist)
			}

			r := &SeiNetworkReconciler{Recorder: record.NewFakeRecorder(10)}
			r.setPausedCondition(network)

			cond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionPaused)
			g.Expect(cond).NotTo(BeNil(), "ConditionPaused must be present after setPausedCondition")
			g.Expect(cond.Status).To(Equal(tc.wantStatus))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}
}

func TestSeedAlwaysPresentConditions(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*seiv1alpha1.SeiNetwork)
		condType   string
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "PlanInProgress seeds False/NotStarted on a fresh network",
			mutate:     func(n *seiv1alpha1.SeiNetwork) {},
			condType:   seiv1alpha1.ConditionPlanInProgress,
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonNotStarted,
		},
		{
			// The seed runs before transition paths in the same
			// reconcile, so a True write from startPlan must not be
			// re-seeded on the next pass.
			name: "PlanInProgress=True is preserved",
			mutate: func(n *seiv1alpha1.SeiNetwork) {
				setCondition(n, seiv1alpha1.ConditionPlanInProgress, metav1.ConditionTrue, "PlanStarted", "")
			},
			condType:   seiv1alpha1.ConditionPlanInProgress,
			wantStatus: metav1.ConditionTrue,
			wantReason: "PlanStarted",
		},
		{
			// The seed only fires on absence, so the transition reason
			// from completePlan (False/PlanComplete) stays put.
			name: "PlanInProgress=False/PlanComplete is preserved",
			mutate: func(n *seiv1alpha1.SeiNetwork) {
				setCondition(n, seiv1alpha1.ConditionPlanInProgress, metav1.ConditionFalse, "PlanComplete", "")
			},
			condType:   seiv1alpha1.ConditionPlanInProgress,
			wantStatus: metav1.ConditionFalse,
			wantReason: "PlanComplete",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			network := newTestNetwork(testNetworkName, testGroupNS)
			tc.mutate(network)

			r := &SeiNetworkReconciler{Recorder: record.NewFakeRecorder(10)}
			r.seedAlwaysPresentConditions(network)

			cond := apimeta.FindStatusCondition(network.Status.Conditions, tc.condType)
			g.Expect(cond).NotTo(BeNil(), "%s must be present after seeding", tc.condType)
			g.Expect(cond.Status).To(Equal(tc.wantStatus))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}
}
