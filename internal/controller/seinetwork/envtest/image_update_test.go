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

// convergeTimeout covers the full genesis ceremony + initial child boot +
// the child NodeUpdate plan that backfills status.currentImage. That chain
// runs longer than the shared 30s pollTimeout in envtest, so the
// steady-state waits here use their own budget.
const convergeTimeout = 120 * time.Second

// TestImageUpdate_DerivedProjection exercises the no-rollout-state-machine
// image-bump path against a real apiserver with the SeiNode controller wired
// in alongside. There is no deployment plan, no templateHash, no
// status.rollout — spec.image propagates to children in-place every reconcile
// and the network's view of progress is a pure derived projection.
//
// Lifecycle:
//   - network reaches steady state on the original image:
//     upToDateReplicas == replicas, RolloutInProgress=False/AllUpToDate
//   - spec.image patched; the faker is paused first so children's
//     status.currentImage lags long enough to observe the derived
//     RolloutInProgress=True deterministically
//   - faker resumed; each child's spec.image AND status.currentImage converge
//     to the new image, upToDateReplicas reaches replicas, and the derived
//     RolloutInProgress goes True → False/AllUpToDate
//   - the image stays pinned across several reconciles (no flip-flop)
func TestImageUpdate_DerivedProjection(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	const (
		oldImage = "ghcr.io/sei-protocol/seid:v1.0.0"
		newImage = "ghcr.io/sei-protocol/seid:v2.0.0"
		replicas = 2
	)

	network := fixtures.NewNetwork(ns, "image-update",
		fixtures.WithReplicas(replicas),
		fixtures.WithImage(oldImage),
	)
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	key := client.ObjectKeyFromObject(network)

	// 1. Children created, genesis completes, and the network reaches steady
	//    state on the original image: every child reports currentImage ==
	//    spec.image, so upToDateReplicas == replicas and the derived
	//    RolloutInProgress is False/AllUpToDate.
	g.Eventually(func(g Gomega) {
		s := getNetwork(t, key)
		g.Expect(listChildren(t, s)).To(HaveLen(replicas))
		g.Expect(s.Status.UpToDateReplicas).To(Equal(int32(replicas)))
		c := rolloutCond(s)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(c.Reason).To(Equal(reasonAllUpToDate))
		for _, n := range s.Status.Nodes {
			g.Expect(n.CurrentImage).To(Equal(oldImage),
				"GroupNodeStatus.CurrentImage must mirror the child's status.currentImage")
		}
	}, convergeTimeout, pollInterval).Should(Succeed(),
		"network steady on old image: upToDateReplicas==replicas, RolloutInProgress=False/AllUpToDate")

	// 2. Stall the faker so the SeiNode StatefulSet rollout cannot complete;
	//    children keep reporting the old currentImage long enough to observe
	//    the derived RolloutInProgress=True.
	testFaker.Pause()

	patchNetworkImage(t, getNetwork(t, key), newImage)

	// spec.image propagates to children in-place even while the roll is
	// stalled (ensureSeiNode patches every reconcile, no hash gate).
	g.Eventually(func(g Gomega) {
		for _, kid := range listChildren(t, getNetwork(t, key)) {
			g.Expect(kid.Spec.Image).To(Equal(newImage))
		}
	}, convergeTimeout, pollInterval).Should(Succeed(),
		"spec.image propagates to every child in-place")

	// Derived RolloutInProgress flips True while children lag the new image.
	g.Eventually(func(g Gomega) {
		s := getNetwork(t, key)
		c := rolloutCond(s)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(c.Reason).To(Equal("ImageRolling"))
		g.Expect(s.Status.UpToDateReplicas).To(BeNumerically("<", int32(replicas)))
	}, convergeTimeout, pollInterval).Should(Succeed(),
		"RolloutInProgress=True/ImageRolling while a child lags the new image")

	// 3. Resume the faker; the child SeiNode controllers roll their
	//    StatefulSets and stamp status.currentImage = newImage.
	testFaker.Resume()

	g.Eventually(func(g Gomega) {
		s := getNetwork(t, key)
		kids := listChildren(t, s)
		g.Expect(kids).To(HaveLen(replicas))
		for i := range kids {
			g.Expect(kids[i].Status.CurrentImage).To(Equal(newImage))
		}
		g.Expect(s.Status.UpToDateReplicas).To(Equal(int32(replicas)))
		c := rolloutCond(s)
		g.Expect(c).NotTo(BeNil())
		g.Expect(c.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(c.Reason).To(Equal(reasonAllUpToDate))
	}, convergeTimeout, pollInterval).Should(Succeed(),
		"every child converges to newImage; upToDateReplicas==replicas; RolloutInProgress=False/AllUpToDate")

	// 4. No flip-flop. Once converged, spec.image stays pinned to the network
	//    image across several reconciles — ensureSeiNode is the single source
	//    of child image and must not oscillate.
	g.Consistently(func(g Gomega) {
		for _, kid := range listChildren(t, getNetwork(t, key)) {
			g.Expect(kid.Spec.Image).To(Equal(newImage),
				"child spec.image must stay pinned to the network image")
		}
	}, 3*time.Second, pollInterval).Should(Succeed())
}

func rolloutCond(s *seiv1alpha1.SeiNetwork) *metav1.Condition {
	return apimeta.FindStatusCondition(s.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
}
