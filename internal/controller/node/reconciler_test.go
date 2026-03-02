package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-node-controller/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// Fake client helpers
// ---------------------------------------------------------------------------

func newNodeTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
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
	return &SeiNodeReconciler{Client: c, Scheme: s}, c
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

// createReadyPod creates a Pod in the fake client that looks ready for a given node.
func createReadyPod(t *testing.T, ctx context.Context, c client.Client, name, namespace string, labels map[string]string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("creating pod %s: %v", name, err)
	}
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		},
	}
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatalf("updating pod status %s: %v", name, err)
	}
}

// createFailedPod creates a Pod in the Failed phase.
func createFailedPod(t *testing.T, ctx context.Context, c client.Client, name, namespace string, labels map[string]string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("creating failed pod: %v", err)
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodFailed}
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatalf("updating failed pod status: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Basic reconcile
// ---------------------------------------------------------------------------

func TestNodeReconcile_NotFound(t *testing.T) {
	g := NewWithT(t)
	r, _ := newNodeReconciler(t)
	ctx := context.Background()

	res, err := r.Reconcile(ctx, nodeReqFor("does-not-exist", "default"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
}

// ---------------------------------------------------------------------------
// Genesis node reconcile
// ---------------------------------------------------------------------------

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
	g.Expect(sts.Spec.Template.Spec.InitContainers).To(BeEmpty())

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

// ---------------------------------------------------------------------------
// Snapshot node reconcile (non-sidecar: no controller-side peer discovery)
// ---------------------------------------------------------------------------

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

func TestNodeReconcile_SnapshotNode_StatefulSetHasInitContainer(t *testing.T) {
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
	g.Expect(sts.Spec.Template.Spec.InitContainers[1].Name).To(Equal("snapshot-restore"))
}

func TestNodeReconcile_SnapshotNode_InitContainerHasS3Env(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newSnapshotNode("snap-0", "default")
	r, c := newNodeReconciler(t, node)

	_, err := r.Reconcile(ctx, nodeReqFor("snap-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "snap-0", Namespace: "default"}, sts)).To(Succeed())

	initEnv := sts.Spec.Template.Spec.InitContainers[1].Env
	g.Expect(envValue(initEnv, "S3_BUCKET")).To(Equal("sei-snapshots"))
	g.Expect(envValue(initEnv, "S3_REGION")).To(Equal("eu-central-1"))
	g.Expect(envValue(initEnv, "CHAIN_ID")).To(Equal("sei-test"))
}

// ---------------------------------------------------------------------------
// Status updates
// ---------------------------------------------------------------------------

func TestNodeReconcile_Status_ReadyPod_RunningPhase(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("mynet-0", "default")
	r, c := newNodeReconciler(t, node)

	_, _ = r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))

	labels := resourceLabelsForNode(node)
	createReadyPod(t, ctx, c, "mynet-0-pod", "default", labels)

	_, err := r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "mynet-0", "default")
	g.Expect(fetched.Status.Phase).To(Equal("Running"))
}

func TestNodeReconcile_Status_FailedPod_FailedPhase(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("mynet-0", "default")
	r, c := newNodeReconciler(t, node)

	_, _ = r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))

	labels := resourceLabelsForNode(node)
	createFailedPod(t, ctx, c, "mynet-0-pod", "default", labels)

	_, err := r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "mynet-0", "default")
	g.Expect(fetched.Status.Phase).To(Equal("Failed"))
}

func TestNodeReconcile_Status_NoPods_PendingPhase(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("mynet-0", "default")
	r, c := newNodeReconciler(t, node)

	_, err := r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "mynet-0", "default")
	g.Expect(fetched.Status.Phase).To(Equal("Pending"))
}

// ---------------------------------------------------------------------------
// Deletion handling
// ---------------------------------------------------------------------------

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
