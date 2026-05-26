//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest/fixtures"
)

// TestSeiNode_StatefulSetTrackedOnStatus asserts the controller creates
// the owned StatefulSet on first reconcile and records {Name, UID} on
// Status. Subsequent reconciles fetch and mutate the tracked object
// rather than re-applying blindly.
func TestSeiNode_StatefulSetTrackedOnStatus(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := newTestSeiNode(ns, "sts-tracked", false)
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
	key := client.ObjectKeyFromObject(node)

	waitFor(t, func() bool {
		cur := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, key, cur); err != nil {
			return false
		}
		return cur.Status.StatefulSet != nil && cur.Status.StatefulSet.UID != ""
	}, "Status.StatefulSet must be recorded after first reconcile")

	cur := &seiv1alpha1.SeiNode{}
	g.Expect(testCli.Get(testCtx, key, cur)).To(Succeed())
	ref := cur.Status.StatefulSet
	g.Expect(ref.Name).To(Equal(node.Name))

	sts := &appsv1.StatefulSet{}
	g.Expect(testCli.Get(testCtx, types.NamespacedName{Name: ref.Name, Namespace: ns}, sts)).To(Succeed())
	g.Expect(sts.UID).To(Equal(ref.UID), "tracked UID must match the live StatefulSet")
}

// TestSeiNode_Paused_ScalesStatefulSetToZero exercises the hard-pause
// behavior: setting Spec.Paused=true scales the owned StatefulSet to
// zero replicas; clearing it restores the desired replica count.
func TestSeiNode_Paused_ScalesStatefulSetToZero(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := newTestSeiNode(ns, "paused-scaled", false)
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
	key := client.ObjectKeyFromObject(node)

	// Initial reconcile creates the StatefulSet at replicas=1.
	stsKey := types.NamespacedName{Name: node.Name, Namespace: ns}
	waitFor(t, func() bool {
		sts := &appsv1.StatefulSet{}
		if err := testCli.Get(testCtx, stsKey, sts); err != nil {
			return false
		}
		return sts.Spec.Replicas != nil && *sts.Spec.Replicas == 1
	}, "StatefulSet must be created with replicas=1 for an unpaused SeiNode")

	// Pause. Controller scales to 0.
	cur := &seiv1alpha1.SeiNode{}
	g.Expect(testCli.Get(testCtx, key, cur)).To(Succeed())
	patch := client.MergeFrom(cur.DeepCopy())
	cur.Spec.Paused = true
	g.Expect(testCli.Patch(testCtx, cur, patch)).To(Succeed())

	waitFor(t, func() bool {
		sts := &appsv1.StatefulSet{}
		if err := testCli.Get(testCtx, stsKey, sts); err != nil {
			return false
		}
		return sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0
	}, "StatefulSet must scale to replicas=0 after pause")

	// Unpause. Controller restores replicas.
	g.Expect(testCli.Get(testCtx, key, cur)).To(Succeed())
	patch = client.MergeFrom(cur.DeepCopy())
	cur.Spec.Paused = false
	g.Expect(testCli.Patch(testCtx, cur, patch)).To(Succeed())

	waitFor(t, func() bool {
		sts := &appsv1.StatefulSet{}
		if err := testCli.Get(testCtx, stsKey, sts); err != nil {
			return false
		}
		return sts.Spec.Replicas != nil && *sts.Spec.Replicas == 1
	}, "StatefulSet must scale back to replicas=1 after unpause")
}

// TestSeiNode_StatefulSetRecreatedAfterDelete covers the
// out-of-band-deletion case: if an operator deletes the StatefulSet,
// the next reconcile observes the NotFound, recreates the object, and
// re-records the new UID on Status.StatefulSet.
func TestSeiNode_StatefulSetRecreatedAfterDelete(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := newTestSeiNode(ns, "sts-recreated", false)
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
	key := client.ObjectKeyFromObject(node)

	waitFor(t, func() bool {
		cur := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, key, cur); err != nil {
			return false
		}
		return cur.Status.StatefulSet != nil
	}, "Status.StatefulSet must be recorded after first reconcile")

	cur := &seiv1alpha1.SeiNode{}
	g.Expect(testCli.Get(testCtx, key, cur)).To(Succeed())
	originalUID := cur.Status.StatefulSet.UID

	sts := &appsv1.StatefulSet{}
	stsKey := types.NamespacedName{Name: node.Name, Namespace: ns}
	g.Expect(testCli.Get(testCtx, stsKey, sts)).To(Succeed())
	g.Expect(testCli.Delete(testCtx, sts)).To(Succeed())

	// Barrier: let the Owns watch fire its deletion reconcile before the
	// spec patch below. Without it, the spec patch races the in-flight
	// reconcile's optimistic-locked status flush → 409 → flake.
	waitFor(t, func() bool {
		s := &appsv1.StatefulSet{}
		err := testCli.Get(testCtx, stsKey, s)
		return apierrors.IsNotFound(err) || (err == nil && s.UID != originalUID)
	}, "controller must observe the StatefulSet deletion before next mutation")

	// Bump the spec so the controller reconciles again (the SeiNode
	// reconciler watches StatefulSet via Owns, but envtest sometimes
	// races; a Spec edit triggers the predicate deterministically).
	g.Expect(testCli.Get(testCtx, key, cur)).To(Succeed())
	patch := client.MergeFrom(cur.DeepCopy())
	if cur.Spec.Overrides == nil {
		cur.Spec.Overrides = map[string]string{}
	}
	cur.Spec.Overrides["chain.touch-counter"] = "1"
	g.Expect(testCli.Patch(testCtx, cur, patch)).To(Succeed())

	waitFor(t, func() bool {
		s := &appsv1.StatefulSet{}
		if err := testCli.Get(testCtx, stsKey, s); err != nil {
			return apierrors.IsNotFound(err) == false
		}
		// Confirm the Status ref reflects the new UID.
		latest := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, key, latest); err != nil {
			return false
		}
		return latest.Status.StatefulSet != nil &&
			latest.Status.StatefulSet.UID == s.UID &&
			s.UID != originalUID
	}, "deleted StatefulSet must be recreated with a new UID and re-tracked on Status")
}

