package nodetask

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	testNS       = "default"
	testTaskName = "upgrade-v2"
	testNodeName = "validator-0"
	testImageV1  = "ghcr.io/sei-protocol/seid:v1.0.0"
	testImageV2  = "ghcr.io/sei-protocol/seid:v2.0.0"
)

func newScheme(t *testing.T) *k8sruntime.Scheme {
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

func newReconciler(t *testing.T, now time.Time, objs ...client.Object) (*SeiNodeTaskReconciler, client.Client) {
	t.Helper()
	s := newScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNodeTask{}, &seiv1alpha1.SeiNode{}).
		Build()
	r := &SeiNodeTaskReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(100),
		Platform: platformtest.Config(),
		Now:      func() time.Time { return now },
		ConfigFor: func(_ context.Context, _ *seiv1alpha1.SeiNodeTask, _ *seiv1alpha1.SeiNode) task.ExecutionConfig {
			return task.ExecutionConfig{
				KubeClient: c,
				APIReader:  c,
				Scheme:     s,
			}
		},
	}
	return r, c
}

func newUpdateImageTask() *seiv1alpha1.SeiNodeTask {
	return &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-1", Generation: 1},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindUpdateNodeImage,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef:      seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
				RequirePhase: seiv1alpha1.PhaseRunning,
			},
			UpdateNodeImage: &seiv1alpha1.UpdateNodeImagePayload{Image: testImageV2},
		},
	}
}

func newRunningNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName, Namespace: testNS, UID: "node-uid-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "sei-test", Image: testImageV1,
			Validator: &seiv1alpha1.ValidatorSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase:        seiv1alpha1.PhaseRunning,
			CurrentImage: testImageV1,
		},
	}
}

func req() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Name: testTaskName, Namespace: testNS}}
}

func getTask(t *testing.T, ctx context.Context, c client.Client) *seiv1alpha1.SeiNodeTask {
	t.Helper()
	out := &seiv1alpha1.SeiNodeTask{}
	if err := c.Get(ctx, types.NamespacedName{Name: testTaskName, Namespace: testNS}, out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReconcile_NotFound(t *testing.T) {
	g := NewWithT(t)
	r, _ := newReconciler(t, time.Now())
	res, err := r.Reconcile(context.Background(), req())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
}

func TestReconcile_TargetMissing_FailsTerminal(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cr := newUpdateImageTask()
	r, c := newReconciler(t, time.Now(), cr)

	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())

	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
}

func TestReconcile_TargetNotRunning_WaitsThenTimesOut(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newUpdateImageTask()
	short := metav1.Duration{Duration: 100 * time.Millisecond}
	cr.Spec.Target.RequirePhaseTimeout = &short
	node := newRunningNode()
	node.Status.Phase = seiv1alpha1.PhaseInitializing

	r, c := newReconciler(t, t0, cr, node)

	// First reconcile: stamps TargetFirstObservedAt, requeues.
	res, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(targetWaitInterval))
	got := getTask(t, ctx, c)
	g.Expect(got.Status.TargetFirstObservedAt).NotTo(BeNil())
	g.Expect(got.Status.Phase).NotTo(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))

	// Advance past the timeout.
	r.Now = func() time.Time { return t0.Add(time.Second) }
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
}

func TestReconcile_UpdateNodeImage_HappyPath(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	t0 := time.Now()
	cr := newUpdateImageTask()
	node := newRunningNode()

	r, c := newReconciler(t, t0, cr, node)

	// R1: synthesize task, phase Running, requeue immediate.
	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))
	g.Expect(got.Status.Task).NotTo(BeNil())
	g.Expect(got.Status.Task.ID).To(Equal(task.DeterministicTaskID(string(cr.UID), string(cr.Spec.Kind), 0)))

	// R2: Execute patches target.spec.image. Status polls, still Running.
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	updatedNode := &seiv1alpha1.SeiNode{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: testNodeName, Namespace: testNS}, updatedNode)).To(Succeed())
	g.Expect(updatedNode.Spec.Image).To(Equal(testImageV2))
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))

	// Simulate the SeiNode controller observing rollout completion.
	updatedNode.Status.CurrentImage = testImageV2
	g.Expect(c.Status().Update(ctx, updatedNode)).To(Succeed())

	// R3: Status sees currentImage matches → Complete.
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))
	g.Expect(got.Status.Outputs).NotTo(BeNil())
	g.Expect(got.Status.Outputs.UpdateNodeImage).NotTo(BeNil())
	g.Expect(got.Status.Outputs.UpdateNodeImage.AppliedImage).To(Equal(testImageV2))
}

func TestReconcile_TerminalNoOp(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	cr := newUpdateImageTask()
	cr.Status.Phase = seiv1alpha1.SeiNodeTaskPhaseComplete
	r, c := newReconciler(t, time.Now(), cr)

	res, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))
	g.Expect(getTask(t, ctx, c).Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))
}
