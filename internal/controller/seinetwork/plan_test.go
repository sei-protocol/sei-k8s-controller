package seinetwork

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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

// A founding validator deleted while the ceremony plan is active must not
// wedge the network: reconcileSeiNodes defers creates under PlanInProgress
// and the ceremony's tasks retry by name against the missing node forever.
// reconcilePlan abandons the plan and deletes the survivors instead — their
// fetched genesis no longer matches what the replacement's gentx would
// assemble — so PlanInProgress drops to False, the whole set is recreated, and
// GenesisCeremonyComplete records ValidatorLost so the planner rebuilds the
// ceremony once the set is whole again.
func TestReconcilePlan_ValidatorLostMidCeremony_AbandonsPlan(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Status.Plan = &seiv1alpha1.TaskPlan{ID: "ceremony", Phase: seiv1alpha1.TaskPlanActive}
	network.Status.IncumbentNodes = []string{testNode0, "genesis-net-1"}
	setPlanInProgress(network, "PlanStarted", "Plan execution started")

	survivor0 := generateSeiNode(network, 0)
	survivor1 := generateSeiNode(network, 1)
	for _, s := range []*seiv1alpha1.SeiNode{survivor0, survivor1} {
		g.Expect(controllerutil.SetControllerReference(network, s, newPlanTestScheme(t))).To(Succeed())
	}

	r := newPlanTestReconciler(t, network, survivor0, survivor1)
	result, err := r.reconcilePlan(ctx, network)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeNumerically(">", 0))

	for _, s := range []*seiv1alpha1.SeiNode{survivor0, survivor1} {
		err := r.Get(ctx, client.ObjectKeyFromObject(s), &seiv1alpha1.SeiNode{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "surviving founding child %s should be deleted", s.Name)
	}

	g.Expect(network.Status.Plan).To(BeNil())

	genesisCond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionGenesisCeremonyComplete)
	g.Expect(genesisCond).NotTo(BeNil())
	g.Expect(genesisCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(genesisCond.Reason).To(Equal(ReasonValidatorLost))

	planCond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionPlanInProgress)
	g.Expect(planCond).NotTo(BeNil())
	g.Expect(planCond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(planCond.Reason).To(Equal(ReasonValidatorLost))

	// The seed must keep ValidatorLost, like CeremonyFailed, until the rebuilt
	// plan starts — resetting to NotStarted would erase why the plan vanished.
	r.setGenesisCeremonyCondition(network)
	genesisCond = apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionGenesisCeremonyComplete)
	g.Expect(genesisCond.Reason).To(Equal(ReasonValidatorLost))
}

// The full founding set under an active plan is the normal ceremony; the
// plan is driven, not abandoned.
func TestReconcilePlan_FullSetMidCeremony_KeepsPlan(t *testing.T) {
	g := NewWithT(t)

	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Status.Plan = &seiv1alpha1.TaskPlan{ID: "ceremony", Phase: seiv1alpha1.TaskPlanActive}
	network.Status.IncumbentNodes = []string{testNode0, "genesis-net-1", "genesis-net-2"}

	g.Expect(validatorLost(network)).To(BeFalse())
}

// A child held in Terminating by its finalizer is already lost to the
// ceremony: it is not an incumbent, so the loss is detected as soon as the
// delete lands rather than once the finalizer releases, and no ceremony is
// built over a node that is on its way out.
func TestPopulateIncumbentNodes_ExcludesTerminatingChildren(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testNetworkName, testGroupNS)
	live := generateSeiNode(network, 0)
	terminating := generateSeiNode(network, 1)
	terminating.Finalizers = []string{"sei.io/test-hold"}
	for _, s := range []*seiv1alpha1.SeiNode{live, terminating} {
		g.Expect(controllerutil.SetControllerReference(network, s, newPlanTestScheme(t))).To(Succeed())
	}

	r := newPlanTestReconciler(t, network, live, terminating)
	g.Expect(r.Delete(ctx, terminating)).To(Succeed())

	g.Expect(r.populateIncumbentNodes(ctx, network)).To(Succeed())
	g.Expect(network.Status.IncumbentNodes).To(ConsistOf(live.Name))
}
