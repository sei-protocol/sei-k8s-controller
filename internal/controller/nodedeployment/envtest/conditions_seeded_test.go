//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest/fixtures"
)

// TestSND_AlwaysPresentConditionsSeededOnFirstReconcile asserts the
// three always-present conditions land on a fresh SND with their
// not-started state values:
//   - PlanInProgress=False/NotStarted
//   - RolloutInProgress=False/NotStarted
//   - GenesisCeremonyComplete=False/NotApplicable (spec.genesis unset)
func TestSND_AlwaysPresentConditionsSeededOnFirstReconcile(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "fresh-snd")
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	waitForStatus(t, client.ObjectKeyFromObject(snd), func(s *seiv1alpha1.SeiNodeDeployment) bool {
		return findCondition(s, seiv1alpha1.ConditionPlanInProgress) != nil &&
			findCondition(s, seiv1alpha1.ConditionRolloutInProgress) != nil &&
			findCondition(s, seiv1alpha1.ConditionGenesisCeremonyComplete) != nil
	}, "PlanInProgress, RolloutInProgress, GenesisCeremonyComplete must all be present after first reconcile")

	cur := &seiv1alpha1.SeiNodeDeployment{}
	g.Expect(testCli.Get(testCtx, client.ObjectKeyFromObject(snd), cur)).To(Succeed())

	plan := findCondition(cur, seiv1alpha1.ConditionPlanInProgress)
	g.Expect(plan.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(plan.Reason).To(Equal("NotStarted"))

	rollout := findCondition(cur, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(rollout.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(rollout.Reason).To(Equal("NotStarted"))

	genesis := findCondition(cur, seiv1alpha1.ConditionGenesisCeremonyComplete)
	g.Expect(genesis.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(genesis.Reason).To(Equal("NotApplicable"))
}

func findCondition(s *seiv1alpha1.SeiNodeDeployment, condType string) *metav1.Condition {
	return apimeta.FindStatusCondition(s.Status.Conditions, condType)
}
