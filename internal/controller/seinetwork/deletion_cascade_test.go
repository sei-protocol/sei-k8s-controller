package seinetwork

import (
	"context"
	"strconv"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// Ownership and deletion regression coverage for spec 004, requirements 1-3.
//
// The chain under test is SeiNetwork -> SeiNode -> StatefulSet -> pod. This
// file owns the first hop and the cascade over the whole chain; the
// SeiNode -> StatefulSet hop is covered where SyncStatefulSet lives, in
// internal/noderesource/sync_test.go, and the StatefulSet -> pod hop is set by
// the StatefulSet controller rather than by us.

const testNetUID types.UID = "network-uid-cascade"

// --- Hop 1: a validator child names its SeiNetwork (SC-002) ---

// The create path must stamp the network as the child's controller. This is the
// edge Kubernetes garbage collection walks on a Delete teardown; without it the
// child outlives the network.
func TestEnsureSeiNode_SetsControllerReferenceOnCreate(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	network.UID = testNetUID
	r := newPlanTestReconciler(t, network)

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	child := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}, child)).To(Succeed())

	ref := metav1.GetControllerOf(child)
	g.Expect(ref).NotTo(BeNil(), "the child must name its SeiNetwork as controller")
	g.Expect(ref.Kind).To(Equal(testKind))
	g.Expect(ref.Name).To(Equal(network.Name))
	g.Expect(ref.UID).To(Equal(network.UID))
	g.Expect(ref.BlockOwnerDeletion).NotTo(BeNil())
	g.Expect(*ref.BlockOwnerDeletion).To(BeTrue(),
		"a foreground delete of the network must wait for its children")
}

// The owner reference is reconciled on every pass, not only at create. A Retain
// teardown orphans children deliberately; the next run recreates the same-named
// SeiNetwork on top of them, with a new UID that removeOwnerRef can never
// re-match. Before this was reconciled the controller propagated image and
// labels to a child it did not own, and a later Delete teardown collected
// nothing — the stale validator kept running and collided with the next run.
func TestEnsureSeiNode_ReadoptsOrphanedChild(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	orphan := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testSyncerOrd0,
			Namespace: testNamespace,
			Labels:    map[string]string{seinetworkLabel: "syncer"},
			// No owner references: a prior Retain teardown stripped them.
		},
		Spec: seiv1alpha1.SeiNodeSpec{Image: "ghcr.io/sei-protocol/seid:v0.9.0"},
	}

	network := newTestNetwork("syncer", testNamespace)
	network.UID = testNetUID
	r := newPlanTestReconciler(t, network, orphan)

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	child := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}, child)).To(Succeed())

	ref := metav1.GetControllerOf(child)
	g.Expect(ref).NotTo(BeNil(), "an orphaned child must be re-adopted, not merely re-specced")
	g.Expect(ref.UID).To(Equal(network.UID))

	// The re-adopted child is visible to the owner-filtered listers again, so
	// scale-down, orphaning and IncumbentNodes all see it.
	owned, err := r.listChildSeiNodes(ctx, network)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(owned).To(HaveLen(1), "a re-adopted child must be visible to listChildSeiNodes")
}

// Adoption must not become theft. A SeiNode already controlled by something
// else is a genuine conflict — two owners fighting over one validator — so the
// reconcile fails loud rather than taking the child.
func TestEnsureSeiNode_RefusesChildOwnedByAnotherController(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	foreign := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testSyncerOrd0,
			Namespace: testNamespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: testAPIVersion,
				Kind:       testKind,
				Name:       "some-other-network",
				UID:        "other-network-uid",
				Controller: new(true),
			}},
		},
	}

	network := newTestNetwork("syncer", testNamespace)
	network.UID = testNetUID
	r := newPlanTestReconciler(t, network, foreign)

	err := r.ensureSeiNode(ctx, network, 0)
	g.Expect(err).To(HaveOccurred(), "must not steal a child another controller owns")
	g.Expect(err.Error()).To(ContainSubstring("adopting SeiNode"))

	child := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}, child)).To(Succeed())
	g.Expect(metav1.GetControllerOf(child).UID).To(Equal(types.UID("other-network-uid")),
		"the existing controller reference must be left alone")
}

