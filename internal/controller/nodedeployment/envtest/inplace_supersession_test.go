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

// TestInPlaceRollout_Supersession verifies the supersession branch in
// detectDeploymentNeeded (internal/controller/nodedeployment/nodes.go):
// when spec.template.spec.image changes a second time before the first
// rollout finishes, the in-flight plan must be replaced with one
// targeting the latest hash, and a RolloutSuperseded event must be
// recorded against the SND so an operator can see the transition in
// `kubectl describe`.
//
// Timing control comes from pausing the StatefulSet status faker. While
// paused, the StatefulSet's .Status never advances past the previous
// generation, which stalls the SeiNode-side ObserveImage task and in
// turn stalls the SND-side AwaitSpecUpdate task that polls children's
// Status.CurrentImage. The v2 rollout therefore parks in flight,
// giving us a deterministic window in which to patch v3.
//
// Asserts in order:
//
//  1. Initial v1 deployment reaches steady state (status.rollout == nil)
//  2. Patching to v2 starts a rollout with TargetHash != v1 hash
//  3. With the faker paused, the v2 rollout remains in flight (does NOT
//     complete) until we resume the faker
//  4. Patching to v3 while v2 is in flight retargets Status.Rollout to
//     the v3 hash and records a RolloutSuperseded event referencing the
//     old (v2) target
//  5. After resuming the faker, the v3 rollout completes cleanly:
//     status.plan nil, status.rollout nil, RolloutInProgress
//     False/RolloutComplete, every child at v3
func TestInPlaceRollout_Supersession(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	const (
		v1       = "ghcr.io/sei-protocol/seid:v1.0.0"
		v2       = "ghcr.io/sei-protocol/seid:v2.0.0"
		v3       = "ghcr.io/sei-protocol/seid:v3.0.0"
		replicas = 2
	)

	snd := fixtures.NewSND(ns, "supersession",
		fixtures.WithReplicas(replicas),
		fixtures.WithImage(v1),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())
	key := client.ObjectKeyFromObject(snd)

	// 1. Initial v1 settle. With the faker running, the rollout
	//    collapses to instantaneous; we just want Status.TemplateHash
	//    populated and no in-flight rollout.
	waitForStatus(t, key, func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		if latest.Status.TemplateHash == "" {
			return false
		}
		if latest.Status.Rollout != nil {
			return false
		}
		return !condTrue(latest, seiv1alpha1.ConditionRolloutInProgress)
	}, "initial v1 deployment reached steady state")

	v1Hash := getSND(t, key).Status.TemplateHash
	g.Expect(v1Hash).NotTo(BeEmpty())

	// 2. Pause the faker BEFORE the v2 patch so the v2 rollout stalls
	//    in flight. The Cleanup ensures we re-enable the faker even if
	//    the test fails partway, keeping subsequent tests on a
	//    deterministic harness.
	testFaker.Pause()
	t.Cleanup(testFaker.Resume)

	// 3. Patch v1→v2 and wait for the rollout to register. Do NOT wait
	//    for completion — with the faker paused, it can't complete.
	patchSNDImage(t, getSND(t, key), v2)

	var v2Hash string
	waitForStatus(t, key, func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		if latest.Status.Rollout == nil {
			return false
		}
		if latest.Status.Rollout.TargetHash == "" || latest.Status.Rollout.TargetHash == v1Hash {
			return false
		}
		v2Hash = latest.Status.Rollout.TargetHash
		return condTrue(latest, seiv1alpha1.ConditionRolloutInProgress)
	}, "v2 rollout registered (Status.Rollout populated, RolloutInProgress=True)")
	g.Expect(v2Hash).NotTo(Equal(v1Hash))

	// 4. Patch v2→v3 while the v2 plan is in flight. detectDeploymentNeeded's
	//    supersession branch should fire on the next reconcile: it nils
	//    Status.Plan, records a RolloutSuperseded event with the v2
	//    target hash, then falls through to write a fresh Status.Rollout
	//    with the v3 hash.
	patchSNDImage(t, getSND(t, key), v3)

	var v3Hash string
	waitForStatus(t, key, func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		if latest.Status.Rollout == nil {
			return false
		}
		if latest.Status.Rollout.TargetHash == v1Hash || latest.Status.Rollout.TargetHash == v2Hash {
			return false
		}
		v3Hash = latest.Status.Rollout.TargetHash
		return condTrue(latest, seiv1alpha1.ConditionRolloutInProgress)
	}, "rollout retargeted to v3 (supersession applied)")
	g.Expect(v3Hash).NotTo(Equal(v2Hash))
	g.Expect(v3Hash).NotTo(Equal(v1Hash))

	// 5. The supersession event was recorded against the SND. The
	//    message format from nodes.go:detectDeploymentNeeded includes
	//    the old (v2) target hash verbatim, so we can assert on
	//    presence + content rather than just count.
	events := listEventsForSND(t, getSND(t, key), "RolloutSuperseded")
	g.Expect(events).NotTo(BeEmpty(), "expected at least one RolloutSuperseded event for the v2→v3 transition")
	g.Expect(events[0].Message).To(ContainSubstring(v2Hash),
		"supersession event should reference the old (v2) target hash")

	// 6. Resume the faker so the v3 rollout can complete.
	testFaker.Resume()

	// 7. The v3 rollout reaches terminal state: plan cleared,
	//    Status.Rollout cleared, RolloutInProgress False with reason
	//    RolloutComplete.
	waitForStatus(t, key, func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		if latest.Status.Plan != nil {
			return false
		}
		if latest.Status.Rollout != nil {
			return false
		}
		cond := apimeta.FindStatusCondition(latest.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
		if cond == nil || cond.Status != metav1.ConditionFalse {
			return false
		}
		return cond.Reason == "RolloutComplete"
	}, "v3 rollout reached terminal state after faker resume")

	// 8. Every child landed on v3 (not v2).
	kids := listChildren(t, getSND(t, key))
	g.Expect(kids).To(HaveLen(replicas))
	for i := range kids {
		g.Expect(kids[i].Spec.Image).To(Equal(v3), "child %s spec.image", kids[i].Name)
	}
}
