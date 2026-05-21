package uploadreport

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/sei-protocol/sei-k8s-controller/internal/taskruntime"
)

const (
	testBucket       = "harbor-validation-results"
	testPrefix       = "nightly/release-test/wf-test"
	testNamespace    = "nightly"
	testWorkflowName = "wf-test"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func testWorkflow() taskruntime.WorkflowIdentity {
	return taskruntime.WorkflowIdentity{Name: testWorkflowName, UID: "uid-test", Namespace: testNamespace}
}

// fakeS3 records every PutObject so tests can assert keys + bodies.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	failOn  string // bucket+"/"+key prefix to fail on
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (s *fakeS3) Put(_ context.Context, bucket, key string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	full := bucket + "/" + key
	if s.failOn != "" && strings.HasPrefix(full, s.failOn) {
		return errors.New("s3 simulated failure")
	}
	s.objects[full] = append([]byte(nil), body...)
	return nil
}

// fakePodLister returns canned pods + per-container log bodies.
type fakePodLister struct {
	pods []corev1.Pod
	logs map[string]map[string]string // pod → container → content
	// streamErr[pod][container] returns this error from StreamContainerLog.
	streamErr map[string]map[string]error
}

func (l *fakePodLister) ListWorkflowPods(_ context.Context, _ string, _ string) ([]corev1.Pod, error) {
	return l.pods, nil
}

func (l *fakePodLister) StreamContainerLog(_ context.Context, _ string, podName, container string) (io.ReadCloser, error) {
	if err := l.streamErr[podName][container]; err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(l.logs[podName][container])), nil
}

func pod(name string, containers ...string) corev1.Pod {
	p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c})
	}
	return p
}

func podWithInit(name, initCtr string, containers ...string) corev1.Pod {
	p := pod(name, containers...)
	p.Spec.InitContainers = []corev1.Container{{Name: initCtr}}
	return p
}

func workflowVarsCM(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskruntime.WorkflowVarsName(testWorkflowName),
			Namespace: testNamespace,
		},
		Data: data,
	}
}

// workflowCR returns an unstructured chaos-mesh.org/v1alpha1 Workflow with
// status fields so the upload payload is non-trivial.
func workflowCR() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "chaos-mesh.org", Version: "v1alpha1", Kind: "Workflow"})
	u.SetName(testWorkflowName)
	u.SetNamespace(testNamespace)
	u.Object["status"] = map[string]any{"phase": "Succeed"}
	return u
}

func workflowNode(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: "chaos-mesh.org", Version: "v1alpha1", Kind: "WorkflowNode"})
	u.SetName(name)
	u.SetNamespace(testNamespace)
	u.SetLabels(map[string]string{"chaos-mesh.org/workflow": testWorkflowName})
	return u
}

