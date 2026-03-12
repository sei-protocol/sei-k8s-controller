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
			StateSync: &seiv1alpha1.StateSyncConfig{
				Snapshot: &seiv1alpha1.SnapshotSource{
					Region: "us-east-1",
					Bucket: seiv1alpha1.BucketSnapshot{URI: "s3://my-bucket/snapshots/latest.tar"},
				},
				TrustPeriod:    "9999h0m0s",
				BackfillBlocks: 0,
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
			StateSync: &seiv1alpha1.StateSyncConfig{
				TrustPeriod:    "168h0m0s",
				BackfillBlocks: 0,
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
				PVC:     &seiv1alpha1.GenesisPVCSource{DataPVC: "data-pvc"},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

func snapshotterNode() *seiv1alpha1.SeiNode {
	node := snapshotNode()
	node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		KeepRecent: 5,
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
	want := []string{taskConfigApply, taskSnapshotRestore, taskDiscoverPeers, taskConfigureStateSync, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestTaskProgressionForNode_PeerSync(t *testing.T) {
	got := taskProgressionForNode(peerSyncNode())
	want := []string{taskConfigApply, taskDiscoverPeers, taskConfigureGenesis, taskConfigureStateSync, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestTaskProgressionForNode_Genesis(t *testing.T) {
	got := taskProgressionForNode(genesisNode())
	want := []string{taskConfigApply, taskConfigValidate, taskMarkReady}
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
	want := []string{taskConfigApply, taskDiscoverPeers, taskConfigValidate, taskMarkReady}
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
	if len(plan.Tasks) != 6 {
		t.Fatalf("expected 6 tasks, got %d: %v", len(plan.Tasks), taskTypes(plan))
	}
	for _, task := range plan.Tasks {
		if task.Status != seiv1alpha1.PlannedTaskPending {
			t.Errorf("task %q status = %q, want Pending", task.Type, task.Status)
		}
	}
	if plan.Tasks[0].Type != taskConfigApply {
		t.Errorf("first task = %q, want %q", plan.Tasks[0].Type, taskConfigApply)
	}
}

func taskTypes(plan *seiv1alpha1.TaskPlan) []string {
	var tt []string
	for _, t := range plan.Tasks {
		tt = append(tt, t.Type)
	}
	return tt
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
	// First task should have been submitted (config-apply is now first).
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted task, got %d", len(mock.submitted))
	}
	if mock.submitted[0].TaskType() != taskConfigApply {
		t.Errorf("submitted task = %q, want %q", mock.submitted[0].TaskType(), taskConfigApply)
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
			taskID: runningResult(taskID, taskConfigApply),
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
			taskID: completedResult(taskID, taskConfigApply, nil),
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
			taskID: completedResult(taskID, taskConfigApply, strPtr("network timeout")),
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

func TestReconcile_CompletePlan_SubmitsScheduledTask(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{submitID: taskID}
	node := snapshotterNode()
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}

	r, c := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != statusPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, statusPollInterval)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 scheduled task submitted, got %d", len(mock.submitted))
	}
	if mock.submitted[0].TaskType() != taskSnapshotUpload {
		t.Errorf("task type = %q, want %q", mock.submitted[0].TaskType(), taskSnapshotUpload)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.ScheduledTasks == nil {
		t.Fatal("expected ScheduledTasks to be set")
	}
	if got := updated.Status.ScheduledTasks[taskSnapshotUpload]; got != taskID.String() {
		t.Errorf("ScheduledTasks[%s] = %q, want %q", taskSnapshotUpload, got, taskID.String())
	}
}

func TestReconcile_CompletePlan_SkipsAlreadyScheduled(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotterNode()
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}
	node.Status.ScheduledTasks = map[string]string{
		taskSnapshotUpload: uuid.New().String(),
	}

	r, _ := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileSidecarProgression(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(mock.submitted) != 0 {
		t.Errorf("expected no submissions for already-scheduled task, got %d", len(mock.submitted))
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

func TestResolveMode(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want string
	}{
		{"empty defaults to full", "", "full"},
		{"validator", "validator", "validator"},
		{"rpc", "rpc", "rpc"},
		{"archive", "archive", "archive"},
		{"seed", "seed", "seed"},
		{"indexer", "indexer", "indexer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := genesisNode()
			node.Spec.Mode = tt.mode
			if got := resolveMode(node); got != tt.want {
				t.Errorf("resolveMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigApplyBuilder(t *testing.T) {
	t.Run("default mode is full", func(t *testing.T) {
		task := configApplyBuilder(genesisNode()).(sidecar.ConfigApplyTask)
		if string(task.Intent.Mode) != "full" {
			t.Errorf("Intent.Mode = %q, want %q", task.Intent.Mode, "full")
		}
		if task.Intent.Incremental {
			t.Error("expected Incremental=false for bootstrap")
		}
	})

	t.Run("explicit mode passed through", func(t *testing.T) {
		node := genesisNode()
		node.Spec.Mode = "validator"
		task := configApplyBuilder(node).(sidecar.ConfigApplyTask)
		if string(task.Intent.Mode) != "validator" {
			t.Errorf("Intent.Mode = %q, want %q", task.Intent.Mode, "validator")
		}
	})

	t.Run("CRD overrides included", func(t *testing.T) {
		node := genesisNode()
		node.Spec.Config = &seiv1alpha1.SeiNodeConfigSpec{
			Overrides: map[string]string{
				"evm.http_port": "9545",
				"logging.level": "debug",
			},
		}
		task := configApplyBuilder(node).(sidecar.ConfigApplyTask)
		if task.Intent.Overrides["evm.http_port"] != "9545" {
			t.Errorf("evm.http_port = %q, want %q", task.Intent.Overrides["evm.http_port"], "9545")
		}
		if task.Intent.Overrides["logging.level"] != "debug" {
			t.Errorf("logging.level = %q, want %q", task.Intent.Overrides["logging.level"], "debug")
		}
	})

	t.Run("snapshot restore adds state_sync overrides", func(t *testing.T) {
		task := configApplyBuilder(snapshotNode()).(sidecar.ConfigApplyTask)
		if task.Intent.Overrides["state_sync.enable"] != "true" {
			t.Errorf("state_sync.enable = %q, want %q", task.Intent.Overrides["state_sync.enable"], "true")
		}
		if task.Intent.Overrides["state_sync.use_local_snapshot"] != "true" {
			t.Errorf("state_sync.use_local_snapshot = %q", task.Intent.Overrides["state_sync.use_local_snapshot"])
		}
		if task.Intent.Overrides["state_sync.trust_period"] != "9999h0m0s" {
			t.Errorf("state_sync.trust_period = %q", task.Intent.Overrides["state_sync.trust_period"])
		}
	})

	t.Run("snapshot generation adds storage overrides", func(t *testing.T) {
		node := snapshotNode()
		node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{KeepRecent: 5}
		task := configApplyBuilder(node).(sidecar.ConfigApplyTask)
		if task.Intent.Overrides["storage.pruning"] != "nothing" {
			t.Errorf("storage.pruning = %q", task.Intent.Overrides["storage.pruning"])
		}
		if task.Intent.Overrides["storage.snapshot_interval"] != "2000" {
			t.Errorf("storage.snapshot_interval = %q", task.Intent.Overrides["storage.snapshot_interval"])
		}
		if task.Intent.Overrides["storage.snapshot_keep_recent"] != "5" {
			t.Errorf("storage.snapshot_keep_recent = %q", task.Intent.Overrides["storage.snapshot_keep_recent"])
		}
	})

	t.Run("controller overrides win over CRD overrides", func(t *testing.T) {
		node := snapshotNode()
		node.Spec.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{KeepRecent: 10}
		node.Spec.Config = &seiv1alpha1.SeiNodeConfigSpec{
			Overrides: map[string]string{
				"storage.pruning": "default",
			},
		}
		task := configApplyBuilder(node).(sidecar.ConfigApplyTask)
		if task.Intent.Overrides["storage.pruning"] != "nothing" {
			t.Errorf("controller override should win: storage.pruning = %q, want %q",
				task.Intent.Overrides["storage.pruning"], "nothing")
		}
	})

	t.Run("genesis node has no extra overrides", func(t *testing.T) {
		task := configApplyBuilder(genesisNode()).(sidecar.ConfigApplyTask)
		if len(task.Intent.Overrides) != 0 {
			t.Errorf("expected no overrides for genesis node, got %v", task.Intent.Overrides)
		}
	})

	t.Run("CRD config version passed through", func(t *testing.T) {
		node := genesisNode()
		node.Spec.Config = &seiv1alpha1.SeiNodeConfigSpec{Version: 2}
		task := configApplyBuilder(node).(sidecar.ConfigApplyTask)
		if task.Intent.TargetVersion != 2 {
			t.Errorf("Intent.TargetVersion = %d, want 2", task.Intent.TargetVersion)
		}
	})

	t.Run("zero config version not set in intent", func(t *testing.T) {
		node := genesisNode()
		node.Spec.Config = &seiv1alpha1.SeiNodeConfigSpec{}
		task := configApplyBuilder(node).(sidecar.ConfigApplyTask)
		if task.Intent.TargetVersion != 0 {
			t.Errorf("Intent.TargetVersion = %d, want 0 (use default)", task.Intent.TargetVersion)
		}
	})

	t.Run("config-validate builder produces correct task type", func(t *testing.T) {
		b := taskBuilderForNode(genesisNode(), taskConfigValidate)
		if _, ok := b.(sidecar.ConfigValidateTask); !ok {
			t.Errorf("expected ConfigValidateTask, got %T", b)
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
	if task.Cron != defaultSnapshotUploadCron {
		t.Errorf("Cron = %q, want %q", task.Cron, defaultSnapshotUploadCron)
	}
}

func TestSnapshotUploadTask_NoDestination(t *testing.T) {
	if task := snapshotUploadTask(snapshotNode()); task != nil {
		t.Errorf("expected nil, got %v", task)
	}
}

func TestNeedsStateSync(t *testing.T) {
	if !needsStateSync(peerSyncNode()) {
		t.Error("peerSyncNode: want true (StateSync set)")
	}
	if needsStateSync(genesisNode()) {
		t.Error("genesisNode: want false (StateSync nil)")
	}
	if !needsStateSync(snapshotNode()) {
		t.Error("snapshotNode: want true (StateSync set with snapshot)")
	}
}
