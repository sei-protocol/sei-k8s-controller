package node

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

type mockSidecarClient struct {
	submitted []sidecar.TaskBuilder
	submitErr error
	submitID  uuid.UUID

	taskResults map[uuid.UUID]*sidecar.TaskResult
	getTaskErr  error
}

func (m *mockSidecarClient) SubmitTask(_ context.Context, task sidecar.TaskBuilder) (uuid.UUID, error) {
	m.submitted = append(m.submitted, task)
	if m.submitErr != nil {
		return uuid.Nil, m.submitErr
	}
	id := m.submitID
	if id == uuid.Nil {
		id = uuid.New()
	}
	return id, nil
}

func (m *mockSidecarClient) GetTask(_ context.Context, id uuid.UUID) (*sidecar.TaskResult, error) {
	if m.getTaskErr != nil {
		return nil, m.getTaskErr
	}
	if m.taskResults != nil {
		if r, ok := m.taskResults[id]; ok {
			return r, nil
		}
	}
	return nil, sidecar.ErrNotFound
}

func strPtr(s string) *string { return &s }

func completedResult(id uuid.UUID, taskType string, taskErr *string) *sidecar.TaskResult {
	now := time.Now()
	return &sidecar.TaskResult{
		Id:          id,
		Type:        taskType,
		SubmittedAt: now.Add(-10 * time.Second),
		CompletedAt: &now,
		Error:       taskErr,
	}
}

func runningResult(id uuid.UUID, taskType string) *sidecar.TaskResult {
	return &sidecar.TaskResult{
		Id:          id,
		Type:        taskType,
		SubmittedAt: time.Now(),
	}
}

func newProgressionReconciler(t *testing.T, mock *mockSidecarClient, objs ...client.Object) (*SeiNodeReconciler, client.Client) {
	t.Helper()
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()
	r := &SeiNodeReconciler{
		Client: c,
		Scheme: s,
		BuildSidecarClientFn: func(_ *seiv1alpha1.SeiNode) SidecarStatusClient {
			return mock
		},
	}
	return r, c
}

func fetchNode(t *testing.T, c client.Client, name, namespace string) *seiv1alpha1.SeiNode {
	t.Helper()
	n := &seiv1alpha1.SeiNode{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, n); err != nil {
		t.Fatalf("fetching node: %v", err)
	}
	return n
}

func snapshotNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   "sei:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{ChainID: "atlantic-2"},
			Snapshot: &seiv1alpha1.SnapshotSource{
				Region: "us-east-1",
				Bucket: seiv1alpha1.BucketSnapshot{URI: "s3://my-bucket/snapshots/latest.tar"},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

func peerSyncNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   "sei:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{
				ChainID: "atlantic-2",
				S3:      &seiv1alpha1.GenesisS3Source{URI: "s3://sei-testnet-genesis-config/atlantic-2/genesis.json", Region: "us-east-2"},
			},
			Peers: &seiv1alpha1.PeerConfig{
				Sources: []seiv1alpha1.PeerSource{
					{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"ChainIdentifier": "atlantic-2"}}},
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

func genesisNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "arctic-1",
			Image:   "sei:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{
				ChainID: "arctic-1",
				Fresh:   true,
				PVC:     &seiv1alpha1.GenesisPVCSource{DataPVC: "data-pvc"},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

func snapshotterNode() *seiv1alpha1.SeiNode {
	node := snapshotNode()
	node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		Interval: 2000, KeepRecent: 5,
		Destination: &seiv1alpha1.SnapshotDestination{
			S3: &seiv1alpha1.S3SnapshotDestination{Bucket: "atlantic-2-snapshots", Prefix: "state-sync", Region: "eu-central-1"},
		},
	}
	return node
}

