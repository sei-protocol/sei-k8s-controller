package nodepool

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
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

func newTestReconciler(t *testing.T, objs ...client.Object) (*SeiNodePoolReconciler, client.Client) {
	t.Helper()
	s := newTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNodePool{}, &seiv1alpha1.SeiNode{}).
		Build()
	return &SeiNodePoolReconciler{Client: c, Scheme: s}, c
}

func reqFor(name, namespace string) ctrl.Request { //nolint:unparam // test helper designed for reuse
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
}

func markJobComplete(t *testing.T, ctx context.Context, c client.Client, name, namespace string) { //nolint:unparam // test helper designed for reuse
	t.Helper()
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, job); err != nil {
		t.Fatalf("fetching job %s: %v", name, err)
	}
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type:   batchv1.JobComplete,
		Status: corev1.ConditionTrue,
	})
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatalf("updating job status %s: %v", name, err)
	}
}

func markJobFailed(t *testing.T, ctx context.Context, c client.Client, name, namespace string) {
	t.Helper()
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, job); err != nil {
		t.Fatalf("fetching job %s: %v", name, err)
	}
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type:    batchv1.JobFailed,
		Status:  corev1.ConditionTrue,
		Message: "backoff limit exceeded",
	})
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatalf("updating job status %s: %v", name, err)
	}
}

func getSeiNodePool(t *testing.T, ctx context.Context, c client.Client, name, namespace string) *seiv1alpha1.SeiNodePool { //nolint:unparam // test helper designed for reuse
	t.Helper()
	sn := &seiv1alpha1.SeiNodePool{}
	if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, sn); err != nil {
		t.Fatalf("fetching SeiNodePool: %v", err)
	}
	return sn
}

func TestReconcile_NotFound(t *testing.T) {
	g := NewWithT(t)
	r, _ := newTestReconciler(t)
	ctx := context.Background()

	res, err := r.Reconcile(ctx, reqFor("does-not-exist", "default"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
}

func TestReconcile_FirstCall_AddsFinalizer(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sn := newSeiNodePool("testnet", "default", 1)
	r, c := newTestReconciler(t, sn)

	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))

	fetched := getSeiNodePool(t, ctx, c, "testnet", "default")
	g.Expect(fetched.Finalizers).To(ContainElement(finalizerName))
}

func TestReconcile_FirstCall_CreatesPreparationResources(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sn := newSeiNodePool("testnet", "default", 2)
	r, c := newTestReconciler(t, sn)

	res, err := r.Reconcile(ctx, reqFor("testnet", "default"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(genesisJobPollInterval))

	// Genesis PVC
	genesisPVC := &corev1.PersistentVolumeClaim{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "testnet-genesis-data", Namespace: "default"}, genesisPVC)).To(Succeed())

	// Data PVCs (one per node)
	for i := range 2 {
		dataPVC := &corev1.PersistentVolumeClaim{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: dataPVCName(sn, i), Namespace: "default"}, dataPVC)).To(Succeed())
	}

	// Genesis script ConfigMap
	cm := &corev1.ConfigMap{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "testnet-genesis-script", Namespace: "default"}, cm)).To(Succeed())
	g.Expect(cm.Data).To(HaveKey("generate.sh"))

	// Genesis Job
	job := &batchv1.Job{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "testnet-genesis", Namespace: "default"}, job)).To(Succeed())
}

func TestReconcile_FirstCall_NoSeiNodesYet(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sn := newSeiNodePool("testnet", "default", 2)
	r, c := newTestReconciler(t, sn)

	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))

	nodeList := &seiv1alpha1.SeiNodeList{}
	g.Expect(c.List(ctx, nodeList)).To(Succeed())
	g.Expect(nodeList.Items).To(BeEmpty())
}

func TestReconcile_GenesisJobPending_DoesNotCreatePrepJobs(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sn := newSeiNodePool("testnet", "default", 1)
	r, c := newTestReconciler(t, sn)

	// First reconcile: creates genesis job (pending)
	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))

	// Second reconcile: genesis job still pending
	res, err := r.Reconcile(ctx, reqFor("testnet", "default"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(genesisJobPollInterval))

	// No prep jobs yet
	prepJob := &batchv1.Job{}
	err = c.Get(ctx, types.NamespacedName{Name: "testnet-prep-0", Namespace: "default"}, prepJob)
	g.Expect(err).To(HaveOccurred())
}

func TestReconcile_GenesisJobComplete_CreatesPrepJobs(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sn := newSeiNodePool("testnet", "default", 2)
	r, c := newTestReconciler(t, sn)

	// First reconcile: creates genesis job
	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))

	// Mark genesis job complete
	markJobComplete(t, ctx, c, "testnet-genesis", "default")

	// Reconcile: genesis complete → creates prep jobs (which are initially pending)
	res, err := r.Reconcile(ctx, reqFor("testnet", "default"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(genesisJobPollInterval))

	for i := range 2 {
		prepJob := &batchv1.Job{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: prepJobName(sn, i), Namespace: "default"}, prepJob)).To(Succeed())
	}
}

