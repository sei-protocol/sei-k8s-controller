package task

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

func testGroup() *seiv1alpha1.SeiNodeGroup {
	return &seiv1alpha1.SeiNodeGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "fork-group", Namespace: "default", UID: "uid-group"},
		Spec: seiv1alpha1.SeiNodeGroupSpec{
			Replicas: 2,
			Template: seiv1alpha1.SeiNodeTemplate{
				Spec: seiv1alpha1.SeiNodeSpec{
					Image: "sei:v6.0.0",
					Sidecar: &seiv1alpha1.SidecarConfig{
						Image: "seictl:latest",
						Port:  7777,
					},
				},
			},
			Genesis: &seiv1alpha1.GenesisCeremonyConfig{
				ChainID: "fork-1",
				Fork: &seiv1alpha1.ForkConfig{
					SourceChainID: "pacific-1",
					SourceImage:   "sei:v5.0.0",
					ExportHeight:  100000,
				},
			},
		},
	}
}

func testGroupCfg(t *testing.T, group *seiv1alpha1.SeiNodeGroup, objs ...metav1.Object) ExecutionConfig {
	t.Helper()
	s := testScheme(t)
	clientObjs := make([]interface{ GetName() string }, 0, len(objs)+1)
	_ = clientObjs // satisfy unused

	builder := fake.NewClientBuilder().WithScheme(s).WithObjects(group)
	for _, obj := range objs {
		if sn, ok := obj.(*seiv1alpha1.SeiNode); ok {
			builder = builder.WithObjects(sn)
		}
	}
	c := builder.Build()
	return ExecutionConfig{
		KubeClient: c,
		Scheme:     s,
		Resource:   group,
		Platform:   platformtest.Config(),
	}
}

// --- create-exporter ---

func TestCreateExporter_Execute_CreatesSeiNode(t *testing.T) {
	group := testGroup()
	cfg := testGroupCfg(t, group)

	params := CreateExporterParams{
		ExporterName:  "fork-group-exporter",
		Namespace:     "default",
		SourceChainID: "pacific-1",
		SourceImage:   "sei:v5.0.0",
		ExportHeight:  100000,
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeCreateExporter("id-1", raw, cfg)
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

	node := &seiv1alpha1.SeiNode{}
	if err := cfg.KubeClient.Get(ctx, types.NamespacedName{Name: "fork-group-exporter", Namespace: "default"}, node); err != nil {
		t.Fatalf("exporter node not found: %v", err)
	}
	if node.Spec.ChainID != "pacific-1" {
		t.Errorf("ChainID = %q, want %q", node.Spec.ChainID, "pacific-1")
	}
	if node.Spec.Image != "sei:v5.0.0" {
		t.Errorf("Image = %q, want %q", node.Spec.Image, "sei:v5.0.0")
	}
	if node.Labels["sei.io/role"] != "exporter" {
		t.Errorf("role label = %q, want %q", node.Labels["sei.io/role"], "exporter")
	}
	if node.Spec.FullNode == nil || node.Spec.FullNode.Snapshot == nil {
		t.Fatal("expected FullNode with Snapshot config")
	}
	if node.Spec.FullNode.Snapshot.S3 == nil || node.Spec.FullNode.Snapshot.S3.TargetHeight != 100000 {
		t.Errorf("TargetHeight = %d, want %d", node.Spec.FullNode.Snapshot.S3.TargetHeight, 100000)
	}
}

func TestCreateExporter_Execute_Idempotent(t *testing.T) {
	group := testGroup()
	existing := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fork-group-exporter",
			Namespace: "default",
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   "sei:v5.0.0",
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase: seiv1alpha1.PhaseInitializing,
		},
	}
	cfg := testGroupCfg(t, group, existing)

	params := CreateExporterParams{
		ExporterName:  "fork-group-exporter",
		Namespace:     "default",
		SourceChainID: "pacific-1",
		SourceImage:   "sei:v5.0.0",
		ExportHeight:  100000,
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeCreateExporter("id-1", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	ctx := context.Background()
	if err := exec.Execute(ctx); err != nil {
		t.Fatalf("Execute on existing: %v", err)
	}
	if exec.Status(ctx) != ExecutionComplete {
		t.Fatalf("expected Complete for existing exporter, got %s", exec.Status(ctx))
	}
}

func TestCreateExporter_Execute_DeletesFailedExporter(t *testing.T) {
	group := testGroup()
	failed := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fork-group-exporter",
			Namespace: "default",
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   "sei:v5.0.0",
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase: seiv1alpha1.PhaseFailed,
		},
	}
	cfg := testGroupCfg(t, group, failed)

	params := CreateExporterParams{
		ExporterName:  "fork-group-exporter",
		Namespace:     "default",
		SourceChainID: "pacific-1",
		SourceImage:   "sei:v5.0.0",
		ExportHeight:  100000,
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeCreateExporter("id-1", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	ctx := context.Background()
	if err := exec.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Should NOT be complete — the failed exporter was deleted,
	// next reconcile will re-enter and create a fresh one.
	if exec.Status(ctx) != ExecutionRunning {
		t.Fatalf("expected Running (pending recreate), got %s", exec.Status(ctx))
	}

	// Verify the failed exporter was deleted
	node := &seiv1alpha1.SeiNode{}
	err = cfg.KubeClient.Get(ctx, types.NamespacedName{Name: "fork-group-exporter", Namespace: "default"}, node)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound after deleting failed exporter, got %v", err)
	}
}