// TestSeiNode_StatefulSetImpostorReplaced covers the UID-mismatch
// recovery: when Status.StatefulSet points at a UID that doesn't match
// the live object, the controller deletes the impostor on one reconcile
// and Applies a fresh StatefulSet on the next. The split keeps each
// reconcile against a clean apiserver state — applying onto a
// still-deleting object has undefined SSA semantics if a finalizer
// ever blocks removal.
//
// envtest has no kube-controller-manager. That's fine for this case:
// StatefulSets carry no finalizers by default, so the Background-
// propagation Delete the controller issues commits to etcd
// immediately. No tombstone state to drain; the Owns watch fires the
// next reconcile, which sees NotFound and creates fresh.
//
// The test asserts on the terminal state — STS recreated with a new
// UID and Status.StatefulSet re-tracked — rather than on the transient
// "delete observed" state, which is unobservably short under
// Background propagation with no finalizers.
func TestSeiNode_StatefulSetImpostorReplaced(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := newTestSeiNode(ns, "sts-impostor", false)
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
	key := client.ObjectKeyFromObject(node)
	stsKey := types.NamespacedName{Name: node.Name, Namespace: ns}

	waitFor(t, func() bool {
		cur := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, key, cur); err != nil {
			return false
		}
		return cur.Status.StatefulSet != nil && cur.Status.StatefulSet.UID != ""
	}, "Status.StatefulSet recorded after first reconcile")

	cur := &seiv1alpha1.SeiNode{}
	g.Expect(testCli.Get(testCtx, key, cur)).To(Succeed())
	realUID := cur.Status.StatefulSet.UID

	// Forge a UID mismatch with Status().Update — a MergeFrom patch
	// on a pointer-typed sub-struct (Status.StatefulSet is *StatefulSetRef)
	// can serialize to an empty body when the diff logic compares
	// pointer identity rather than dereferenced contents; Update sends
	// the full object and avoids that pitfall.
	cur.Status.StatefulSet.UID = "00000000-0000-0000-0000-000000000000"
	g.Expect(testCli.Status().Update(testCtx, cur)).To(Succeed())

	// Barrier: confirm the forged status is durable and any reconcile
	// woken by Status.Update has drained before the spec patch below.
	// Without it, back-to-back writes race the in-flight reconcile's
	// optimistic-locked status flush → 409 → flake.
	waitFor(t, func() bool {
		latest := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, key, latest); err != nil {
			return false
		}
		return latest.Status.StatefulSet != nil &&
			latest.Status.StatefulSet.UID == "00000000-0000-0000-0000-000000000000"
	}, "Status.Update forging UID must be durable before next mutation")

	// Trigger a spec-bumped reconcile so the controller picks up the
	// forged mismatch. The Owns(StatefulSet) watch would fire on the
	// subsequent Delete too, but a spec bump deterministically pokes
	// the predicate on the SeiNode itself.
	g.Expect(testCli.Get(testCtx, key, cur)).To(Succeed())
	specPatch := client.MergeFrom(cur.DeepCopy())
	if cur.Spec.Overrides == nil {
		cur.Spec.Overrides = map[string]string{}
	}
	cur.Spec.Overrides["chain.touch-counter"] = "1"
	g.Expect(testCli.Patch(testCtx, cur, specPatch)).To(Succeed())

	// Converge to terminal state: a new StatefulSet exists with a UID
	// different from the original, and Status.StatefulSet tracks it.
	// This proves both legs of the impostor branch ran (Delete of the
	// realUID object, Apply that produced a fresh UID).
	waitFor(t, func() bool {
		sts := &appsv1.StatefulSet{}
		if err := testCli.Get(testCtx, stsKey, sts); err != nil {
			return false
		}
		if sts.UID == realUID {
			return false
		}
		latest := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, key, latest); err != nil {
			return false
		}
		return latest.Status.StatefulSet != nil &&
			latest.Status.StatefulSet.UID == sts.UID
	}, "replacement StatefulSet must be created with a new UID and re-tracked on Status")
}

func newTestSeiNode(namespace, name string, paused bool) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "pacific-1",
			Image:    fixtures.DefaultImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Paused:   paused,
		},
	}
}
