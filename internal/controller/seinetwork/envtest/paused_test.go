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
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// TestSeiNetwork_Paused_PropagatesAndBlocksOrchestration asserts:
//  1. spec.paused=true sets ConditionPaused=True and Status.Phase=Paused.
//  2. Paused state propagates to every owned child SeiNode.
//  3. An image change applied while paused does not propagate to children's
//     spec (reconcileSeiNodes short-circuits while paused).
//  4. Clearing spec.paused propagates back to children and the deferred
//     image change propagates in-place to convergence.
func TestSeiNetwork_Paused_PropagatesAndBlocksOrchestration(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	snd := fixtures.NewNetwork(ns, "paused-snd", fixtures.WithReplicas(2))
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())
	key := client.ObjectKeyFromObject(snd)

	// 1. Initial settle. Both children exist and the SeiNetwork has
	//    ConditionPaused=False/NotPaused seeded.
	waitForStatus(t, key, func(s *seiv1alpha1.SeiNetwork) bool {
		return len(listChildren(t, s)) == 2 &&
			pausedCond(s) != nil && pausedCond(s).Status == metav1.ConditionFalse
	}, "two children exist and ConditionPaused=False is seeded on a fresh SeiNetwork")

	// 2. Pause the SeiNetwork. ConditionPaused flips True, Status.Phase moves
	//    to Paused, and children pick up Spec.Paused=true on the next
	//    reconcile.
	cur := getNetwork(t, key)
	patch := client.MergeFrom(cur.DeepCopy())
	cur.Spec.Paused = true
	g.Expect(testCli.Patch(testCtx, cur, patch)).To(Succeed())

	waitForStatus(t, key, func(s *seiv1alpha1.SeiNetwork) bool {
		c := pausedCond(s)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == "Paused" &&
			s.Status.Phase == seiv1alpha1.GroupPhasePaused
	}, "ConditionPaused must transition to True/Paused and Status.Phase to Paused")

	waitFor(t, func() bool {
		children := listChildren(t, getNetwork(t, key))
		if len(children) == 0 {
			return false
		}
		for i := range children {
			if !children[i].Spec.Paused {
				return false
			}
		}
		return true
	}, "every child SeiNode must have Spec.Paused=true after SeiNetwork pause")

	// 3. Image change is observed but blocked. Patch v1→v2 while paused
	//    and assert child Spec.Image stays at the original for 3s — a
	//    paused reconcile short-circuits before ensureSeiNode.
	cur = getNetwork(t, key)
	patchNetworkImage(t, cur, "ghcr.io/sei-protocol/seid:v2.0.0")

	g.Consistently(func(g Gomega) {
		s := getNetwork(t, key)
		g.Expect(s.Status.Plan).To(BeNil(), "Status.Plan must stay nil while paused")
		for _, child := range listChildren(t, s) {
			g.Expect(child.Spec.Image).To(Equal(fixtures.DefaultImage),
				"child Spec.Image must not propagate while paused")
		}
	}, 3*time.Second, 200*time.Millisecond).Should(Succeed())

	// 4. Unpause. Children's Spec.Paused clears, and the deferred v2
	//    image propagates in-place via ensureSeiNode.
	cur = getNetwork(t, key)
	patch = client.MergeFrom(cur.DeepCopy())
	cur.Spec.Paused = false
	g.Expect(testCli.Patch(testCtx, cur, patch)).To(Succeed())

	waitFor(t, func() bool {
		children := listChildren(t, getNetwork(t, key))
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

	// 5. The deferred image change propagates to every child's Spec.Image
	//    on subsequent ensureSeiNode runs (no plan, no rollout machinery).
	waitFor(t, func() bool {
		children := listChildren(t, getNetwork(t, key))
		if len(children) == 0 {
			return false
		}
		for i := range children {
			if children[i].Spec.Image != "ghcr.io/sei-protocol/seid:v2.0.0" {
				return false
			}
		}
		return true
	}, "child Spec.Image must converge to v2 after unpause")
}

func pausedCond(s *seiv1alpha1.SeiNetwork) *metav1.Condition {
	return apimeta.FindStatusCondition(s.Status.Conditions, seiv1alpha1.ConditionPaused)
}

// Note: the unpause-during-active-plan deadlock guard moved to a unit test
// (TestSyncPausedToChildren_IgnoresPlanInProgress in nodes_test.go). With the
// rollout state machine gone, the genesis ceremony is the only network-level
// plan and it completes too fast in envtest to deterministically pause
// against; the invariant it guards — syncPausedToChildren runs unconditionally
// regardless of plan state — is better asserted directly.

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
