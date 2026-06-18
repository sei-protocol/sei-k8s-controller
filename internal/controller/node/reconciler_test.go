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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
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
	return newNodeReconcilerWithSidecar(t, &mockSidecarClient{nodeID: "mock-node-id"}, objs...)
}

func newNodeReconcilerWithSidecar(t *testing.T, mock *mockSidecarClient, objs ...client.Object) (*SeiNodeReconciler, client.Client) {
	t.Helper()
	s := newNodeTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()
	r := &SeiNodeReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(100),
		Platform: platformtest.Config(),
		Planner: &planner.NodeResolver{
			BuildSidecarClient: func(_ *seiv1alpha1.SeiNode) (task.SidecarClient, error) { return mock, nil },
			Platform:           platformtest.Config(),
		},
		PlanExecutor: &planner.Executor[*seiv1alpha1.SeiNode]{
			ConfigFor: func(_ context.Context, node *seiv1alpha1.SeiNode) task.ExecutionConfig {
				return task.ExecutionConfig{
					BuildSidecarClient: func() (task.SidecarClient, error) { return mock, nil },
					KubeClient:         c,
					APIReader:          c,
					Scheme:             s,
					Resource:           node,
					Platform:           platformtest.Config(),
				}
			},
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

const (
	testImageV2  = "ghcr.io/sei-protocol/seid:v2.0.0"
	testRevision = "rev-2"
)

func TestNodeReconcile_NotFound(t *testing.T) {
	g := NewWithT(t)
	r, _ := newNodeReconciler(t)
	ctx := context.Background()

	res, err := r.Reconcile(ctx, nodeReqFor("does-not-exist", "default"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
}

func TestNodeReconcile_ValidatorNode_CreateStatefulSetAndService(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("mynet-0", "default")
	r, c := newNodeReconciler(t, node)

	// Drive through Pending -> Initializing -> infrastructure tasks.
	// Plan: ensure-data-pvc, apply-statefulset, apply-service, then sidecar tasks.
	// Each reconcile drives one step forward.
	for range 8 {
		_, err := r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))
		g.Expect(err).NotTo(HaveOccurred())
	}

	// StatefulSet created
	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "mynet-0", Namespace: "default"}, sts)).To(Succeed())
	g.Expect(*sts.Spec.Replicas).To(Equal(int32(1)))

	// Service created
	svc := &corev1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "mynet-0", Namespace: "default"}, svc)).To(Succeed())
	g.Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
}

func TestNodeReconcile_AddsFinalizer(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newSnapshotNode("snap-0", "default")
	r, c := newNodeReconciler(t, node)

	_, _ = r.Reconcile(ctx, nodeReqFor("snap-0", "default"))

	fetched := getSeiNode(t, ctx, c, "snap-0", "default")
	g.Expect(fetched.Finalizers).To(ContainElement(nodeFinalizerName))
}

func TestNodeReconcile_StatefulSet_Idempotent(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newSnapshotNode("snap-0", "default")
	r, c := newNodeReconciler(t, node)

	// Drive through Pending -> Initializing -> infrastructure tasks, then one more for idempotency.
	for range 8 {
		_, err := r.Reconcile(ctx, nodeReqFor("snap-0", "default"))
		g.Expect(err).NotTo(HaveOccurred())
	}

	stsList := &appsv1.StatefulSetList{}
	g.Expect(c.List(ctx, stsList, client.InNamespace("default"))).To(Succeed())
	g.Expect(stsList.Items).To(HaveLen(1))
}

func TestNodeReconcile_SnapshotNode_CreatesPVC(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newSnapshotNode("snap-0", "default")
	r, c := newNodeReconciler(t, node)

	// Reconcile 1: finalizer + build plan. Reconcile 2: execute ensure-data-pvc.
	for range 3 {
		_, err := r.Reconcile(ctx, nodeReqFor("snap-0", "default"))
		g.Expect(err).NotTo(HaveOccurred())
	}

	pvc := &corev1.PersistentVolumeClaim{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "data-snap-0", Namespace: "default"}, pvc)).To(Succeed())
	g.Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce))
}

