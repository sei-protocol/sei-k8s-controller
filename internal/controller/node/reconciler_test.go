package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func newNodeTestScheme(t *testing.T) *k8sruntime.Scheme {
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

func newNodeReconciler(t *testing.T, objs ...client.Object) (*SeiNodeReconciler, client.Client) {
	t.Helper()
	s := newNodeTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()
	r := &SeiNodeReconciler{
		Client: c,
		Scheme: s,
		BuildSidecarClientFn: func(_ *seiv1alpha1.SeiNode) SidecarStatusClient {
			return &mockSidecarClient{}
		},
	}
	return r, c
}

func nodeReqFor(name, namespace string) ctrl.Request { //nolint:unparam // test helper designed for reuse
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
}

func getSeiNode(t *testing.T, ctx context.Context, c client.Client, name, namespace string) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	t.Helper()
	node := &seiv1alpha1.SeiNode{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, node); err != nil {
		t.Fatalf("fetching SeiNode %s: %v", name, err)
	}
	return node
}

func TestNodeReconcile_NotFound(t *testing.T) {
	g := NewWithT(t)
	r, _ := newNodeReconciler(t)
	ctx := context.Background()

	res, err := r.Reconcile(ctx, nodeReqFor("does-not-exist", "default"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
}

func TestNodeReconcile_GenesisNode_CreateStatefulSetAndService(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("mynet-0", "default")
	r, c := newNodeReconciler(t, node)

	_, err := r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	// StatefulSet created
	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "mynet-0", Namespace: "default"}, sts)).To(Succeed())
	g.Expect(*sts.Spec.Replicas).To(Equal(int32(1)))

	// Service created
	svc := &corev1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "mynet-0", Namespace: "default"}, svc)).To(Succeed())
	g.Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
}

func TestNodeReconcile_GenesisNode_NoPVCCreated(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("mynet-0", "default")
	r, c := newNodeReconciler(t, node)

	_, err := r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	pvcList := &corev1.PersistentVolumeClaimList{}
	g.Expect(c.List(ctx, pvcList, client.InNamespace("default"))).To(Succeed())
	g.Expect(pvcList.Items).To(BeEmpty())
}

func TestNodeReconcile_GenesisNode_AddsFinalizer(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("mynet-0", "default")
	r, c := newNodeReconciler(t, node)

	_, _ = r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))

	fetched := getSeiNode(t, ctx, c, "mynet-0", "default")
	g.Expect(fetched.Finalizers).To(ContainElement(nodeFinalizerName))
}

func TestNodeReconcile_StatefulSet_Idempotent(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("mynet-0", "default")
	r, c := newNodeReconciler(t, node)

	_, _ = r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))
	_, err := r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	stsList := &appsv1.StatefulSetList{}
	g.Expect(c.List(ctx, stsList, client.InNamespace("default"))).To(Succeed())
	g.Expect(stsList.Items).To(HaveLen(1))
}

func TestNodeReconcile_SnapshotNode_CreatesPVC(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newSnapshotNode("snap-0", "default")
	r, c := newNodeReconciler(t, node)

	_, err := r.Reconcile(ctx, nodeReqFor("snap-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	pvc := &corev1.PersistentVolumeClaim{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "data-snap-0", Namespace: "default"}, pvc)).To(Succeed())
	g.Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))
}

func TestNodeReconcile_SnapshotNode_StatefulSetHasInitContainers(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newSnapshotNode("snap-0", "default")
	r, c := newNodeReconciler(t, node)

	_, err := r.Reconcile(ctx, nodeReqFor("snap-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "snap-0", Namespace: "default"}, sts)).To(Succeed())
	g.Expect(sts.Spec.Template.Spec.InitContainers).To(HaveLen(2))
	g.Expect(sts.Spec.Template.Spec.InitContainers[0].Name).To(Equal("seid-init"))
	g.Expect(sts.Spec.Template.Spec.InitContainers[1].Name).To(Equal("sei-sidecar"))
}

func TestNodeDeletion_GenesisNode_RemovesFinalizerNoPVCDeletion(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("mynet-0", "default")
	node.Finalizers = []string{nodeFinalizerName}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-mynet-0", Namespace: "default"},
	}

	r, c := newNodeReconciler(t, node, pvc)

	g.Expect(c.Delete(ctx, node)).To(Succeed())
	_ = c.Get(ctx, types.NamespacedName{Name: "mynet-0", Namespace: "default"}, node)

	_, err := r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	remaining := &corev1.PersistentVolumeClaim{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "data-mynet-0", Namespace: "default"}, remaining)).To(Succeed())

	fetched := &seiv1alpha1.SeiNode{}
	_ = c.Get(ctx, types.NamespacedName{Name: "mynet-0", Namespace: "default"}, fetched)
	g.Expect(fetched.Finalizers).NotTo(ContainElement(nodeFinalizerName))
}

func TestNodeDeletion_SnapshotNode_WithoutRetain_DeletesPVC(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newSnapshotNode("snap-0", "default")
	node.Finalizers = []string{nodeFinalizerName}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-snap-0",
			Namespace: "default",
			Labels:    resourceLabelsForNode(node),
		},
	}

	r, c := newNodeReconciler(t, node, pvc)
	g.Expect(c.Delete(ctx, node)).To(Succeed())
	_ = c.Get(ctx, types.NamespacedName{Name: "snap-0", Namespace: "default"}, node)

	_, err := r.Reconcile(ctx, nodeReqFor("snap-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	remaining := &corev1.PersistentVolumeClaim{}
	err = c.Get(ctx, types.NamespacedName{Name: "data-snap-0", Namespace: "default"}, remaining)
	g.Expect(err).To(HaveOccurred())
}

func TestNodeDeletion_SnapshotNode_WithRetain_KeepsPVC(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newSnapshotNode("snap-0", "default")
	node.Spec.Storage.RetainOnDelete = true
	node.Finalizers = []string{nodeFinalizerName}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-snap-0",
			Namespace: "default",
			Labels:    resourceLabelsForNode(node),
		},
	}

	r, c := newNodeReconciler(t, node, pvc)
	g.Expect(c.Delete(ctx, node)).To(Succeed())
	_ = c.Get(ctx, types.NamespacedName{Name: "snap-0", Namespace: "default"}, node)

	_, err := r.Reconcile(ctx, nodeReqFor("snap-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	remaining := &corev1.PersistentVolumeClaim{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "data-snap-0", Namespace: "default"}, remaining)).To(Succeed())
}
