//go:build envtest

package envtest_test

import (
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// A founding validator deleted while the genesis ceremony plan is active must
// not wedge the network. The ceremony's tasks name the founding set and would
// retry against the missing node forever while the PlanInProgress gate keeps
// reconcileSeiNodes from recreating it. The network abandons the plan, records
// ValidatorLost, tears down and recreates the whole founding set (the
// survivor's fetched genesis is stale once the replacement mints a new gentx),
// and rebuilds the ceremony to completion.
func TestGenesisCeremony_ValidatorLostMidCeremony_Recovers(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	// With instant stub sidecars the ceremony finishes within a reconcile
	// lap. A completion delay on sidecar tasks holds genesis assembly — and
	// so the plan — open long enough to take the validator away under it.
	testStub.SetCompleteAfter(5 * time.Second)
	t.Cleanup(func() { testStub.SetCompleteAfter(0) })

	network := fixtures.NewNetwork(ns, "ceremony-lost", fixtures.WithReplicas(2))
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	key := client.ObjectKeyFromObject(network)

	waitForStatus(t, key, func(n *seiv1alpha1.SeiNetwork) bool {
		return apimeta.IsStatusConditionTrue(n.Status.Conditions, seiv1alpha1.ConditionPlanInProgress) &&
			n.Status.Plan != nil
	}, "the genesis ceremony plan is active")

	// The whole founding set is torn down and recreated — the survivor's
	// fetched genesis is stale once the replacement mints a new gentx — so
	// every child needs the same GC stand-in. envtest runs no garbage
	// collector: finalizers would pin the SeiNodes in Terminating and the
	// owner-referenced data PVCs would outlive them, and a recreated child's
	// init plan refuses to adopt a claim it does not own. Clear both up front
	// so the scenario stays about the ceremony. This also means the test
	// proves the set is recreated and the ceremony rebuilt, not that a real
	// cluster's teardown clears the sidecar's genesis markers — that rests on
	// the SeiNode finalizer deleting the data PVC, which is exercised by the
	// node controller's own tests.
	originalUIDs := map[string]types.UID{}
	for i := range 2 {
		childKey := types.NamespacedName{Name: fmt.Sprintf("%s-%d", network.Name, i), Namespace: ns}
		child := &seiv1alpha1.SeiNode{}
		g.Expect(testCli.Get(testCtx, childKey, child)).To(Succeed())
		originalUIDs[child.Name] = child.UID
		if len(child.Finalizers) > 0 {
			patch := client.MergeFrom(child.DeepCopy())
			child.Finalizers = nil
			g.Expect(testCli.Patch(testCtx, child, patch)).To(Succeed())
		}

		pvcKey := types.NamespacedName{Name: "data-" + childKey.Name, Namespace: ns}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := testCli.Get(testCtx, pvcKey, pvc); err == nil {
			if len(pvc.Finalizers) > 0 {
				patch := client.MergeFrom(pvc.DeepCopy())
				pvc.Finalizers = nil
				g.Expect(testCli.Patch(testCtx, pvc, patch)).To(Succeed())
			}
			g.Expect(client.IgnoreNotFound(testCli.Delete(testCtx, pvc))).To(Succeed())
		}
		waitFor(t, func() bool {
			return testCli.Get(testCtx, pvcKey, &corev1.PersistentVolumeClaim{}) != nil
		}, "the data PVC of "+childKey.Name+" is gone")
	}

	lostKey := types.NamespacedName{Name: network.Name + "-1", Namespace: ns}
	lost := &seiv1alpha1.SeiNode{}
	g.Expect(testCli.Get(testCtx, lostKey, lost)).To(Succeed())
	g.Expect(testCli.Delete(testCtx, lost)).To(Succeed())

	// The ValidatorLost condition is transient — the rebuilt plan supersedes
	// it as soon as the child is back — so the durable operator signal is the
	// warning event.
	waitFor(t, func() bool {
		events := &corev1.EventList{}
		if err := testCli.List(testCtx, events, client.InNamespace(ns)); err != nil {
			return false
		}
		for _, ev := range events.Items {
			if ev.Reason == "ValidatorLost" && ev.InvolvedObject.Name == network.Name {
				return true
			}
		}
		return false
	}, "the network emits a ValidatorLost warning for the abandoned ceremony")

	waitFor(t, func() bool {
		for name, uid := range originalUIDs {
			fresh := &seiv1alpha1.SeiNode{}
			if err := testCli.Get(testCtx, types.NamespacedName{Name: name, Namespace: ns}, fresh); err != nil || fresh.UID == uid {
				return false
			}
		}
		return true
	}, "the whole founding set is recreated with fresh identities")

	waitForStatusWithin(t, convergeTimeout, key, func(n *seiv1alpha1.SeiNetwork) bool {
		return apimeta.IsStatusConditionTrue(n.Status.Conditions, seiv1alpha1.ConditionGenesisCeremonyComplete) &&
			apimeta.IsStatusConditionFalse(n.Status.Conditions, seiv1alpha1.ConditionPlanInProgress)
	}, "the rebuilt genesis ceremony completes over the whole set")

	g.Expect(listChildren(t, network)).To(HaveLen(2))
}
