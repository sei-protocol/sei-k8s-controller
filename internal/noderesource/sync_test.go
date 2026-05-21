package noderesource

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

const (
	testNamespace    = "default"
	impostorName     = "impostor-swap"
	steadyStateName  = "steady-state"
	originalTestUID  = "uid-A"
	impostorTestUID  = "uid-B"
)

// newSyncTestScheme builds a scheme with both core and sei.io types.
// SyncStatefulSet writes appsv1.StatefulSet and reads seiv1alpha1.SeiNode,
// so both must be registered for the fake client to encode/decode.
func newSyncTestScheme(t *testing.T) *k8sruntime.Scheme {
	t.Helper()
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSyncStatefulSet_FirstCreate exercises the cold-start path: no
// tracked Status, no live StatefulSet. SyncStatefulSet renders the
// desired object and Server-Side-Applies it, returning the live result
// for the caller to record on Status.
func TestSyncStatefulSet_FirstCreate(t *testing.T) {
	g := NewWithT(t)
	s := newSyncTestScheme(t)

	node := newGenesisNode("first-create", testNamespace)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()

	sts, err := SyncStatefulSet(context.Background(), c, s, node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).NotTo(BeNil(), "first-create must return the applied StatefulSet")
	g.Expect(sts.Name).To(Equal("first-create"))
	g.Expect(sts.Namespace).To(Equal(testNamespace))

	live := &appsv1.StatefulSet{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: "first-create", Namespace: testNamespace}, live)).To(Succeed())
}

// TestSyncStatefulSet_ImpostorDeletes covers the UID-mismatch branch.
// Status.StatefulSet tracks UID-A. A live StatefulSet with the same
// name but UID-B (the "impostor") is present. SyncStatefulSet must
// Delete the impostor and return (nil, nil) — leaving the Apply to the
// next reconcile, when Get will observe NotFound and the Apply lands
// as a fresh Create. Splitting Delete and Apply across reconciles
// avoids applying onto a still-deleting object.
func TestSyncStatefulSet_ImpostorDeletes(t *testing.T) {
	g := NewWithT(t)
	s := newSyncTestScheme(t)

	node := newGenesisNode(impostorName, testNamespace)
	node.Status.StatefulSet = &seiv1alpha1.StatefulSetRef{
		Name: impostorName,
		UID:  originalTestUID,
	}

	impostor := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      impostorName,
			Namespace: testNamespace,
			UID:       impostorTestUID,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node, impostor).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()

	sts, err := SyncStatefulSet(context.Background(), c, s, node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).To(BeNil(), "impostor branch returns nil so caller requeues without applying")

	// The impostor must be gone from the apiserver (fake client honors
	// Background-propagation Delete on objects with no finalizers).
	live := &appsv1.StatefulSet{}
	err = c.Get(context.Background(), types.NamespacedName{Name: impostorName, Namespace: testNamespace}, live)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "impostor must be deleted; got err=%v", err)
}

// TestSyncStatefulSet_ImpostorThenApply simulates the two-reconcile
// recovery sequence end-to-end on the fake client. Reconcile 1 deletes
// the impostor and returns nil. Reconcile 2 observes NotFound and
// Applies fresh; the returned StatefulSet has a new identity that the
// caller stamps onto Status.
func TestSyncStatefulSet_ImpostorThenApply(t *testing.T) {
	g := NewWithT(t)
	s := newSyncTestScheme(t)

	node := newGenesisNode("impostor-2reconcile", testNamespace)
	node.Status.StatefulSet = &seiv1alpha1.StatefulSetRef{
		Name: "impostor-2reconcile",
		UID:  originalTestUID,
	}

	impostor := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "impostor-2reconcile",
			Namespace: testNamespace,
			UID:       impostorTestUID,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node, impostor).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()

	// Reconcile 1: detect mismatch, Delete, return nil.
	sts, err := SyncStatefulSet(context.Background(), c, s, node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).To(BeNil())

	// Reconcile 2: Get sees NotFound, Apply creates fresh.
	sts, err = SyncStatefulSet(context.Background(), c, s, node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).NotTo(BeNil(), "second reconcile must Apply fresh")
	g.Expect(sts.UID).NotTo(Equal(types.UID(originalTestUID)))
	g.Expect(sts.UID).NotTo(Equal(types.UID(impostorTestUID)))
}

// TestSyncStatefulSet_MatchingUID is the steady-state path. A live
// StatefulSet exists whose UID matches Status.StatefulSet. Sync must
// not delete; it Applies (idempotent) and returns the live object.
func TestSyncStatefulSet_MatchingUID(t *testing.T) {
	g := NewWithT(t)
	s := newSyncTestScheme(t)

	node := newGenesisNode(steadyStateName, testNamespace)
	node.Status.StatefulSet = &seiv1alpha1.StatefulSetRef{
		Name: steadyStateName,
		UID:  originalTestUID,
	}

	existing := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      steadyStateName,
			Namespace: testNamespace,
			UID:       originalTestUID,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node, existing).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()

	sts, err := SyncStatefulSet(context.Background(), c, s, node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).NotTo(BeNil())

	// The original object must still be present (UID preserved means
	// no delete occurred).
	live := &appsv1.StatefulSet{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: steadyStateName, Namespace: testNamespace}, live)).To(Succeed())
	g.Expect(live.UID).To(Equal(types.UID(originalTestUID)))
}

// TestSyncStatefulSet_TrackedButMissing covers the "controller restarts
// after operator delete" path. Status tracks a UID but the live STS is
// gone. Sync skips the impostor branch and Applies fresh.
func TestSyncStatefulSet_TrackedButMissing(t *testing.T) {
	g := NewWithT(t)
	s := newSyncTestScheme(t)

	node := newGenesisNode("tracked-missing", testNamespace)
	node.Status.StatefulSet = &seiv1alpha1.StatefulSetRef{
		Name: "tracked-missing",
		UID:  originalTestUID,
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()

	sts, err := SyncStatefulSet(context.Background(), c, s, node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).NotTo(BeNil(), "tracked-but-missing must Apply fresh on the same reconcile")
}

// TestSyncStatefulSet_PausedReplicasZero asserts the pause behavior:
// Spec.Paused=true forces Replicas=0 in the Applied object regardless
// of whether the node is in the impostor branch path.
func TestSyncStatefulSet_PausedReplicasZero(t *testing.T) {
	g := NewWithT(t)
	s := newSyncTestScheme(t)

	node := newGenesisNode("paused-zero", testNamespace)
	node.Spec.Paused = true

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()

	sts, err := SyncStatefulSet(context.Background(), c, s, node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).NotTo(BeNil())
	g.Expect(sts.Spec.Replicas).NotTo(BeNil())
	g.Expect(*sts.Spec.Replicas).To(Equal(int32(0)))
}
