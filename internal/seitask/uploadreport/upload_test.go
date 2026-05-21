package uploadreport

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
}

func newFakeS3() *fakeS3 { return &fakeS3{objects: map[string][]byte{}} }

func (s *fakeS3) Put(_ context.Context, bucket, key string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[bucket+"/"+key] = append([]byte(nil), body...)
	return nil
}

// fakePodLister returns canned pod names + log bodies.
type fakePodLister struct {
	names map[string][]string         // workflow-name → pod names
	logs  map[string]string           // pod-name → log content
}

func (l *fakePodLister) ListWorkflowPods(_ context.Context, _ string, workflowName string) ([]string, error) {
	return l.names[workflowName], nil
}

func (l *fakePodLister) StreamPodLog(_ context.Context, _ string, podName string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(l.logs[podName])), nil
}

func TestRun_UploadsWorkflowVarsAndPodLogs(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskruntime.WorkflowVarsName(testWorkflowName),
			Namespace: testNamespace,
		},
		Data: map[string]string{
			string(taskruntime.KeyRunID):        testWorkflowName,
			string(taskruntime.KeyExitReason):   string(taskruntime.ExitReasonPass),
			string(taskruntime.KeyAdminAddress): "sei1abc",
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(cm).Build()
	s3 := newFakeS3()
	pods := &fakePodLister{
		names: map[string][]string{testWorkflowName: {"keygen-pod", "provision-snd-pod"}},
		logs:  map[string]string{"keygen-pod": "keygen output\n", "provision-snd-pod": "provision output\n"},
	}

	res, err := Run(context.Background(), c, Params{
		Bucket:   testBucket,
		Prefix:   testPrefix,
		Workflow: testWorkflow(),
		Pods:     pods,
		S3:       s3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitReason != taskruntime.ExitReasonPass {
		t.Fatalf("ExitReason = %q", res.ExitReason)
	}

	wantKeys := []string{
		testBucket + "/" + testPrefix + "/workflow-vars.yaml",
		testBucket + "/" + testPrefix + "/pods/keygen-pod.log",
		testBucket + "/" + testPrefix + "/pods/provision-snd-pod.log",
	}
	for _, k := range wantKeys {
		if _, ok := s3.objects[k]; !ok {
			t.Fatalf("expected S3 key %q, got %v", k, s3.objects)
		}
	}
	if got := string(s3.objects[testBucket+"/"+testPrefix+"/pods/keygen-pod.log"]); got != "keygen output\n" {
		t.Fatalf("keygen log content: %q", got)
	}
}

func TestRun_PropagatesInfraFailureExitReason(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskruntime.WorkflowVarsName(testWorkflowName),
			Namespace: testNamespace,
		},
		Data: map[string]string{
			string(taskruntime.KeyExitReason): string(taskruntime.ExitReasonInfraFail),
		},
	}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(cm).Build()

	res, err := Run(context.Background(), c, Params{
		Bucket:   testBucket,
		Prefix:   testPrefix,
		Workflow: testWorkflow(),
		Pods:     &fakePodLister{},
		S3:       newFakeS3(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitReason != taskruntime.ExitReasonInfraFail {
		t.Fatalf("ExitReason = %q, want infra-fail", res.ExitReason)
	}
}

func TestRun_NoCMTreatsAsPass(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()

	res, err := Run(context.Background(), c, Params{
		Bucket:   testBucket,
		Prefix:   testPrefix,
		Workflow: testWorkflow(),
		Pods:     &fakePodLister{},
		S3:       newFakeS3(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitReason != taskruntime.ExitReasonPass {
		t.Fatalf("ExitReason = %q, want pass", res.ExitReason)
	}
}

func TestRun_NormalizesTrailingSlashInPrefix(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	s3 := newFakeS3()
	pods := &fakePodLister{names: map[string][]string{testWorkflowName: {"p1"}}, logs: map[string]string{"p1": "x"}}

	_, err := Run(context.Background(), c, Params{
		Bucket:   testBucket,
		Prefix:   testPrefix + "/", // trailing slash
		Workflow: testWorkflow(),
		Pods:     pods,
		S3:       s3,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := testBucket + "/" + testPrefix + "/pods/p1.log"
	if _, ok := s3.objects[want]; !ok {
		t.Fatalf("expected normalized key %q, got %v", want, s3.objects)
	}
}

func TestValidate(t *testing.T) {
	full := Params{
		Bucket:   testBucket,
		Prefix:   testPrefix,
		Workflow: testWorkflow(),
		Pods:     &fakePodLister{},
		S3:       newFakeS3(),
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

// fakeBytesReader satisfies io.ReadCloser when tests need a non-nopcloser.
type fakeBytesReader struct{ b *bytes.Reader }

func (f fakeBytesReader) Read(p []byte) (int, error) { return f.b.Read(p) }
func (f fakeBytesReader) Close() error               { return nil }