func TestRun_UploadsAllArtifacts(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(workflowVarsCM(map[string]string{
			string(taskruntime.KeyRunID):      testWorkflowName,
			string(taskruntime.KeyExitReason): string(taskruntime.ExitReasonPass),
		})).
		WithObjects(workflowCR(), workflowNode("step-1"), workflowNode("step-2")).
		Build()
	s3 := newFakeS3()
	pods := &fakePodLister{
		pods: []corev1.Pod{
			pod("keygen-pod", "seitask"),
			podWithInit("provision-pod", "seid-init", "seitask"),
		},
		logs: map[string]map[string]string{
			"keygen-pod":    {"seitask": "keygen output\n"},
			"provision-pod": {"seid-init": "init output\n", "seitask": "provision output\n"},
		},
	}

	res, err := Run(context.Background(), c, Params{
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(), Pods: pods, S3: s3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitReason != taskruntime.ExitReasonPass {
		t.Fatalf("ExitReason = %q", res.ExitReason)
	}
	wantKeys := []string{
		testBucket + "/" + testPrefix + "/workflow-vars.yaml",
		testBucket + "/" + testPrefix + "/workflow.yaml",
		testBucket + "/" + testPrefix + "/workflownodes.yaml",
		testBucket + "/" + testPrefix + "/pods/keygen-pod/seitask.log",
		testBucket + "/" + testPrefix + "/pods/provision-pod/seid-init.log",
		testBucket + "/" + testPrefix + "/pods/provision-pod/seitask.log",
	}
	for _, k := range wantKeys {
		if _, ok := s3.objects[k]; !ok {
			t.Fatalf("expected S3 key %q, got %v", k, keysOf(s3))
		}
	}
	if got := string(s3.objects[testBucket+"/"+testPrefix+"/pods/provision-pod/seid-init.log"]); got != "init output\n" {
		t.Fatalf("init container log: %q", got)
	}
}

func TestRun_PropagatesEXITReasonInfraFail(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(
		workflowVarsCM(map[string]string{string(taskruntime.KeyExitReason): string(taskruntime.ExitReasonInfraFail)}),
	).Build()

	res, err := Run(context.Background(), c, Params{
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(),
		Pods: &fakePodLister{}, S3: newFakeS3(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitReason != taskruntime.ExitReasonInfraFail {
		t.Fatalf("ExitReason = %q", res.ExitReason)
	}
}

func TestRun_PropagatesEXITReasonTaskFail(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(
		workflowVarsCM(map[string]string{string(taskruntime.KeyExitReason): string(taskruntime.ExitReasonTaskFail)}),
	).Build()

	res, err := Run(context.Background(), c, Params{
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(),
		Pods: &fakePodLister{}, S3: newFakeS3(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitReason != taskruntime.ExitReasonTaskFail {
		t.Fatalf("ExitReason = %q", res.ExitReason)
	}
}

func TestRun_NoWorkflowVarsCMTreatsAsPass(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	res, err := Run(context.Background(), c, Params{
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(),
		Pods: &fakePodLister{}, S3: newFakeS3(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitReason != taskruntime.ExitReasonPass {
		t.Fatalf("ExitReason = %q", res.ExitReason)
	}
}

func TestRun_NormalizesLeadingAndTrailingSlashInPrefix(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	s3 := newFakeS3()
	pods := &fakePodLister{
		pods: []corev1.Pod{pod("p1", "c1")},
		logs: map[string]map[string]string{"p1": {"c1": "x"}},
	}

	_, err := Run(context.Background(), c, Params{
		Bucket: testBucket, Prefix: "/" + testPrefix + "/", Workflow: testWorkflow(), Pods: pods, S3: s3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := testBucket + "/" + testPrefix + "/pods/p1/c1.log"
	if _, ok := s3.objects[want]; !ok {
		t.Fatalf("expected normalized key %q, got %v", want, keysOf(s3))
	}
}

// Pod GC race: List returns a pod, but Stream errors NotFound by the time
// we get there. Should be recorded in SkippedPods and not fail Run.
func TestRun_PodGCRaceIsRecordedAndBenign(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	gcErr := apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "stale-pod")
	pods := &fakePodLister{
		pods: []corev1.Pod{pod("stale-pod", "c1"), pod("live-pod", "c1")},
		logs: map[string]map[string]string{"live-pod": {"c1": "live\n"}},
		streamErr: map[string]map[string]error{
			"stale-pod": {"c1": gcErr},
		},
	}
	s3 := newFakeS3()

	res, err := Run(context.Background(), c, Params{
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(), Pods: pods, S3: s3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.SkippedPods) != 1 || res.SkippedPods[0].Pod != "stale-pod" || res.SkippedPods[0].Reason != "pod-gone" {
		t.Fatalf("SkippedPods = %+v", res.SkippedPods)
	}
	// live-pod's log still uploaded.
	if _, ok := s3.objects[testBucket+"/"+testPrefix+"/pods/live-pod/c1.log"]; !ok {
		t.Fatalf("live-pod log missing: %v", keysOf(s3))
	}
}

// RBAC drop / unexpected stream error → infra-fail, propagated as Run error.
func TestRun_RBACDropOnPodLogIsInfraFail(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	rbacErr := apierrors.NewForbidden(schema.GroupResource{Resource: "pods/log"}, "p1", errors.New("no perm"))
	pods := &fakePodLister{
		pods:      []corev1.Pod{pod("p1", "c1")},
		streamErr: map[string]map[string]error{"p1": {"c1": rbacErr}},
	}

	_, err := Run(context.Background(), c, Params{
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(), Pods: pods, S3: newFakeS3(),
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var infra *taskruntime.InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("expected InfraError, got %T: %v", err, err)
	}
}

// Context cancellation propagates without leaving partial state assumptions.
func TestRun_ContextCancellation(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	pods := &fakePodLister{
		pods: []corev1.Pod{pod("p1", "c1")},
		logs: map[string]map[string]string{"p1": {"c1": "x"}},
	}
	// Just smoke that Run doesn't panic and returns. With pre-cancelled ctx,
	// the controller-runtime Get on workflow-vars NotFound; the workflow CR
	// Get also NotFound (no workflow in fake client). pods/logs upload may
	// proceed since the fake doesn't respect ctx — accept either outcome.
	_, _ = Run(ctx, c, Params{
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(), Pods: pods, S3: newFakeS3(),
	})
}

func TestValidate(t *testing.T) {
	full := Params{
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(),
		Pods: &fakePodLister{}, S3: newFakeS3(),
	}
	cases := []struct {
		name string
		mut  func(*Params)
		want bool
	}{
		{"complete", func(*Params) {}, false},
		{"missing bucket", func(p *Params) { p.Bucket = "" }, true},
		{"missing prefix", func(p *Params) { p.Prefix = "" }, true},
		{"missing workflow", func(p *Params) { p.Workflow.Name = "" }, true},
		{"missing pods seam", func(p *Params) { p.Pods = nil }, true},
		{"missing s3 seam", func(p *Params) { p.S3 = nil }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := full
			tc.mut(&p)
			err := validate(p)
			if (err != nil) != tc.want {
				t.Fatalf("validate err=%v wantErr=%v", err, tc.want)
			}
		})
	}
}

func keysOf(s *fakeS3) []string {
	out := make([]string, 0, len(s.objects))
	for k := range s.objects {
		out = append(out, k)
	}
	return out
}
