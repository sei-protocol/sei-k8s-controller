package noderesource

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

const switchName = "nc-switch"

func switchedNode() *seiv1alpha1.SeiNode {
	node := newSnapshotNode(switchName, testNamespace)
	node.Spec.FullNode.Snapshot = nil
	node.Spec.NodeConfig = &seiv1alpha1.NodeConfig{
		ConfigRef: seiv1alpha1.ConfigFileRef{Name: "nc-switch-config-v1"},
		AppRef:    seiv1alpha1.ConfigFileRef{Name: "nc-switch-config-v1"},
	}
	node.Status.StatefulSet = &seiv1alpha1.StatefulSetRef{Name: switchName, UID: originalTestUID}
	return node
}

func liveStatefulSet(policy appsv1.PodManagementPolicyType) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: switchName, Namespace: testNamespace, UID: originalTestUID},
		Spec:       appsv1.StatefulSetSpec{PodManagementPolicy: policy},
	}
}

// Spec 013 Req 1.2: a node that gained spec.nodeConfig gets its StatefulSet
// deleted with orphan propagation, so the pod and the data PVC stay; the next
// sync applies the Parallel StatefulSet.
func TestSyncStatefulSet_NodeConfigSwitchRecreatesWithOrphan(t *testing.T) {
	g := NewWithT(t)
	s := newSyncTestScheme(t)
	node := switchedNode()

	var propagation *metav1.DeletionPropagation
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node, liveStatefulSet(appsv1.OrderedReadyPodManagement)).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				o := &client.DeleteOptions{}
				o.ApplyOptions(opts)
				propagation = o.PropagationPolicy
				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	sts, err := SyncStatefulSet(context.Background(), c, s, node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).To(BeNil(), "the switch returns nil so the next reconcile applies")
	g.Expect(node.Status.StatefulSet).To(BeNil())
	g.Expect(propagation).NotTo(BeNil())
	g.Expect(*propagation).To(Equal(metav1.DeletePropagationOrphan))

	live := &appsv1.StatefulSet{}
	err = c.Get(context.Background(), types.NamespacedName{Name: switchName, Namespace: testNamespace}, live)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the old StatefulSet must be gone; got err=%v", err)

	sts, err = SyncStatefulSet(context.Background(), c, s, node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).NotTo(BeNil())
	g.Expect(sts.Spec.PodManagementPolicy).To(Equal(appsv1.ParallelPodManagement))
	g.Expect(sts.Spec.UpdateStrategy.Type).To(Equal(appsv1.RollingUpdateStatefulSetStrategyType))
}

// Spec 013 Req 1.3: an unchanged policy leaves the StatefulSet in place. A
// rendered empty policy equals the live API default.
func TestSyncStatefulSet_SamePolicyKeepsStatefulSet(t *testing.T) {
	cases := []struct {
		name string
		node func() *seiv1alpha1.SeiNode
		live appsv1.PodManagementPolicyType
	}{
		{"nodeConfig node on Parallel", switchedNode, appsv1.ParallelPodManagement},
		{"controller-configured node on the API default", func() *seiv1alpha1.SeiNode {
			n := switchedNode()
			n.Spec.NodeConfig = nil
			return n
		}, appsv1.OrderedReadyPodManagement},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			s := newSyncTestScheme(t)
			node := tc.node()
			c := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(node, liveStatefulSet(tc.live)).
				WithStatusSubresource(&seiv1alpha1.SeiNode{}).
				Build()

			sts, err := SyncStatefulSet(context.Background(), c, s, node, platformtest.Config())
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(sts).NotTo(BeNil())
			g.Expect(sts.UID).To(Equal(types.UID(originalTestUID)), "no delete may occur")
		})
	}
}

// Spec 013 Req 1.4: while the orphaned StatefulSet terminates, the sync waits
// instead of applying over it.
func TestSyncStatefulSet_WaitsForTerminatingStatefulSet(t *testing.T) {
	g := NewWithT(t)
	s := newSyncTestScheme(t)
	node := switchedNode()
	node.Status.StatefulSet = nil

	terminating := liveStatefulSet(appsv1.OrderedReadyPodManagement)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{metav1.FinalizerOrphanDependents}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node, terminating).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()

	sts, err := SyncStatefulSet(context.Background(), c, s, node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sts).To(BeNil())

	live := &appsv1.StatefulSet{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: switchName, Namespace: testNamespace}, live)).To(Succeed())
	g.Expect(live.Spec.PodManagementPolicy).To(Equal(appsv1.OrderedReadyPodManagement), "no apply may patch the terminating object")
}
