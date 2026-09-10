package seinetwork

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

// A network with no scheduling block creates children with none: the field has
// no default, so an unset value must stay unset all the way down.
func TestEnsureSeiNode_UnsetSchedulingStaysUnset(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	r := newPlanTestReconciler(t, network)
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	child := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}, child)).To(Succeed())
	g.Expect(child.Spec.Scheduling).To(BeNil())
}

// spec.scheduling is copied onto a child at create and re-synced on every
// later reconcile, both a change and a clear, through the same path as the
// sidecar and config fields.
func TestEnsureSeiNode_PropagatesScheduling(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	network.Spec.Scheduling = &seiv1alpha1.SchedulingConfig{NodeIsolation: seiv1alpha1.NodeIsolationDedicated}
	r := newPlanTestReconciler(t, network)

	childKey := types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}
	child := &seiv1alpha1.SeiNode{}

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.Spec.Scheduling).To(Equal(network.Spec.Scheduling))
	g.Expect(noderesource.IsDedicatedNode(child)).To(BeTrue())

	network.Spec.Scheduling.NodeIsolation = seiv1alpha1.NodeIsolationShared
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.Spec.Scheduling.NodeIsolation).To(Equal(seiv1alpha1.NodeIsolationShared))

	network.Spec.Scheduling = nil
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.Spec.Scheduling).To(BeNil())
}

func validatorPod(name, network, child, workerNode string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				seinetworkLabel:        network,
				noderesource.NodeLabel: child,
			},
		},
		Spec:   corev1.PodSpec{NodeName: workerNode},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// status.nodes reports each validator's worker node from its live pod, and
// Pending while no pod is bound — a child with no pod yet, or one whose pod
// the scheduler cannot place.
func TestUpdateStatus_ReportsWorkerNodePlacement(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	network.Spec.Replicas = 3
	r := newPlanTestReconciler(t, network)
	for i := range 3 {
		g.Expect(r.ensureSeiNode(ctx, network, i)).To(Succeed())
	}

	g.Expect(r.Create(ctx, validatorPod("syncer-0-0", "syncer", "syncer-0", "ip-10-0-0-1", corev1.PodRunning))).To(Succeed())
	// Unschedulable: exists, no binding.
	g.Expect(r.Create(ctx, validatorPod("syncer-1-0", "syncer", "syncer-1", "", corev1.PodPending))).To(Succeed())
	// A finished bootstrap Job pod keeps its nodeName but is not the validator.
	g.Expect(r.Create(ctx, validatorPod("syncer-2-bootstrap", "syncer", "syncer-2", "ip-10-0-0-9", corev1.PodSucceeded))).To(Succeed())
	// Another network's pod on the same namespace must not leak in.
	g.Expect(r.Create(ctx, validatorPod("other-0-0", "other", "syncer-2", "ip-10-0-0-8", corev1.PodRunning))).To(Succeed())

	statusBase := client.MergeFromWithOptions(network.DeepCopy(), client.MergeFromWithOptimisticLock{})
	g.Expect(r.updateStatus(ctx, network, statusBase)).To(Succeed())

	g.Expect(network.Status.Nodes).To(HaveLen(3))
	byName := map[string]seiv1alpha1.GroupNodeStatus{}
	for _, n := range network.Status.Nodes {
		byName[n.Name] = n
	}
	g.Expect(byName["syncer-0"].WorkerNode).To(Equal("ip-10-0-0-1"))
	g.Expect(byName["syncer-0"].Placement).To(Equal(seiv1alpha1.PlacementScheduled))
	g.Expect(byName["syncer-1"].WorkerNode).To(BeEmpty())
	g.Expect(byName["syncer-1"].Placement).To(Equal(seiv1alpha1.PlacementPending))
	g.Expect(byName["syncer-2"].WorkerNode).To(BeEmpty())
	g.Expect(byName["syncer-2"].Placement).To(Equal(seiv1alpha1.PlacementPending))

	// A reschedule shows up on the next status pass.
	pod := &corev1.Pod{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: "syncer-1-0", Namespace: testNamespace}, pod)).To(Succeed())
	pod.Spec.NodeName = "ip-10-0-0-2"
	g.Expect(r.Update(ctx, pod)).To(Succeed())

	g.Expect(r.Get(ctx, types.NamespacedName{Name: "syncer", Namespace: testNamespace}, network)).To(Succeed())
	statusBase = client.MergeFromWithOptions(network.DeepCopy(), client.MergeFromWithOptimisticLock{})
	g.Expect(r.updateStatus(ctx, network, statusBase)).To(Succeed())
	for _, n := range network.Status.Nodes {
		if n.Name == "syncer-1" {
			g.Expect(n.WorkerNode).To(Equal("ip-10-0-0-2"))
			g.Expect(n.Placement).To(Equal(seiv1alpha1.PlacementScheduled))
		}
	}
}

func TestPodToSeiNetwork_MapsByLabel(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	g.Expect(podToSeiNetwork(ctx, validatorPod("p", "syncer", "syncer-0", "", corev1.PodPending))).To(Equal(
		[]reconcile.Request{{NamespacedName: types.NamespacedName{Name: "syncer", Namespace: testNamespace}}}))
	g.Expect(podToSeiNetwork(ctx, &corev1.Pod{})).To(BeEmpty())
}
