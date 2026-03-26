package task

import (
	"context"
	"encoding/json"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

func testScheme(t *testing.T) *k8sruntime.Scheme {
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

func testNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default", UID: "uid-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   "sei:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{
					S3:          &seiv1alpha1.S3SnapshotSource{TargetHeight: 100},
					TrustPeriod: "168h",
				},
			},
		},
	}
}

func testCfg(t *testing.T, objs ...client.Object) ExecutionConfig {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		Build()
	return ExecutionConfig{
		KubeClient: c,
		Scheme:     s,
		Resource:   testNode(),
		Platform:   platform.DefaultConfig(),
	}
}

// --- deploy-bootstrap-service ---

func TestDeployBootstrapService_Execute_CreatesService(t *testing.T) {
	node := testNode()
	cfg := testCfg(t)
	params := DeployBootstrapServiceParams{ServiceName: node.Name, Namespace: node.Namespace}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapService("id-1", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	ctx := context.Background()
	if err := exec.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exec.Status(ctx) != ExecutionComplete {
		t.Fatalf("expected Complete, got %s", exec.Status(ctx))
	}

	svc := &corev1.Service{}
	if err := cfg.KubeClient.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, svc); err != nil {
		t.Fatalf("service not found: %v", err)
	}
}

func TestDeployBootstrapService_Execute_Idempotent(t *testing.T) {
	node := testNode()
	svc := GenerateBootstrapService(node)
	cfg := testCfg(t, svc)

	params := DeployBootstrapServiceParams{ServiceName: node.Name, Namespace: node.Namespace}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapService("id-1", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	ctx := context.Background()
	if err := exec.Execute(ctx); err != nil {
		t.Fatalf("Execute on existing service: %v", err)
	}
	if exec.Status(ctx) != ExecutionComplete {
		t.Fatalf("expected Complete, got %s", exec.Status(ctx))
	}
}

func TestDeployBootstrapService_Status_DetectsExisting(t *testing.T) {
	node := testNode()
	svc := GenerateBootstrapService(node)
	cfg := testCfg(t, svc)

	params := DeployBootstrapServiceParams{ServiceName: node.Name, Namespace: node.Namespace}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapService("id-1", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionComplete {
		t.Fatalf("expected Status to detect existing service as Complete")
	}
}

// --- deploy-bootstrap-job ---

func TestDeployBootstrapJob_Execute_CreatesJob(t *testing.T) {
	node := testNode()
	cfg := testCfg(t)
	params := DeployBootstrapJobParams{JobName: BootstrapJobName(node), Namespace: node.Namespace}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapJob("id-2", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	ctx := context.Background()
	if err := exec.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if exec.Status(ctx) != ExecutionComplete {
		t.Fatalf("expected Complete, got %s", exec.Status(ctx))
	}

	job := &batchv1.Job{}
	if err := cfg.KubeClient.Get(ctx, types.NamespacedName{Name: BootstrapJobName(node), Namespace: node.Namespace}, job); err != nil {
		t.Fatalf("job not found: %v", err)
	}
}

func TestDeployBootstrapJob_Execute_NilSnapshot(t *testing.T) {
	node := testNode()
	node.Spec.FullNode.Snapshot = nil
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).Build()
	cfg := ExecutionConfig{
		KubeClient: c,
		Scheme:     s,
		Resource:   node,
		Platform:   platform.DefaultConfig(),
	}

	params := DeployBootstrapJobParams{JobName: BootstrapJobName(node), Namespace: node.Namespace}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapJob("id-2", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if err := exec.Execute(context.Background()); err == nil {
		t.Fatal("expected error for nil snapshot, got nil")
	}
}

// --- await-bootstrap-complete ---

func TestAwaitBootstrapComplete_JobRunning(t *testing.T) {
	node := testNode()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: BootstrapJobName(node), Namespace: node.Namespace},
		Status:     batchv1.JobStatus{Active: 1},
	}
	cfg := testCfg(t, job)

	params := AwaitBootstrapCompleteParams{JobName: BootstrapJobName(node), Namespace: node.Namespace}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapAwait("id-3", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	ctx := context.Background()
	if err := exec.Execute(ctx); err != nil {
		t.Fatalf("Execute (no-op): %v", err)
	}
	if exec.Status(ctx) != ExecutionRunning {
		t.Fatalf("expected Running for active job, got %s", exec.Status(ctx))
	}
}

func TestAwaitBootstrapComplete_JobComplete(t *testing.T) {
	node := testNode()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: BootstrapJobName(node), Namespace: node.Namespace},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
		},
	}
	cfg := testCfg(t, job)

	params := AwaitBootstrapCompleteParams{JobName: BootstrapJobName(node), Namespace: node.Namespace}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapAwait("id-3", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionComplete {
		t.Fatal("expected Complete for finished job")
	}
}