// Reconciling ownership must stay idempotent: an already-owned, in-sync child
// gets no write, so the added owner-reference check cannot turn a steady-state
// reconcile into an update loop.
func TestEnsureSeiNode_OwnedChildIsNoOp(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	network.UID = testNetUID
	r := newPlanTestReconciler(t, network)

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	child := &seiv1alpha1.SeiNode{}
	childKey := types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	rvBefore := child.ResourceVersion

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.ResourceVersion).To(Equal(rvBefore),
		"an already-owned child must not be rewritten every reconcile")
}

// --- The Delete arm keeps the chain intact and defers to GC (R2) ---

// Under Delete the finalizer runs but touches no owner reference: releasing the
// finalizer is the whole cascade, and stripping a reference here would strand
// the tree.
func TestHandleDeletion_DeletePolicy_KeepsOwnerReferencesAndReleasesFinalizer(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := deletingNetwork(seiv1alpha1.DeletionPolicyDelete)
	child := childSeiNode(0, "child-uid-0")
	r := newPlanTestReconciler(t, network, child)

	_, err := r.handleDeletion(ctx, network)
	g.Expect(err).NotTo(HaveOccurred())

	got := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(child), got)).To(Succeed())
	ref := metav1.GetControllerOf(got)
	g.Expect(ref).NotTo(BeNil(), "the Delete arm must leave the owner reference in place for GC")
	g.Expect(ref.UID).To(Equal(testNetUID))

	// Finalizer released, so the apiserver can drop the network and GC can start.
	live := &seiv1alpha1.SeiNetwork{}
	err = r.Get(ctx, client.ObjectKeyFromObject(network), live)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"the network must be gone once the finalizer is released")
}

// SC-001 and SC-003: a Delete teardown leaves no child SeiNode, StatefulSet or
// pod, and nothing it deleted comes back. Neither the fake client nor envtest
// runs a garbage collector, so collectGarbage applies the collector's own
// reachability rule to the objects the controller built. What this asserts is
// therefore the part we own: that the ownership graph is complete and connected
// from the network down to the pod.
func TestHandleDeletion_DeletePolicy_GarbageCollectionRemovesWholeTree(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := deletingNetwork(seiv1alpha1.DeletionPolicyDelete)

	const replicas = 3
	objs := make([]client.Object, 0, 1+replicas*3) // network + child/STS/pod per ordinal
	objs = append(objs, network)
	for ordinal := range replicas {
		child := childSeiNode(ordinal, types.UID(seiNodeName(network, ordinal)+"-uid"))
		sts := childStatefulSet(child)
		objs = append(objs, child, sts, childPod(sts))
	}
	r := newPlanTestReconciler(t, objs...)

	_, err := r.handleDeletion(ctx, network)
	g.Expect(err).NotTo(HaveOccurred())

	collectGarbage(t, r.Client)

	nodes := &seiv1alpha1.SeiNodeList{}
	g.Expect(r.List(ctx, nodes, client.InNamespace(testGroupNS))).To(Succeed())
	g.Expect(nodes.Items).To(BeEmpty(), "no validator child may survive a Delete teardown")

	sets := &appsv1.StatefulSetList{}
	g.Expect(r.List(ctx, sets, client.InNamespace(testGroupNS))).To(Succeed())
	g.Expect(sets.Items).To(BeEmpty(), "no StatefulSet may survive a Delete teardown")

	pods := &corev1.PodList{}
	g.Expect(r.List(ctx, pods, client.InNamespace(testGroupNS))).To(Succeed())
	g.Expect(pods.Items).To(BeEmpty(), "no validator pod may survive a Delete teardown")
}