func TestBootstrapMode(t *testing.T) {
	tests := []struct {
		name string
		node *seiv1alpha1.SeiNode
		want string
	}{
		{"snapshot", snapshotNode(), "snapshot"},
		{"peer-sync", peerSyncNode(), "peer-sync"},
		{"genesis", genesisNode(), "genesis"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bootstrapMode(tt.node); got != tt.want {
				t.Errorf("bootstrapMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTaskProgressionForNode_Snapshot(t *testing.T) {
	got := taskProgressionForNode(snapshotNode())
	want := []string{taskSnapshotRestore, taskDiscoverPeers, taskConfigPatch, taskMarkReady}
	assertProgression(t, got, want)
}

func TestTaskProgressionForNode_PeerSync(t *testing.T) {
	got := taskProgressionForNode(peerSyncNode())
	want := []string{taskDiscoverPeers, taskConfigureGenesis, taskConfigureStateSync, taskConfigPatch, taskMarkReady}
	assertProgression(t, got, want)
}

func TestTaskProgressionForNode_Genesis(t *testing.T) {
	got := taskProgressionForNode(genesisNode())
	want := []string{taskConfigPatch, taskMarkReady}
	assertProgression(t, got, want)
}

func TestTaskProgressionForNode_GenesisWithPeers(t *testing.T) {
	node := genesisNode()
	node.Spec.Peers = &seiv1alpha1.PeerConfig{
		Sources: []seiv1alpha1.PeerSource{
			{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"Chain": "arctic-1"}}},
		},
	}
	got := taskProgressionForNode(node)
	want := []string{taskDiscoverPeers, taskConfigPatch, taskMarkReady}
	assertProgression(t, got, want)
}

func assertProgression(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("progression = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("progression[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildTaskPlan(t *testing.T) {
	plan := buildTaskPlan(snapshotNode())
	if plan.Phase != seiv1alpha1.TaskPlanActive {
		t.Errorf("phase = %q, want Active", plan.Phase)
	}
	if len(plan.Tasks) != 4 {
		t.Fatalf("expected 4 tasks, got %d", len(plan.Tasks))
	}
	for _, task := range plan.Tasks {
		if task.Status != seiv1alpha1.PlannedTaskPending {
			t.Errorf("task %q status = %q, want Pending", task.Type, task.Status)
		}
	}
	if plan.Tasks[0].Type != taskSnapshotRestore {
		t.Errorf("first task = %q, want %q", plan.Tasks[0].Type, taskSnapshotRestore)
	}
}

func TestReconcile_CreatesPlanOnFirstRun(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	r, c := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.InitPlan == nil {
		t.Fatal("expected InitPlan to be created")
	}
	if updated.Status.InitPlan.Phase != seiv1alpha1.TaskPlanActive {
		t.Errorf("phase = %q, want Active", updated.Status.InitPlan.Phase)
	}
	// First task should have been submitted.
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].TaskType() != taskSnapshotRestore {
		t.Errorf("submitted task = %q, want %q", mock.submitted[0].TaskType(), taskSnapshotRestore)
	}
}

func TestReconcile_SubmitsFirstPendingTask(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{submitID: taskID}
	node := snapshotNode()
	node.Status.InitPlan = buildTaskPlan(node)
	r, c := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted, got %d", len(mock.submitted))
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	task := updated.Status.InitPlan.Tasks[0]
	if task.Status != seiv1alpha1.PlannedTaskSubmitted {
		t.Errorf("task status = %q, want Submitted", task.Status)
	}
	if task.TaskID != taskID.String() {
		t.Errorf("taskID = %q, want %q", task.TaskID, taskID.String())
	}
}

func TestReconcile_PollsSubmittedTask_StillRunning(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{
		taskResults: map[uuid.UUID]*sidecar.TaskResult{
			taskID: runningResult(taskID, taskSnapshotRestore),
		},
	}
	node := snapshotNode()
	node.Status.InitPlan = buildTaskPlan(node)
	node.Status.InitPlan.Tasks[0].Status = seiv1alpha1.PlannedTaskSubmitted
	node.Status.InitPlan.Tasks[0].TaskID = taskID.String()

	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != bootstrapPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, bootstrapPollInterval)
	}
	if len(mock.submitted) != 0 {
		t.Errorf("expected no new submissions, got %d", len(mock.submitted))
	}
}

func TestReconcile_PollsSubmittedTask_Completed_AdvancesToNext(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{
		taskResults: map[uuid.UUID]*sidecar.TaskResult{
			taskID: completedResult(taskID, taskSnapshotRestore, nil),
		},
	}
	node := snapshotNode()
	node.Status.InitPlan = buildTaskPlan(node)
	node.Status.InitPlan.Tasks[0].Status = seiv1alpha1.PlannedTaskSubmitted
	node.Status.InitPlan.Tasks[0].TaskID = taskID.String()

	r, c := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected immediate requeue after task completion")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.InitPlan.Tasks[0].Status != seiv1alpha1.PlannedTaskComplete {
		t.Errorf("task[0] status = %q, want Complete", updated.Status.InitPlan.Tasks[0].Status)
	}
}

func TestReconcile_PollsSubmittedTask_Failed_FailsPlan(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{
		taskResults: map[uuid.UUID]*sidecar.TaskResult{
			taskID: completedResult(taskID, taskSnapshotRestore, strPtr("network timeout")),
		},
	}
	node := snapshotNode()
	node.Status.InitPlan = buildTaskPlan(node)
	node.Status.InitPlan.Tasks[0].Status = seiv1alpha1.PlannedTaskSubmitted
	node.Status.InitPlan.Tasks[0].TaskID = taskID.String()

	r, c := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue on failure, got %v", result)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.InitPlan.Tasks[0].Status != seiv1alpha1.PlannedTaskFailed {
		t.Errorf("task[0] status = %q, want Failed", updated.Status.InitPlan.Tasks[0].Status)
	}
	if updated.Status.InitPlan.Tasks[0].Error != "network timeout" {
		t.Errorf("task[0] error = %q, want %q", updated.Status.InitPlan.Tasks[0].Error, "network timeout")
	}
	if updated.Status.InitPlan.Phase != seiv1alpha1.TaskPlanFailed {
		t.Errorf("plan phase = %q, want Failed", updated.Status.InitPlan.Phase)
	}
}

