//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// TestNetwork_AlwaysPresentConditionsSeededOnFirstReconcile asserts the
// always-present conditions land on a fresh SeiNetwork:
//   - PlanInProgress=False/NotStarted
//   - RolloutInProgress present as a derived projection (False/AllUpToDate
//     once updateStatus computes it from the child snapshot)
//   - GenesisCeremonyComplete present (reason races NotStarted→InProgress as
//     the planner schedules the ceremony immediately, so assert presence only)
func TestNetwork_AlwaysPresentConditionsSeededOnFirstReconcile(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	network := fixtures.NewNetwork(ns, "fresh-network")
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())

	waitForStatus(t, client.ObjectKeyFromObject(network), func(n *seiv1alpha1.SeiNetwork) bool {
		return findCondition(n, seiv1alpha1.ConditionPlanInProgress) != nil &&
			findCondition(n, seiv1alpha1.ConditionRolloutInProgress) != nil &&
			findCondition(n, seiv1alpha1.ConditionGenesisCeremonyComplete) != nil
	}, "PlanInProgress, RolloutInProgress, GenesisCeremonyComplete must all be present after first reconcile")

	// The derived RolloutInProgress settles to False/AllUpToDate once the
	// network reaches steady state (every child reports spec.image). It may
	// transiently read True/ImageRolling while children are mid-genesis, so
	// wait for the steady value rather than sampling immediately.
	waitForStatus(t, client.ObjectKeyFromObject(network), func(n *seiv1alpha1.SeiNetwork) bool {
		c := findCondition(n, seiv1alpha1.ConditionRolloutInProgress)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == reasonAllUpToDate
	}, "RolloutInProgress settles to False/AllUpToDate at steady state")
}

func findCondition(n *seiv1alpha1.SeiNetwork, condType string) *metav1.Condition {
	return apimeta.FindStatusCondition(n.Status.Conditions, condType)
}