func TestReconcile_AllJobsComplete_CreatesSeiNodes(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sn := newSeiNodePool("testnet", "default", 2)
	r, c := newTestReconciler(t, sn)

	// First reconcile: creates genesis job + data/genesis PVCs
	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))

	// Mark genesis job complete and reconcile to create prep jobs
	markJobComplete(t, ctx, c, "testnet-genesis", "default")
	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))

	// Mark prep jobs complete
	markJobComplete(t, ctx, c, "testnet-prep-0", "default")
	markJobComplete(t, ctx, c, "testnet-prep-1", "default")

	// Reconcile: all done → creates SeiNodes
	_, err := r.Reconcile(ctx, reqFor("testnet", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	for i := range 2 {
		node := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: seiNodeName(sn, i), Namespace: "default"}, node)).To(Succeed())
		g.Expect(node.Spec.ChainID).To(Equal("sei-test"))
		g.Expect(node.Spec.Genesis.PVC).NotTo(BeNil())
		g.Expect(node.Spec.Genesis.PVC.DataPVC).To(Equal(dataPVCName(sn, i)))
	}
}

func TestReconcile_AllJobsComplete_CreatesNetworkPolicy(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sn := newSeiNodePool("testnet", "default", 1)
	r, c := newTestReconciler(t, sn)

	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))
	markJobComplete(t, ctx, c, "testnet-genesis", "default")
	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))
	markJobComplete(t, ctx, c, "testnet-prep-0", "default")

	_, err := r.Reconcile(ctx, reqFor("testnet", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	np := &networkingv1.NetworkPolicy{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "testnet", Namespace: "default"}, np)).To(Succeed())
	g.Expect(np.Spec.Ingress).To(HaveLen(1))
	g.Expect(np.Spec.Ingress[0].From[0].PodSelector.MatchLabels).To(
		HaveKeyWithValue(nodepoolLabel, "testnet"),
	)
}

func TestReconcile_AllJobsComplete_SeiNodesIdempotent(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sn := newSeiNodePool("testnet", "default", 1)
	r, c := newTestReconciler(t, sn)

	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))
	markJobComplete(t, ctx, c, "testnet-genesis", "default")
	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))
	markJobComplete(t, ctx, c, "testnet-prep-0", "default")

	// Two reconciles after all jobs complete — SeiNode count must not double
	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))
	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))

	nodeList := &seiv1alpha1.SeiNodeList{}
	g.Expect(c.List(ctx, nodeList)).To(Succeed())
	g.Expect(nodeList.Items).To(HaveLen(1))
}

func TestReconcile_GenesisJobFailed_SetsFailedStatus(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sn := newSeiNodePool("testnet", "default", 1)
	r, c := newTestReconciler(t, sn)

	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))
	markJobFailed(t, ctx, c, "testnet-genesis", "default")

	_, err := r.Reconcile(ctx, reqFor("testnet", "default"))
	g.Expect(err).To(HaveOccurred())

	fetched := getSeiNodePool(t, ctx, c, "testnet", "default")
	g.Expect(fetched.Status.Phase).To(Equal("Failed"))

	cond := findCondition(fetched.Status.Conditions, ConditionTypeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(ReasonGenesisJobFailed))
}

func TestReconcile_PrepJobFailed_SetsFailedStatus(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	sn := newSeiNodePool("testnet", "default", 1)
	r, c := newTestReconciler(t, sn)

	_, _ = r.Reconcile(ctx, reqFor("testnet", "default"))
	markJobComplete(t, ctx, c, "testnet-genesis", "default")
	_, _ = r.Reconcile(ctx, reqFor("testnet", "default")) // creates prep job
	markJobFailed(t, ctx, c, "testnet-prep-0", "default")

	_, err := r.Reconcile(ctx, reqFor("testnet", "default"))
	g.Expect(err).To(HaveOccurred())

	fetched := getSeiNodePool(t, ctx, c, "testnet", "default")
	cond := findCondition(fetched.Status.Conditions, ConditionTypeReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal(ReasonPrepJobFailed))
}

func TestUpdateStatus_AggregatesSeiNodePhases(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	sn := newSeiNodePool("testnet", "default", 2)
	sn.Finalizers = []string{finalizerName}

	// Pre-create SeiNodes with phases
	labels := resourceLabels(sn)
	node0 := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "testnet-0", Namespace: "default", Labels: labels},
		Status:     seiv1alpha1.SeiNodeStatus{Phase: "Running"},
	}
	node1 := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "testnet-1", Namespace: "default", Labels: labels},
		Status:     seiv1alpha1.SeiNodeStatus{Phase: "Pending"},
	}

	r, c := newTestReconciler(t, sn, node0, node1)

	// Update SeiNode statuses (needed because WithStatusSubresource separates them)
	n0 := &seiv1alpha1.SeiNode{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "testnet-0", Namespace: "default"}, n0)).To(Succeed())
	n0.Status.Phase = seiv1alpha1.PhaseRunning
	g.Expect(c.Status().Update(ctx, n0)).To(Succeed())

	n1 := &seiv1alpha1.SeiNode{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "testnet-1", Namespace: "default"}, n1)).To(Succeed())
	n1.Status.Phase = seiv1alpha1.PhasePending
	g.Expect(c.Status().Update(ctx, n1)).To(Succeed())

	g.Expect(r.updateStatus(ctx, sn)).To(Succeed())

	fetched := getSeiNodePool(t, ctx, c, "testnet", "default")
	g.Expect(fetched.Status.TotalNodes).To(Equal(int32(2)))
	g.Expect(fetched.Status.ReadyNodes).To(Equal(int32(1)))
	g.Expect(fetched.Status.Phase).To(Equal("Pending"))
}

