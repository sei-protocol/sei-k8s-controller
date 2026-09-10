package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// Spec 004 requirement 5: a StatefulSet deleted under a live SeiNode comes
// back, and the node records why. The recreate itself is SyncStatefulSet's
// job (internal/noderesource/sync_test.go); what is asserted here is the
// record the node controller leaves behind.

// A cold start is a create, not a recreate: nothing was tracked, so nothing
// is announced.
func TestReconcileStatefulSet_FirstCreateRecordsNothing(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("sts-first", "default")
	r, _ := newNodeReconciler(t, node)
	recorder := r.Recorder.(*record.FakeRecorder)

	g.Expect(r.reconcileStatefulSet(ctx, node)).To(Succeed())
	g.Expect(node.Status.StatefulSet).NotTo(BeNil())
	g.Expect(recorder.Events).NotTo(Receive())
}

// The tracked UID names a StatefulSet that is gone. The Apply lands as a fresh
// create with a new UID, Status follows the new identity, and the node carries
// an Event that says the workload came back because the SeiNode still exists.
func TestReconcileStatefulSet_RecreateUnderLiveNodeRecordsReason(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	const previousUID types.UID = "sts-uid-deleted"
	node := newGenesisNode("sts-recreated", "default")
	node.Status.StatefulSet = &seiv1alpha1.StatefulSetRef{Name: node.Name, UID: previousUID}
	r, c := newNodeReconciler(t, node)
	recorder := r.Recorder.(*record.FakeRecorder)

	g.Expect(r.reconcileStatefulSet(ctx, node)).To(Succeed())

	live := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, live)).To(Succeed())
	g.Expect(node.Status.StatefulSet).NotTo(BeNil())
	g.Expect(node.Status.StatefulSet.UID).To(Equal(live.UID))
	g.Expect(node.Status.StatefulSet.UID).NotTo(Equal(previousUID))

	var event string
	g.Expect(recorder.Events).To(Receive(&event))
	g.Expect(event).To(ContainSubstring("StatefulSetRecreated"))
	g.Expect(event).To(ContainSubstring(string(previousUID)))
	g.Expect(event).To(ContainSubstring("SeiNode sts-recreated still exists"))
	g.Expect(recorder.Events).NotTo(Receive(), "one recreate, one record")
}

// Steady state: tracked UID matches the live object. Re-applying is not a
// recreate and must not be reported as one.
func TestReconcileStatefulSet_MatchingUIDRecordsNothing(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("sts-steady", "default")
	r, _ := newNodeReconciler(t, node)
	recorder := r.Recorder.(*record.FakeRecorder)

	g.Expect(r.reconcileStatefulSet(ctx, node)).To(Succeed())
	g.Expect(recorder.Events).NotTo(Receive())

	g.Expect(r.reconcileStatefulSet(ctx, node)).To(Succeed())
	g.Expect(recorder.Events).NotTo(Receive(), "a re-apply onto the tracked UID is not a recreate")
}