// The contrast case, and the guard on the refactored policy switch: Retain
// still strips the reference so the children outlive the network. The reason
// recorded on the child is requirement 4's job, not this ticket's.
func TestHandleDeletion_RetainPolicy_OrphansChildrenSoTheySurvive(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := deletingNetwork(seiv1alpha1.DeletionPolicyRetain)
	child := childSeiNode(0, "child-uid-0")
	sts := childStatefulSet(child)
	r := newPlanTestReconciler(t, network, child, sts, childPod(sts))

	_, err := r.handleDeletion(ctx, network)
	g.Expect(err).NotTo(HaveOccurred())

	got := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, client.ObjectKeyFromObject(child), got)).To(Succeed())
	g.Expect(metav1.GetControllerOf(got)).To(BeNil(), "Retain must orphan the child")

	collectGarbage(t, r.Client)

	nodes := &seiv1alpha1.SeiNodeList{}
	g.Expect(r.List(ctx, nodes, client.InNamespace(testGroupNS))).To(Succeed())
	g.Expect(nodes.Items).To(HaveLen(1), "a retained child must survive")

	// The child still owns its own workload, so the retained validator keeps running.
	sets := &appsv1.StatefulSetList{}
	g.Expect(r.List(ctx, sets, client.InNamespace(testGroupNS))).To(Succeed())
	g.Expect(sets.Items).To(HaveLen(1), "a retained child keeps its StatefulSet")
}

// --- Nothing is recreated mid-cascade (R3, SC-003) ---

// A deleting network creates no child. Reconcile routes a deleting network to
// handleDeletion, but the invariant is asserted at the mutation site: the
// cascade must not be fought by the code that converges replicas.
func TestReconcileSeiNodes_DeletingNetwork_CreatesNoChild(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := deletingNetwork(seiv1alpha1.DeletionPolicyDelete)
	r := newPlanTestReconciler(t, network)

	g.Expect(r.reconcileSeiNodes(ctx, network)).To(Succeed())

	nodes := &seiv1alpha1.SeiNodeList{}
	g.Expect(r.List(ctx, nodes, client.InNamespace(testGroupNS))).To(Succeed())
	g.Expect(nodes.Items).To(BeEmpty(),
		"a network under deletion must not create the children its replica count wants")
}

// The same invariant after the collector has already taken a child: the
// reconcile must not bring back what the cascade removed.
func TestReconcileSeiNodes_DeletingNetwork_DoesNotRecreateCollectedChild(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := deletingNetwork(seiv1alpha1.DeletionPolicyDelete)
	survivor := childSeiNode(0, "child-uid-0")
	// Ordinals 1 and 2 are already collected.
	r := newPlanTestReconciler(t, network, survivor)

	g.Expect(r.reconcileSeiNodes(ctx, network)).To(Succeed())

	nodes := &seiv1alpha1.SeiNodeList{}
	g.Expect(r.List(ctx, nodes, client.InNamespace(testGroupNS))).To(Succeed())
	g.Expect(nodes.Items).To(HaveLen(1), "a collected child must not be recreated mid-cascade")
	g.Expect(network.Status.IncumbentNodes).To(ConsistOf(survivor.Name),
		"the incumbent list still tracks what is left")
}

// --- fixtures ---

// deletingNetwork builds a SeiNetwork already marked for deletion with the
// controller's finalizer still held — the state handleDeletion runs in.
func deletingNetwork(policy seiv1alpha1.DeletionPolicy) *seiv1alpha1.SeiNetwork {
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.UID = testNetUID
	network.Spec.DeletionPolicy = policy
	network.Finalizers = []string{networkFinalizerName}
	now := metav1.Now()
	network.DeletionTimestamp = &now
	return network
}

// childSeiNode builds a validator child owned by the network deletingNetwork
// returns, as ensureSeiNode would have created it.
func childSeiNode(ordinal int, uid types.UID) *seiv1alpha1.SeiNode {
	name := testNetworkName + "-" + strconv.Itoa(ordinal)
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testGroupNS,
			UID:       uid,
			Labels:    map[string]string{seinetworkLabel: testNetworkName},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         testAPIVersion,
				Kind:               testKind,
				Name:               testNetworkName,
				UID:                testNetUID,
				Controller:         new(true),
				BlockOwnerDeletion: new(true),
			}},
		},
	}
}

