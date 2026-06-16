package seinetwork

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// Every SeiNetwork runs exactly one network-level plan — the genesis
// ceremony — so a completing plan latches GenesisCeremonyComplete=True.
func TestCompletePlan_GenesisCeremony_LatchesComplete(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Status.Plan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}
	setPlanInProgress(network, "Genesis", "assembling")

	r := newPlanTestReconciler(t, network)
	r.completePlan(ctx, network)

	cond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionGenesisCeremonyComplete)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("Complete"))
	g.Expect(network.Status.Plan).To(BeNil())
}

// A failed ceremony plan latches GenesisCeremonyComplete=False/CeremonyFailed,
// clears the plan, and drops PlanInProgress to False. The phase is NOT written
// here — it is derived from the condition by computeGroupPhase (see
// TestFailedCeremony_SurfacesFailedPhase_AcrossUpdateStatus) so it survives the
// per-reconcile phase recomputation.
func TestFailPlan_LatchesCeremonyFailedAndClearsPlan(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Status.Plan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanFailed}
	setPlanInProgress(network, "Genesis", "assembling")

	r := newPlanTestReconciler(t, network)

	r.failPlan(ctx, network)

	g.Expect(network.Status.Plan).To(BeNil())

	genesisCond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionGenesisCeremonyComplete)
	g.Expect(genesisCond).NotTo(BeNil())
	g.Expect(genesisCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(genesisCond.Reason).To(Equal("CeremonyFailed"))

	planCond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionPlanInProgress)
	g.Expect(planCond).NotTo(BeNil())
	g.Expect(planCond.Status).To(Equal(metav1.ConditionFalse))
}
