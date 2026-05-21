//go:build envtest

package envtest_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest/fixtures"
)

// TestSND_Paused_PropagatesAndBlocksOrchestration asserts:
//  1. spec.paused=true sets ConditionPaused=True and Status.Phase=Paused.
//  2. Paused state propagates to every owned child SeiNode.
//  3. A template change applied while paused does not start a rollout
//     and does not mutate children's spec.
//  4. Clearing spec.paused propagates back to children and the deferred
//     template change rolls forward to convergence.
func TestSND_Paused_PropagatesAndBlocksOrchestration(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "paused-snd", fixtures.WithReplicas(2))
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())
	key := client.ObjectKeyFromObject(snd)

	// 1. Initial settle. Both children exist and the SND has
	//    ConditionPaused=False/NotPaused seeded.
	waitForStatus(t, key, func(s *seiv1alpha1.SeiNodeDeployment) bool {
		return len(listChildren(t, s)) == 2 &&
			pausedCond(s) != nil && pausedCond(s).Status == metav1.ConditionFalse
	}, "two children exist and ConditionPaused=False is seeded on a fresh SND")

	// 2. Pause the SND. ConditionPaused flips True, Status.Phase moves
	//    to Paused, and children pick up Spec.Paused=true on the next
	//    reconcile.
	cur := getSND(t, key)
	patch := client.MergeFrom(cur.DeepCopy())
	cur.Spec.Paused = true
	g.Expect(testCli.Patch(testCtx, cur, patch)).To(Succeed())

	waitForStatus(t, key, func(s *seiv1alpha1.SeiNodeDeployment) bool {
		c := pausedCond(s)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == "Paused" &&
			s.Status.Phase == seiv1alpha1.GroupPhasePaused
	}, "ConditionPaused must transition to True/Paused and Status.Phase to Paused")

	waitFor(t, func() bool {
		children := listChildren(t, getSND(t, key))
		if len(children) == 0 {
			return false
		}
		for i := range children {
			if !children[i].Spec.Paused {
				return false
			}
		}
		return true
	}, "every child SeiNode must have Spec.Paused=true after SND pause")

	// 3. Template change is observed but blocked. Patch v1→v2 while
	//    paused and assert no rollout fires for 3s. The SND's
	//    TemplateHash isn't rolled forward, Status.Rollout stays nil,
	//    and child Spec.Image stays at the original.
	cur = getSND(t, key)
	patchSNDImage(t, cur, "ghcr.io/sei-protocol/seid:v2.0.0")

	g.Consistently(func(g Gomega) {
		s := getSND(t, key)
		g.Expect(s.Status.Rollout).To(BeNil(), "Status.Rollout must stay nil while paused")
		g.Expect(condTrue(s, seiv1alpha1.ConditionRolloutInProgress)).To(BeFalse(),
			"ConditionRolloutInProgress must not be True while paused")
		g.Expect(s.Status.Plan).To(BeNil(), "Status.Plan must stay nil while paused")
		for _, child := range listChildren(t, s) {
			g.Expect(child.Spec.Image).To(Equal(fixtures.DefaultImage),
				"child Spec.Image must not propagate while paused")
		}
	}, 3*time.Second, 200*time.Millisecond).Should(Succeed())

	// 4. Unpause. Children's Spec.Paused clears, and the deferred v2
	//    template change rolls forward — TemplateHash must NOT have
	//    silently advanced during the pause window, or this resume
	//    converges to a stale spec.
	cur = getSND(t, key)
	patch = client.MergeFrom(cur.DeepCopy())
	cur.Spec.Paused = false
	g.Expect(testCli.Patch(testCtx, cur, patch)).To(Succeed())

	waitFor(t, func() bool {
		children := listChildren(t, getSND(t, key))
		if len(children) == 0 {
			return false
		}
		for i := range children {
			if children[i].Spec.Paused {
				return false
			}
		}
		return true
	}, "every child SeiNode must have Spec.Paused=false after unpause")

	// 5. Deferred template change converges. ConditionRolloutInProgress
	//    must reach True (the rollout starts), and every child's
	//    Spec.Image must advance to v2 on subsequent ensureSeiNode runs.
	waitForStatus(t, key, func(s *seiv1alpha1.SeiNodeDeployment) bool {
		return condTrue(s, seiv1alpha1.ConditionRolloutInProgress) || s.Status.Rollout != nil
	}, "deferred template change must trigger a rollout after unpause")

	waitFor(t, func() bool {
		children := listChildren(t, getSND(t, key))
		if len(children) == 0 {
			return false
		}
		for i := range children {
			if children[i].Spec.Image != "ghcr.io/sei-protocol/seid:v2.0.0" {
				return false
			}
		}
		return true
	}, "child Spec.Image must converge to the v2 template after unpause")
}

func pausedCond(s *seiv1alpha1.SeiNodeDeployment) *metav1.Condition {
	return apimeta.FindStatusCondition(s.Status.Conditions, seiv1alpha1.ConditionPaused)
}

// TestSeiNode_Paused_FreezesReconcile asserts a paused SeiNode reports
// ConditionSeiNodePaused=True and does not advance through its lifecycle.
func TestSeiNode_Paused_FreezesReconcile(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "paused-node",
			Namespace: ns,
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "pacific-1",
			Image:    fixtures.DefaultImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Paused:   true,
		},
	}
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
	key := client.ObjectKeyFromObject(node)

	// ConditionSeiNodePaused must appear True.
	waitFor(t, func() bool {
		cur := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, key, cur); err != nil {
			return false
		}
		c := apimeta.FindStatusCondition(cur.Status.Conditions, seiv1alpha1.ConditionSeiNodePaused)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == "Paused"
	}, "ConditionSeiNodePaused must be True/Paused on a paused node")

	// Phase must not advance — a paused node never leaves whatever
	// state it started in. The first reconcile may set Phase="" or
	// Phase=Pending; neither should progress further.
	g.Consistently(func(g Gomega) {
		cur := &seiv1alpha1.SeiNode{}
		g.Expect(testCli.Get(testCtx, key, cur)).To(Succeed())
		g.Expect(cur.Status.Phase).NotTo(Equal(seiv1alpha1.PhaseInitializing),
			"phase must not advance past Pending while paused")
		g.Expect(cur.Status.Phase).NotTo(Equal(seiv1alpha1.PhaseRunning),
			"phase must not reach Running while paused")
	}, 3*time.Second, 200*time.Millisecond).Should(Succeed())
}
