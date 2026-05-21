package uploadreport

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

func workflowVarsCM(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskruntime.WorkflowVarsName(testWorkflowName),
			Namespace: testNamespace,
		},
		Data: data,
	}
}

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

	res, err := Run(context.Background(), c, Params{
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(), S3: s3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitReason != taskruntime.ExitReasonPass {
		t.Fatalf("ExitReason = %q", res.ExitReason)
	}
	for _, k := range []string{
		testBucket + "/" + testPrefix + "/workflow-vars.yaml",
		testBucket + "/" + testPrefix + "/workflow.yaml",
		testBucket + "/" + testPrefix + "/workflownodes.yaml",
	} {
		if _, ok := s3.objects[k]; !ok {
			t.Fatalf("expected S3 key %q, got %v", k, keysOf(s3))
		}
	}
}

func TestRun_PropagatesEXITReasonInfraFail(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(
		workflowVarsCM(map[string]string{string(taskruntime.KeyExitReason): string(taskruntime.ExitReasonInfraFail)}),
	).Build()

	res, err := Run(context.Background(), c, Params{
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(), S3: newFakeS3(),
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
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(), S3: newFakeS3(),
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
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(), S3: newFakeS3(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitReason != taskruntime.ExitReasonPass {
		t.Fatalf("ExitReason = %q", res.ExitReason)
	}
}

func TestRun_NormalizesLeadingAndTrailingSlashInPrefix(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(workflowCR()).Build()
	s3 := newFakeS3()

	_, err := Run(context.Background(), c, Params{
		Bucket: testBucket, Prefix: "/" + testPrefix + "/", Workflow: testWorkflow(), S3: s3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := testBucket + "/" + testPrefix + "/workflow.yaml"
	if _, ok := s3.objects[want]; !ok {
		t.Fatalf("expected normalized key %q, got %v", want, keysOf(s3))
	}
}

func TestRun_S3UploadFailureIsInfraError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(workflowCR()).Build()
	s3 := newFakeS3()
	s3.failOn = testBucket + "/" + testPrefix

	_, err := Run(context.Background(), c, Params{
		Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(), S3: s3,
	})
	if err == nil {
		t.Fatalf("expected error")
	}
	var infra *taskruntime.InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("expected InfraError, got %T: %v", err, err)
	}
}

func TestValidate(t *testing.T) {
	full := Params{Bucket: testBucket, Prefix: testPrefix, Workflow: testWorkflow(), S3: newFakeS3()}
	cases := []struct {
		name string
		mut  func(*Params)
		want bool
	}{
		{"complete", func(*Params) {}, false},
		{"missing bucket", func(p *Params) { p.Bucket = "" }, true},
		{"missing prefix", func(p *Params) { p.Prefix = "" }, true},
		{"missing workflow", func(p *Params) { p.Workflow.Name = "" }, true},
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