func TestReconcile_AllTasksComplete_MarksPlanComplete(t *testing.T) {
	mock := &mockSidecarClient{}
	node := genesisNode()
	node.Status.InitPlan = buildTaskPlan(node)
	// Mark all tasks complete.
	for i := range node.Status.InitPlan.Tasks {
		node.Status.InitPlan.Tasks[i].Status = seiv1alpha1.PlannedTaskComplete
	}

	r, c := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != statusPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, statusPollInterval)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.InitPlan.Phase != seiv1alpha1.TaskPlanComplete {
		t.Errorf("plan phase = %q, want Complete", updated.Status.InitPlan.Phase)
	}
}

func TestReconcile_FailedPlan_NoOps(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	node.Status.InitPlan = buildTaskPlan(node)
	node.Status.InitPlan.Phase = seiv1alpha1.TaskPlanFailed

	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for failed plan, got %v", result)
	}
	if len(mock.submitted) != 0 {
		t.Errorf("expected no submissions for failed plan, got %d", len(mock.submitted))
	}
}

func TestReconcile_CompletePlan_RunsRuntimeTasks(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotterNode()
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}

	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != statusPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, statusPollInterval)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 runtime task submitted, got %d", len(mock.submitted))
	}
	if mock.submitted[0].TaskType() != taskSnapshotUpload {
		t.Errorf("task type = %q, want %q", mock.submitted[0].TaskType(), taskSnapshotUpload)
	}
}

func TestReconcile_SubmitError_RequeuesGracefully(t *testing.T) {
	mock := &mockSidecarClient{submitErr: fmt.Errorf("connection refused")}
	node := snapshotNode()
	node.Status.InitPlan = buildTaskPlan(node)

	r, c := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != bootstrapPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, bootstrapPollInterval)
	}
	// Task should still be Pending (not Submitted) since submit failed.
	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.InitPlan.Tasks[0].Status != seiv1alpha1.PlannedTaskPending {
		t.Errorf("task status = %q, want Pending after submit failure", updated.Status.InitPlan.Tasks[0].Status)
	}
}

func TestReconcile_GetTaskError_RequeuesGracefully(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{getTaskErr: fmt.Errorf("connection refused")}
	node := snapshotNode()
	node.Status.InitPlan = buildTaskPlan(node)
	node.Status.InitPlan.Tasks[0].Status = seiv1alpha1.PlannedTaskSubmitted
	node.Status.InitPlan.Tasks[0].TaskID = taskID.String()

	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != bootstrapPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, bootstrapPollInterval)
	}
}

// --- TaskBuilder tests ---

func TestTaskBuilderForNode_SnapshotRestore(t *testing.T) {
	b := taskBuilderForNode(snapshotNode(), taskSnapshotRestore)
	task, ok := b.(sidecar.SnapshotRestoreTask)
	if !ok {
		t.Fatalf("expected SnapshotRestoreTask, got %T", b)
	}
	if task.Bucket != "my-bucket" {
		t.Errorf("Bucket = %q, want %q", task.Bucket, "my-bucket")
	}
	if task.Region != "us-east-1" {
		t.Errorf("Region = %q, want %q", task.Region, "us-east-1")
	}
}

func TestTaskBuilderForNode_DiscoverPeers_EC2Tags(t *testing.T) {
	node := snapshotNode()
	node.Spec.Peers = &seiv1alpha1.PeerConfig{
		Sources: []seiv1alpha1.PeerSource{
			{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"ChainIdentifier": "atlantic-2"}}},
		},
	}
	b := taskBuilderForNode(node, taskDiscoverPeers)
	task, ok := b.(sidecar.DiscoverPeersTask)
	if !ok {
		t.Fatalf("expected DiscoverPeersTask, got %T", b)
	}
	if len(task.Sources) != 1 {
		t.Fatalf("expected 1 source, got %d", len(task.Sources))
	}
	if task.Sources[0].Type != sidecar.PeerSourceEC2Tags {
		t.Errorf("type = %v, want ec2Tags", task.Sources[0].Type)
	}
}