// --- await-exporter-running ---

func TestAwaitExporterRunning_Running(t *testing.T) {
	group := testGroup()
	exporter := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "fork-group-exporter", Namespace: "default"},
		Status:     seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseInitializing},
	}
	cfg := testGroupCfg(t, group, exporter)

	params := AwaitExporterRunningParams{ExporterName: "fork-group-exporter", Namespace: "default"}
	raw, _ := json.Marshal(params)
	exec, err := deserializeAwaitExporterRunning("id-2", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionRunning {
		t.Fatal("expected Running for Initializing exporter")
	}
}

func TestAwaitExporterRunning_Complete(t *testing.T) {
	group := testGroup()
	exporter := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "fork-group-exporter", Namespace: "default"},
		Status:     seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning},
	}
	cfg := testGroupCfg(t, group, exporter)

	params := AwaitExporterRunningParams{ExporterName: "fork-group-exporter", Namespace: "default"}
	raw, _ := json.Marshal(params)
	exec, err := deserializeAwaitExporterRunning("id-2", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionComplete {
		t.Fatal("expected Complete for Running exporter")
	}
}

func TestAwaitExporterRunning_Failed(t *testing.T) {
	group := testGroup()
	exporter := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "fork-group-exporter", Namespace: "default"},
		Status:     seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseFailed},
	}
	cfg := testGroupCfg(t, group, exporter)

	params := AwaitExporterRunningParams{ExporterName: "fork-group-exporter", Namespace: "default"}
	raw, _ := json.Marshal(params)
	exec, err := deserializeAwaitExporterRunning("id-2", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionFailed {
		t.Fatal("expected Failed for Failed exporter")
	}
	if exec.Err() == nil {
		t.Fatal("expected non-nil error")
	}
}