func TestUpdateStatus_AllNodesRunning(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	sn := newSeiNodePool("testnet", "default", 2)
	sn.Finalizers = []string{finalizerName}
	labels := resourceLabels(sn)

	node0 := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "testnet-0", Namespace: "default", Labels: labels},
	}
	node1 := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "testnet-1", Namespace: "default", Labels: labels},
	}

	r, c := newTestReconciler(t, sn, node0, node1)

	for _, name := range []string{"testnet-0", "testnet-1"} {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, n)).To(Succeed())
		n.Status.Phase = seiv1alpha1.PhaseRunning
		g.Expect(c.Status().Update(ctx, n)).To(Succeed())
	}

	g.Expect(r.updateStatus(ctx, sn)).To(Succeed())

	fetched := getSeiNodePool(t, ctx, c, "testnet", "default")
	g.Expect(fetched.Status.Phase).To(Equal("Running"))
	g.Expect(fetched.Status.ReadyNodes).To(Equal(int32(2)))
}

func TestHandleDeletion_WithoutRetain_DeletesPVCs(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// SeiNodePool with finalizer already set
	sn := newSeiNodePool("testnet", "default", 1)
	sn.Finalizers = []string{finalizerName}

	// Pre-create a data PVC labelled to the pool
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-testnet-0",
			Namespace: "default",
			Labels:    resourceLabels(sn),
		},
	}

	r, c := newTestReconciler(t, sn, pvc)

	// Trigger deletion — fake client sets DeletionTimestamp on objects with finalizers
	g.Expect(c.Delete(ctx, sn)).To(Succeed())

	// Fetch to get the DeletionTimestamp-bearing version
	_ = c.Get(ctx, types.NamespacedName{Name: "testnet", Namespace: "default"}, sn)

	_, err := r.Reconcile(ctx, reqFor("testnet", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	// PVC should be deleted
	remaining := &corev1.PersistentVolumeClaim{}
	err = c.Get(ctx, types.NamespacedName{Name: "data-testnet-0", Namespace: "default"}, remaining)
	g.Expect(err).To(HaveOccurred()) // not found

	// Finalizer should be removed
	fetched := &seiv1alpha1.SeiNodePool{}
	_ = c.Get(ctx, types.NamespacedName{Name: "testnet", Namespace: "default"}, fetched)
	g.Expect(fetched.Finalizers).NotTo(ContainElement(finalizerName))
}

func TestHandleDeletion_WithRetain_KeepsPVCs(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	sn := newSeiNodePool("testnet", "default", 1)
	sn.Spec.Storage.RetainOnDelete = true
	sn.Finalizers = []string{finalizerName}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "data-testnet-0",
			Namespace: "default",
			Labels:    resourceLabels(sn),
		},
	}

	r, c := newTestReconciler(t, sn, pvc)
	g.Expect(c.Delete(ctx, sn)).To(Succeed())
	_ = c.Get(ctx, types.NamespacedName{Name: "testnet", Namespace: "default"}, sn)

	_, err := r.Reconcile(ctx, reqFor("testnet", "default"))
	g.Expect(err).NotTo(HaveOccurred())

	// PVC should still exist
	remaining := &corev1.PersistentVolumeClaim{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: "data-testnet-0", Namespace: "default"}, remaining)).To(Succeed())
}

func TestHandleDeletion_WaitsForSeiNodesToDisappear(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	sn := newSeiNodePool("testnet", "default", 1)
	sn.Finalizers = []string{finalizerName}

	// A SeiNode still present with the pool label
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "testnet-0",
			Namespace: "default",
			Labels:    resourceLabels(sn),
		},
	}

	r, c := newTestReconciler(t, sn, node)
	g.Expect(c.Delete(ctx, sn)).To(Succeed())
	_ = c.Get(ctx, types.NamespacedName{Name: "testnet", Namespace: "default"}, sn)

	res, err := r.Reconcile(ctx, reqFor("testnet", "default"))
	g.Expect(err).NotTo(HaveOccurred())
	// Still requeuing because SeiNode is present
	g.Expect(res.RequeueAfter).NotTo(BeZero())

	// Finalizer still present
	fetched := getSeiNodePool(t, ctx, c, "testnet", "default")
	g.Expect(fetched.Finalizers).To(ContainElement(finalizerName))
}