func TestNodeReconcile_SnapshotNode_StatefulSetHasInitContainers(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newSnapshotNode("snap-0", "default")
	r, c := newNodeReconciler(t, node)

	// Drive through Pending -> Initializing -> infrastructure tasks.
	for range 8 {
		_, err := r.Reconcile(ctx, nodeReqFor("snap-0", "default"))
		g.Expect(err).NotTo(HaveOccurred())
	}

	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "snap-0", Namespace: "default"}, sts)).To(Succeed())
	g.Expect(sts.Spec.Template.Spec.InitContainers).To(HaveLen(3))
	g.Expect(sts.Spec.Template.Spec.InitContainers[0].Name).To(Equal("seid-init"))
	g.Expect(sts.Spec.Template.Spec.InitContainers[1].Name).To(Equal("sei-sidecar"))
	g.Expect(sts.Spec.Template.Spec.InitContainers[2].Name).To(Equal("kube-rbac-proxy"))
}

// runningFullNode returns a fullNode pre-seeded to Running with its STS already
// present, mirroring TestNodeReconcile_RunningPhase_UpdatesStatefulSetImage. Used
// to exercise the endpoint-status derivation, which only fires once Running.
func runningFullNode(t *testing.T, name, namespace string) (*seiv1alpha1.SeiNode, *appsv1.StatefulSet) {
	t.Helper()
	node := newSnapshotNode(name, namespace) // fullNode shape
	node.Spec.FullNode.Snapshot = nil        // plain follower, no restore
	node.Finalizers = []string{nodeFinalizerName}
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.CurrentImage = node.Spec.Image

	sts, err := noderesource.GenerateStatefulSet(node, platformtest.Config())
	if err != nil {
		t.Fatalf("generating statefulset: %v", err)
	}
	sts.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
	return node, sts
}

func TestNodeReconcile_RunningFullNode_SetsEndpoint(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node, sts := runningFullNode(t, "chaos-rpc", "sei")
	svc := noderesource.GenerateHeadlessService(node)
	svc.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	r, c := newNodeReconciler(t, node, sts, svc)

	_, err := r.Reconcile(ctx, nodeReqFor("chaos-rpc", "sei"))
	g.Expect(err).NotTo(HaveOccurred())

	got := getSeiNode(t, ctx, c, "chaos-rpc", "sei")
	g.Expect(got.Status.Endpoint).NotTo(BeNil())
	g.Expect(got.Status.Endpoint.EvmJsonRpc).To(Equal("http://chaos-rpc.sei.svc:8545"))
	g.Expect(got.Status.Endpoint.EvmWs).To(Equal("ws://chaos-rpc.sei.svc:8546"))
	g.Expect(got.Status.Endpoint.TendermintRpc).To(Equal("http://chaos-rpc.sei.svc:26657"))
	g.Expect(got.Status.Endpoint.TendermintRest).To(Equal("http://chaos-rpc.sei.svc:1317"))

	// The headless Service the URL resolves to exists with a :8545 port.
	gotSvc := &corev1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "chaos-rpc", Namespace: "sei"}, gotSvc)).To(Succeed())
	g.Expect(gotSvc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
	var has8545 bool
	for _, p := range gotSvc.Spec.Ports {
		if p.Port == 8545 {
			has8545 = true
		}
	}
	g.Expect(has8545).To(BeTrue(), "headless Service should expose :8545")
}

func TestNodeReconcile_RunningValidator_NoEndpoint(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("genesis-val-0", "sei")
	node.Finalizers = []string{nodeFinalizerName}
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.CurrentImage = node.Spec.Image

	sts, err := noderesource.GenerateStatefulSet(node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	sts.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("StatefulSet"))

	r, c := newNodeReconciler(t, node, sts)

	_, err = r.Reconcile(ctx, nodeReqFor("genesis-val-0", "sei"))
	g.Expect(err).NotTo(HaveOccurred())

	got := getSeiNode(t, ctx, c, "genesis-val-0", "sei")
	g.Expect(got.Status.Endpoint).To(BeNil())
}

func TestNodeReconcile_Endpoint_StableAcrossReconciles(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node, sts := runningFullNode(t, "chaos-rpc", "sei")
	r, c := newNodeReconciler(t, node, sts)

	_, err := r.Reconcile(ctx, nodeReqFor("chaos-rpc", "sei"))
	g.Expect(err).NotTo(HaveOccurred())
	first := getSeiNode(t, ctx, c, "chaos-rpc", "sei").Status.Endpoint
	g.Expect(first).NotTo(BeNil())

	// Second reconcile: identity-derived URLs are unchanged (no churn).
	_, err = r.Reconcile(ctx, nodeReqFor("chaos-rpc", "sei"))
	g.Expect(err).NotTo(HaveOccurred())
	second := getSeiNode(t, ctx, c, "chaos-rpc", "sei").Status.Endpoint
	g.Expect(second).To(Equal(first))
}