func TestAwaitExporterRunning_NotFound_KeepsPolling(t *testing.T) {
	group := testGroup()
	cfg := testGroupCfg(t, group)

	params := AwaitExporterRunningParams{ExporterName: "fork-group-exporter", Namespace: "default"}
	raw, _ := json.Marshal(params)
	exec, err := deserializeAwaitExporterRunning("id-2", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionRunning {
		t.Fatal("expected Running when exporter not found (cache lag)")
	}
}

// --- teardown-exporter ---

func TestTeardownExporter_Execute_DeletesNode(t *testing.T) {
	group := testGroup()
	exporter := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "fork-group-exporter", Namespace: "default"},
	}
	cfg := testGroupCfg(t, group, exporter)

	params := TeardownExporterParams{ExporterName: "fork-group-exporter", Namespace: "default"}
	raw, _ := json.Marshal(params)
	exec, err := deserializeTeardownExporter("id-4", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	ctx := context.Background()
	if err := exec.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	node := &seiv1alpha1.SeiNode{}
	err = cfg.KubeClient.Get(ctx, types.NamespacedName{Name: "fork-group-exporter", Namespace: "default"}, node)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected exporter to be deleted, got %v", err)
	}
}

func TestTeardownExporter_Execute_Idempotent(t *testing.T) {
	group := testGroup()
	cfg := testGroupCfg(t, group)

	params := TeardownExporterParams{ExporterName: "fork-group-exporter", Namespace: "default"}
	raw, _ := json.Marshal(params)
	exec, err := deserializeTeardownExporter("id-4", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if err := exec.Execute(context.Background()); err != nil {
		t.Fatalf("expected no error for nonexistent exporter, got %v", err)
	}
}

func TestTeardownExporter_Status_CompleteWhenGone(t *testing.T) {
	group := testGroup()
	cfg := testGroupCfg(t, group)

	params := TeardownExporterParams{ExporterName: "fork-group-exporter", Namespace: "default"}
	raw, _ := json.Marshal(params)
	exec, err := deserializeTeardownExporter("id-4", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionComplete {
		t.Fatal("expected Complete when exporter already gone")
	}
}

func TestTeardownExporter_Status_RunningWhilePresent(t *testing.T) {
	group := testGroup()
	exporter := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "fork-group-exporter", Namespace: "default"},
	}
	cfg := testGroupCfg(t, group, exporter)

	params := TeardownExporterParams{ExporterName: "fork-group-exporter", Namespace: "default"}
	raw, _ := json.Marshal(params)
	exec, err := deserializeTeardownExporter("id-4", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	if exec.Status(context.Background()) != ExecutionRunning {
		t.Fatal("expected Running while exporter still exists")
	}
}

// --- submit-export-state ---

func TestSubmitExportState_SidecarTaskIDDeterministic(t *testing.T) {
	group := testGroup()
	cfg := testGroupCfg(t, group)

	params := SubmitExportStateParams{
		ExporterName:  "fork-group-exporter",
		Namespace:     "default",
		ExportHeight:  100000,
		SourceChainID: "pacific-1",
	}
	raw, _ := json.Marshal(params)

	exec1, err := deserializeSubmitExportState("id-3", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize 1: %v", err)
	}
	exec2, err := deserializeSubmitExportState("id-3", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize 2: %v", err)
	}

	se1 := exec1.(*submitExportStateExecution)
	se2 := exec2.(*submitExportStateExecution)

	if se1.sidecarTaskID() != se2.sidecarTaskID() {
		t.Errorf("sidecar task IDs not deterministic: %s vs %s",
			se1.sidecarTaskID(), se2.sidecarTaskID())
	}
}

func TestSubmitExportState_Timeout(t *testing.T) {
	group := testGroup()
	// Create an exporter that was created long ago
	exporter := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "fork-group-exporter",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-7 * time.Hour)),
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning},
	}
	cfg := testGroupCfg(t, group, exporter)

	params := SubmitExportStateParams{
		ExporterName:  "fork-group-exporter",
		Namespace:     "default",
		ExportHeight:  100000,
		SourceChainID: "pacific-1",
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeSubmitExportState("id-3", raw, cfg)
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}

	status := exec.Status(context.Background())
	if status != ExecutionFailed {
		t.Fatalf("expected Failed after timeout, got %s", status)
	}
	if exec.Err() == nil {
		t.Fatal("expected non-nil timeout error")
	}
}