func TestAwaitBootstrapComplete_JobFailed(t *testing.T) {
	node := testNode()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: BootstrapJobName(node), Namespace: node.Namespace},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "OOM killed"},
			},
		},
	}
	cfg := testCfg(t, job)

	params := AwaitBootstrapCompleteParams{JobName: BootstrapJobName(node), Namespace: node.Namespace}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapAwait("id-3", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionFailed {
		t.Fatal("expected Failed for failed job")
	}
	if exec.Err() == nil {
		t.Fatal("expected non-nil error")
	}
}

func TestAwaitBootstrapComplete_JobNotFound(t *testing.T) {
	cfg := testCfg(t)

	params := AwaitBootstrapCompleteParams{JobName: "nonexistent-job", Namespace: "default"}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapAwait("id-3", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionFailed {
		t.Fatal("expected Failed for missing job")
	}
}

// --- teardown-bootstrap ---

func TestTeardownBootstrap_Execute_DeletesResources(t *testing.T) {
	node := testNode()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: BootstrapJobName(node), Namespace: node.Namespace},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace},
	}
	cfg := testCfg(t, job, svc)

	params := TeardownBootstrapParams{
		JobName:     BootstrapJobName(node),
		ServiceName: node.Name,
		Namespace:   node.Namespace,
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapTeardown("id-4", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	ctx := context.Background()
	if err := exec.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	gotJob := &batchv1.Job{}
	if err := cfg.KubeClient.Get(ctx, types.NamespacedName{Name: BootstrapJobName(node), Namespace: node.Namespace}, gotJob); !apierrors.IsNotFound(err) {
		t.Fatalf("expected job to be deleted, got err=%v", err)
	}
	gotSvc := &corev1.Service{}
	if err := cfg.KubeClient.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, gotSvc); !apierrors.IsNotFound(err) {
		t.Fatalf("expected service to be deleted, got err=%v", err)
	}
}

func TestTeardownBootstrap_Execute_Idempotent(t *testing.T) {
	cfg := testCfg(t)

	params := TeardownBootstrapParams{
		JobName:     "nonexistent",
		ServiceName: "nonexistent",
		Namespace:   "default",
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapTeardown("id-4", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if err := exec.Execute(context.Background()); err != nil {
		t.Fatalf("expected no error deleting nonexistent resources, got %v", err)
	}
}

func TestTeardownBootstrap_Status_CompleteWhenGone(t *testing.T) {
	cfg := testCfg(t)

	params := TeardownBootstrapParams{
		JobName:     "gone-job",
		ServiceName: "gone-svc",
		Namespace:   "default",
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapTeardown("id-4", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionComplete {
		t.Fatal("expected Complete when resources are already gone")
	}
}

func TestTeardownBootstrap_Status_RunningWhilePresent(t *testing.T) {
	node := testNode()
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: BootstrapJobName(node), Namespace: node.Namespace},
	}
	cfg := testCfg(t, job)

	params := TeardownBootstrapParams{
		JobName:     BootstrapJobName(node),
		ServiceName: node.Name,
		Namespace:   node.Namespace,
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeBootstrapTeardown("id-4", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionRunning {
		t.Fatal("expected Running while job still present")
	}
}
