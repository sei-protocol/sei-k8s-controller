//go:build envtest

package envtest_test

import (
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
// ValidatorLost, recreates the child, and rebuilds the ceremony to completion.
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

	childKey := types.NamespacedName{Name: network.Name + "-1", Namespace: ns}
	child := &seiv1alpha1.SeiNode{}
	g.Expect(testCli.Get(testCtx, childKey, child)).To(Succeed())
	originalUID := child.UID
	if len(child.Finalizers) > 0 {
		patch := client.MergeFrom(child.DeepCopy())
		child.Finalizers = nil
		g.Expect(testCli.Patch(testCtx, child, patch)).To(Succeed())
	}

	// envtest runs no garbage collector: the owner-referenced data PVC would
	// outlive its SeiNode and the replacement's init plan would refuse to
	// adopt it. Stand in for GC before the delete so the recreated child
	// never races a lingering claim, keeping the scenario about the ceremony.
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
	}, "the lost validator's data PVC is gone")

	g.Expect(testCli.Delete(testCtx, child)).To(Succeed())

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
		fresh := &seiv1alpha1.SeiNode{}
		return testCli.Get(testCtx, childKey, fresh) == nil && fresh.UID != originalUID
	}, "the lost validator is recreated")

	waitForStatusWithin(t, convergeTimeout, key, func(n *seiv1alpha1.SeiNetwork) bool {
		return apimeta.IsStatusConditionTrue(n.Status.Conditions, seiv1alpha1.ConditionGenesisCeremonyComplete) &&
			apimeta.IsStatusConditionFalse(n.Status.Conditions, seiv1alpha1.ConditionPlanInProgress)
	}, "the rebuilt genesis ceremony completes over the whole set")

	g.Expect(listChildren(t, network)).To(HaveLen(2))
}