func TestNodeReconcile_RunningPhase_UpdatesStatefulSetImage(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newGenesisNode("mynet-0", "default")
	node.Finalizers = []string{nodeFinalizerName}
	node.Status.Phase = seiv1alpha1.PhaseRunning

	// Pre-create a StatefulSet with the old image.
	oldSts, err := noderesource.GenerateStatefulSet(node, platformtest.Config())
	g.Expect(err).NotTo(HaveOccurred())
	oldSts.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("StatefulSet"))

	r, c := newNodeReconciler(t, node, oldSts)

	// Verify old image on the StatefulSet.
	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "mynet-0", Namespace: "default"}, sts)).To(Succeed())
	seid := findContainer(sts.Spec.Template.Spec.Containers, "seid")
	g.Expect(seid.Image).To(Equal("ghcr.io/sei-protocol/seid:latest"))

	// Update the image on the SeiNode spec.
	node = getSeiNode(t, ctx, c, "mynet-0", "default")
	node.Spec.Image = testImageV2
	g.Expect(c.Update(ctx, node)).To(Succeed())

	// Reconcile — this builds a convergence plan and drives apply-statefulset.
	for range 4 {
		_, err := r.Reconcile(ctx, nodeReqFor("mynet-0", "default"))
		g.Expect(err).NotTo(HaveOccurred())
	}

	// StatefulSet should now reflect the new image.
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "mynet-0", Namespace: "default"}, sts)).To(Succeed())
	seid = findContainer(sts.Spec.Template.Spec.Containers, "seid")
	g.Expect(seid.Image).To(Equal(testImageV2))
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
			Labels:    noderesource.ResourceLabels(node),
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

// --- probeSidecarHealth tests ---

func runningSeiNodeForProbe() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "p-0", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "atlantic-2",
			Image:    "sei:v1.0.0",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning, CurrentImage: "sei:v1.0.0"},
	}
}

func findSidecarReady(node *seiv1alpha1.SeiNode) *metav1.Condition {
	for i := range node.Status.Conditions {
		c := &node.Status.Conditions[i]
		if c.Type == seiv1alpha1.ConditionSidecarReady {
			return c
		}
	}
	return nil
}

// Probe outcome unit tests live in the planner package now that the probe
// itself lives there. These tests exercise Reconcile-level integration:
// that the reconciler passes a client to the planner when Phase==Running
// (and skips during Initializing), and that SidecarReady transitions emit
// kubectl events.

func TestReconcile_Running_ProbesAndSetsCondition(t *testing.T) {
	g := NewWithT(t)
	node := runningSeiNodeForProbe()

	r, c := newNodeReconciler(t, node)
	unhealthy := false
	mock := &mockSidecarClient{healthz: &unhealthy}
	r.Planner.BuildSidecarClient = func(_ *seiv1alpha1.SeiNode) (task.SidecarClient, error) { return mock, nil }

	_, err := r.Reconcile(context.Background(), nodeReqFor(node.Name, node.Namespace))
	g.Expect(err).NotTo(HaveOccurred())

	got := getSeiNode(t, context.Background(), c, node.Name, node.Namespace)
	cond := findSidecarReady(got)
	g.Expect(cond).NotTo(BeNil(), "probe should stamp SidecarReady when Running")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("NotReady"))
}

func TestReconcile_Initializing_SkipsProbe(t *testing.T) {
	g := NewWithT(t)
	node := runningSeiNodeForProbe()
	node.Status.Phase = seiv1alpha1.PhaseInitializing

	r, c := newNodeReconciler(t, node)
	unhealthy := false
	mock := &mockSidecarClient{healthz: &unhealthy}
	r.Planner.BuildSidecarClient = func(_ *seiv1alpha1.SeiNode) (task.SidecarClient, error) { return mock, nil }

	_, err := r.Reconcile(context.Background(), nodeReqFor(node.Name, node.Namespace))
	g.Expect(err).NotTo(HaveOccurred())

	got := getSeiNode(t, context.Background(), c, node.Name, node.Namespace)
	g.Expect(findSidecarReady(got)).To(BeNil(),
		"probe must be skipped while Initializing — init plan owns the sidecar")
}