// childStatefulSet builds the workload a SeiNode owns, as SyncStatefulSet
// would have applied it.
func childStatefulSet(node *seiv1alpha1.SeiNode) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      node.Name,
			Namespace: node.Namespace,
			UID:       node.UID + "-sts",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         seiv1alpha1.GroupVersion.String(),
				Kind:               "SeiNode",
				Name:               node.Name,
				UID:                node.UID,
				Controller:         new(true),
				BlockOwnerDeletion: new(true),
			}},
		},
	}
}

// childPod builds the pod the StatefulSet controller would have created,
// carrying the owner reference that controller sets.
func childPod(sts *appsv1.StatefulSet) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sts.Name + "-0",
			Namespace: sts.Namespace,
			UID:       sts.UID + "-pod",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         appsv1.SchemeGroupVersion.String(),
				Kind:               "StatefulSet",
				Name:               sts.Name,
				UID:                sts.UID,
				Controller:         new(true),
				BlockOwnerDeletion: new(true),
			}},
		},
	}
}

// collectGarbage stands in for the kube-controller-manager's garbage
// collector, which neither the fake client nor envtest runs. It sweeps the test
// namespace until a pass deletes nothing, removing every SeiNode, StatefulSet
// and pod whose owner references all dangle — the collector's own reachability
// rule. Running it here tests the part the controller owns: whether the
// ownership graph it built is complete and connected from the network down to
// the pod.
//
// Child finalizers are deliberately absent from these fixtures. Releasing the
// SeiNode finalizer is the node controller's deletion path, covered in its own
// package; including it here would test that controller rather than the graph.
func collectGarbage(t *testing.T, c client.Client) {
	t.Helper()
	g := NewWithT(t)
	ctx := context.Background()

	for pass := 0; ; pass++ {
		g.Expect(pass).To(BeNumerically("<", 10), "garbage collection did not settle")

		networks := &seiv1alpha1.SeiNetworkList{}
		g.Expect(c.List(ctx, networks, client.InNamespace(testGroupNS))).To(Succeed())
		nodes := &seiv1alpha1.SeiNodeList{}
		g.Expect(c.List(ctx, nodes, client.InNamespace(testGroupNS))).To(Succeed())
		sets := &appsv1.StatefulSetList{}
		g.Expect(c.List(ctx, sets, client.InNamespace(testGroupNS))).To(Succeed())
		pods := &corev1.PodList{}
		g.Expect(c.List(ctx, pods, client.InNamespace(testGroupNS))).To(Succeed())

		// Everything still present is a live owner; everything but the network
		// is also a potential dependent.
		live := make(map[types.UID]bool,
			len(networks.Items)+len(nodes.Items)+len(sets.Items)+len(pods.Items))
		dependents := make([]client.Object, 0,
			len(nodes.Items)+len(sets.Items)+len(pods.Items))
		for i := range networks.Items {
			live[networks.Items[i].UID] = true
		}
		for i := range nodes.Items {
			live[nodes.Items[i].UID] = true
			dependents = append(dependents, &nodes.Items[i])
		}
		for i := range sets.Items {
			live[sets.Items[i].UID] = true
			dependents = append(dependents, &sets.Items[i])
		}
		for i := range pods.Items {
			live[pods.Items[i].UID] = true
			dependents = append(dependents, &pods.Items[i])
		}

		collected := 0
		for _, obj := range dependents {
			refs := obj.GetOwnerReferences()
			if len(refs) == 0 {
				continue // a root object, or one deliberately orphaned
			}
			reachable := false
			for _, ref := range refs {
				if live[ref.UID] {
					reachable = true
					break
				}
			}
			if reachable {
				continue
			}
			g.Expect(client.IgnoreNotFound(c.Delete(ctx, obj))).To(Succeed())
			collected++
		}
		if collected == 0 {
			return
		}
	}
}