func TestTaskBuilderForNode_ConfigureGenesis_WithS3(t *testing.T) {
	b := configureGenesisBuilder(peerSyncNode())
	task, ok := b.(sidecar.ConfigureGenesisTask)
	if !ok {
		t.Fatalf("expected ConfigureGenesisTask, got %T", b)
	}
	if task.URI != "s3://sei-testnet-genesis-config/atlantic-2/genesis.json" {
		t.Errorf("URI = %q", task.URI)
	}
}

func TestTaskBuilderForNode_MarkReady(t *testing.T) {
	b := taskBuilderForNode(snapshotNode(), taskMarkReady)
	if _, ok := b.(sidecar.MarkReadyTask); !ok {
		t.Errorf("expected MarkReadyTask, got %T", b)
	}
}

func TestConfigPatchBuilder(t *testing.T) {
	t.Run("snapshot restore adds config.toml statesync patch", func(t *testing.T) {
		task := configPatchBuilder(snapshotNode()).(sidecar.ConfigPatchTask)
		configPatch, ok := task.Files["config.toml"]
		if !ok {
			t.Fatal("expected config.toml patch for snapshot node")
		}
		ss, ok := configPatch["statesync"].(map[string]any)
		if !ok {
			t.Fatal("expected statesync section")
		}
		if ss["use-local-snapshot"] != true {
			t.Errorf("use-local-snapshot = %v, want true", ss["use-local-snapshot"])
		}
		if ss["trust-period"] != snapshotTrustPeriod {
			t.Errorf("trust-period = %v, want %s", ss["trust-period"], snapshotTrustPeriod)
		}
	})

	t.Run("snapshot generation adds app.toml patch", func(t *testing.T) {
		node := snapshotNode()
		node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{Interval: 2000, KeepRecent: 10}
		task := configPatchBuilder(node).(sidecar.ConfigPatchTask)
		appPatch, ok := task.Files["app.toml"]
		if !ok {
			t.Fatal("expected app.toml patch")
		}
		if appPatch["snapshot-interval"] != int64(2000) {
			t.Errorf("snapshot-interval = %v, want 2000", appPatch["snapshot-interval"])
		}
		if appPatch["pruning"] != "nothing" {
			t.Errorf("pruning = %v, want nothing", appPatch["pruning"])
		}
		if appPatch["snapshot-keep-recent"] != int64(10) {
			t.Errorf("snapshot-keep-recent = %v, want 10", appPatch["snapshot-keep-recent"])
		}
	})

	t.Run("snapshot generation defaults KeepRecent to 5", func(t *testing.T) {
		node := snapshotNode()
		node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{Interval: 1000}
		task := configPatchBuilder(node).(sidecar.ConfigPatchTask)
		if task.Files["app.toml"]["snapshot-keep-recent"] != int64(5) {
			t.Errorf("snapshot-keep-recent = %v, want 5", task.Files["app.toml"]["snapshot-keep-recent"])
		}
	})

	t.Run("snapshot + snapshot generation produces both files", func(t *testing.T) {
		node := snapshotterNode()
		task := configPatchBuilder(node).(sidecar.ConfigPatchTask)
		if _, ok := task.Files["config.toml"]; !ok {
			t.Error("expected config.toml patch")
		}
		if _, ok := task.Files["app.toml"]; !ok {
			t.Error("expected app.toml patch")
		}
	})

	t.Run("no snapshot produces no config.toml", func(t *testing.T) {
		task := configPatchBuilder(peerSyncNode()).(sidecar.ConfigPatchTask)
		if _, ok := task.Files["config.toml"]; ok {
			t.Error("expected no config.toml patch for non-snapshot node")
		}
	})

	t.Run("genesis node produces empty Files", func(t *testing.T) {
		task := configPatchBuilder(genesisNode()).(sidecar.ConfigPatchTask)
		if len(task.Files) != 0 {
			t.Errorf("expected empty Files for genesis node, got %d entries", len(task.Files))
		}
	})
}

func TestSnapshotUploadTask_WithDestination(t *testing.T) {
	b := snapshotUploadTask(snapshotterNode())
	if b == nil {
		t.Fatal("expected non-nil task")
	}
	task := b.(sidecar.SnapshotUploadTask)
	if task.Bucket != "atlantic-2-snapshots" {
		t.Errorf("Bucket = %q, want %q", task.Bucket, "atlantic-2-snapshots")
	}
}

func TestSnapshotUploadTask_NoDestination(t *testing.T) {
	if task := snapshotUploadTask(snapshotNode()); task != nil {
		t.Errorf("expected nil, got %v", task)
	}
}

func TestNeedsStateSync(t *testing.T) {
	if !needsStateSync(peerSyncNode()) {
		t.Error("peerSyncNode: want true")
	}
	if needsStateSync(genesisNode()) {
		t.Error("genesisNode: want false (fresh)")
	}
	if needsStateSync(snapshotNode()) {
		t.Error("snapshotNode: want false (has snapshot)")
	}
}
