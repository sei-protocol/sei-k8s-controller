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
			FullNode: &seiv1alpha1.FullNodeSpec{
				Sync: &seiv1alpha1.SyncConfig{
					BlockSync: &seiv1alpha1.BlockSyncConfig{
						Snapshot: &seiv1alpha1.SnapshotRestoreConfig{
							Region:      "us-east-1",
							Bucket:      seiv1alpha1.BucketSnapshot{URI: "s3://my-bucket/snapshots/latest.tar"},
							TrustPeriod: "9999h0m0s",
						},
					},
				},
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
			FullNode: &seiv1alpha1.FullNodeSpec{
				Sync: &seiv1alpha1.SyncConfig{
					Peers: &seiv1alpha1.PeerConfig{
						Sources: []seiv1alpha1.PeerSource{
							{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"ChainIdentifier": "atlantic-2"}}},
						},
					},
					StateSync: &seiv1alpha1.StateSyncConfig{},
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
			Validator: &seiv1alpha1.ValidatorSpec{},
			Sidecar:   &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

func snapshotterNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   "sei:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{ChainID: "atlantic-2"},
			Archive: &seiv1alpha1.ArchiveSpec{
				Peers: &seiv1alpha1.PeerConfig{
					Sources: []seiv1alpha1.PeerSource{{
						EC2Tags: &seiv1alpha1.EC2TagsPeerSource{
							Region: "eu-central-1",
							Tags:   map[string]string{"ChainIdentifier": "atlantic-2"},
						},
					}},
				},
				SnapshotGeneration: &seiv1alpha1.SnapshotGenerationConfig{
					KeepRecent: 5,
					Destination: &seiv1alpha1.SnapshotDestination{
						S3: &seiv1alpha1.S3SnapshotDestination{Bucket: "atlantic-2-snapshots", Prefix: "state-sync", Region: "eu-central-1"},
					},
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

// --- Bootstrap mode tests ---

func TestSyncBootstrapMode(t *testing.T) {
	tests := []struct {
		name string
		node *seiv1alpha1.SeiNode
		want string
	}{
		{"snapshot", snapshotNode(), "snapshot"},
		{"state-sync", peerSyncNode(), "state-sync"},
		{"genesis", genesisNode(), "genesis"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sync := syncConfig(tt.node)
			if got := syncBootstrapMode(sync); got != tt.want {
				t.Errorf("syncBootstrapMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Plan building tests ---

func TestBuildPlan_Snapshot(t *testing.T) {
	planner, _ := PlannerForNode(snapshotNode())
	plan := planner.BuildPlan(snapshotNode())
	got := taskTypes(plan)
	want := []string{taskSnapshotRestore, taskConfigApply, taskConfigureStateSync, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_SnapshotWithPeers(t *testing.T) {
	node := snapshotNode()
	node.Spec.FullNode.Sync.Peers = &seiv1alpha1.PeerConfig{
		Sources: []seiv1alpha1.PeerSource{
			{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"Chain": "atlantic-2"}}},
		},
	}
	planner, _ := PlannerForNode(node)
	plan := planner.BuildPlan(node)
	got := taskTypes(plan)
	want := []string{taskSnapshotRestore, taskConfigApply, taskDiscoverPeers, taskConfigureStateSync, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_StateSync(t *testing.T) {
	planner, _ := PlannerForNode(peerSyncNode())
	plan := planner.BuildPlan(peerSyncNode())
	got := taskTypes(plan)
	want := []string{taskConfigureGenesis, taskConfigApply, taskDiscoverPeers, taskConfigureStateSync, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_Genesis(t *testing.T) {
	planner, _ := PlannerForNode(genesisNode())
	plan := planner.BuildPlan(genesisNode())
	got := taskTypes(plan)
	want := []string{taskConfigApply, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_GenesisWithPeers(t *testing.T) {
	node := genesisNode()
	node.Spec.Validator.Sync = &seiv1alpha1.SyncConfig{
		Peers: &seiv1alpha1.PeerConfig{
			Sources: []seiv1alpha1.PeerSource{
				{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"Chain": "arctic-1"}}},
			},
		},
	}
	planner, _ := PlannerForNode(node)
	plan := planner.BuildPlan(node)
	got := taskTypes(plan)
	want := []string{taskConfigApply, taskDiscoverPeers, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func replayerNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-replayer", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   "sei:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{
				ChainID: "pacific-1",
				S3:      &seiv1alpha1.GenesisS3Source{URI: "s3://sei-testnet-genesis-config/pacific-1/genesis.json", Region: "us-east-2"},
			},
			Replayer: &seiv1alpha1.ReplayerSpec{
				Snapshot: seiv1alpha1.SnapshotSource{
					Region: "eu-central-1",
					Bucket: seiv1alpha1.BucketSnapshot{URI: "s3://pacific-1-snapshots/state-sync/"},
				},
				Peers: seiv1alpha1.PeerConfig{
					Sources: []seiv1alpha1.PeerSource{
						{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"ChainIdentifier": "pacific-1"}}},
					},
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

func TestBuildPlan_Replayer(t *testing.T) {
	node := replayerNode()
	planner, _ := PlannerForNode(node)
	plan := planner.BuildPlan(node)
	got := taskTypes(plan)
	want := []string{taskSnapshotRestore, taskConfigureGenesis, taskConfigApply, taskDiscoverPeers, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildTask_Replayer_DiscoverPeers(t *testing.T) {
	node := replayerNode()
	planner, _ := PlannerForNode(node)
	b := planner.BuildTask(node, taskDiscoverPeers)
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

func TestPlannerForNode_Replayer(t *testing.T) {
	node := replayerNode()
	p, err := PlannerForNode(node)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != modeArchive {
		t.Errorf("Mode() = %q, want %q", p.Mode(), modeArchive)
	}
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

func TestBuildPlanPhaseAndTasks(t *testing.T) {
	planner, _ := PlannerForNode(snapshotNode())
	plan := planner.BuildPlan(snapshotNode())
	if plan.Phase != seiv1alpha1.TaskPlanActive {
		t.Errorf("phase = %q, want Active", plan.Phase)
	}
	if len(plan.Tasks) != 5 {
		t.Fatalf("expected 5 tasks, got %d: %v", len(plan.Tasks), taskTypes(plan))
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

func taskTypes(plan *seiv1alpha1.TaskPlan) []string {
	tt := make([]string, 0, len(plan.Tasks))
	for _, t := range plan.Tasks {
		tt = append(tt, t.Type)
	}
	return tt
}

// --- Reconcile progression tests ---

func TestReconcile_CreatesPlanOnFirstRun(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	planner, _ := PlannerForNode(node)
	r, c := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileSidecarProgression(context.Background(), node, planner)
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
	planner, _ := PlannerForNode(node)
	node.Status.InitPlan = planner.BuildPlan(node)
	r, c := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileSidecarProgression(context.Background(), node, planner)
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
	planner, _ := PlannerForNode(node)
	node.Status.InitPlan = planner.BuildPlan(node)
	node.Status.InitPlan.Tasks[0].Status = seiv1alpha1.PlannedTaskSubmitted
	node.Status.InitPlan.Tasks[0].TaskID = taskID.String()

	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node, planner)
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
	planner, _ := PlannerForNode(node)
	node.Status.InitPlan = planner.BuildPlan(node)
	node.Status.InitPlan.Tasks[0].Status = seiv1alpha1.PlannedTaskSubmitted
	node.Status.InitPlan.Tasks[0].TaskID = taskID.String()

	r, c := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node, planner)
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
	planner, _ := PlannerForNode(node)
	node.Status.InitPlan = planner.BuildPlan(node)
	node.Status.InitPlan.Tasks[0].Status = seiv1alpha1.PlannedTaskSubmitted
	node.Status.InitPlan.Tasks[0].TaskID = taskID.String()

	r, c := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node, planner)
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
	planner, _ := PlannerForNode(node)
	node.Status.InitPlan = planner.BuildPlan(node)
	for i := range node.Status.InitPlan.Tasks {
		node.Status.InitPlan.Tasks[i].Status = seiv1alpha1.PlannedTaskComplete
	}

	r, c := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node, planner)
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
	planner, _ := PlannerForNode(node)
	node.Status.InitPlan = planner.BuildPlan(node)
	node.Status.InitPlan.Phase = seiv1alpha1.TaskPlanFailed

	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node, planner)
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
	planner, _ := PlannerForNode(node)
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}

	r, c := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node, planner)
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
	planner, _ := PlannerForNode(node)
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}
	node.Status.ScheduledTasks = map[string]string{
		taskSnapshotUpload: uuid.New().String(),
	}

	r, _ := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileSidecarProgression(context.Background(), node, planner)
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
	planner, _ := PlannerForNode(node)
	node.Status.InitPlan = planner.BuildPlan(node)

	r, c := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node, planner)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != bootstrapPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, bootstrapPollInterval)
	}
	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.InitPlan.Tasks[0].Status != seiv1alpha1.PlannedTaskPending {
		t.Errorf("task status = %q, want Pending after submit failure", updated.Status.InitPlan.Tasks[0].Status)
	}
}

func TestReconcile_GetTaskError_RequeuesGracefully(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{getTaskErr: fmt.Errorf("connection refused")}
	node := snapshotNode()
	planner, _ := PlannerForNode(node)
	node.Status.InitPlan = planner.BuildPlan(node)
	node.Status.InitPlan.Tasks[0].Status = seiv1alpha1.PlannedTaskSubmitted
	node.Status.InitPlan.Tasks[0].TaskID = taskID.String()

	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileSidecarProgression(context.Background(), node, planner)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != bootstrapPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, bootstrapPollInterval)
	}
}

// --- TaskBuilder tests ---

func TestBuildTask_SnapshotRestore(t *testing.T) {
	node := snapshotNode()
	planner, _ := PlannerForNode(node)
	b := planner.BuildTask(node, taskSnapshotRestore)
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

func TestBuildTask_DiscoverPeers_EC2Tags(t *testing.T) {
	node := snapshotNode()
	node.Spec.FullNode.Sync.Peers = &seiv1alpha1.PeerConfig{
		Sources: []seiv1alpha1.PeerSource{
			{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"ChainIdentifier": "atlantic-2"}}},
		},
	}
	planner, _ := PlannerForNode(node)
	b := planner.BuildTask(node, taskDiscoverPeers)
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

func TestBuildTask_ConfigureGenesis_WithS3(t *testing.T) {
	b := configureGenesisBuilder(peerSyncNode())
	task, ok := b.(sidecar.ConfigureGenesisTask)
	if !ok {
		t.Fatalf("expected ConfigureGenesisTask, got %T", b)
	}
	if task.URI != "s3://sei-testnet-genesis-config/atlantic-2/genesis.json" {
		t.Errorf("URI = %q", task.URI)
	}
}

func TestBuildTask_MarkReady(t *testing.T) {
	node := snapshotNode()
	planner, _ := PlannerForNode(node)
	b := planner.BuildTask(node, taskMarkReady)
	if _, ok := b.(sidecar.MarkReadyTask); !ok {
		t.Errorf("expected MarkReadyTask, got %T", b)
	}
}

// --- ConfigApply tests ---

func TestConfigApply_FullNodeMode(t *testing.T) {
	node := genesisNode()
	// Genesis node uses Validator sub-spec, switch to FullNode for this test
	node.Spec.Validator = nil
	node.Spec.FullNode = &seiv1alpha1.FullNodeSpec{}
	planner, _ := PlannerForNode(node)
	b := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	if string(task.Intent.Mode) != modeFull {
		t.Errorf("Intent.Mode = %q, want %q", task.Intent.Mode, modeFull)
	}
}

func TestConfigApply_ValidatorMode(t *testing.T) {
	node := genesisNode()
	planner, _ := PlannerForNode(node)
	b := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	if string(task.Intent.Mode) != modeValidator {
		t.Errorf("Intent.Mode = %q, want %q", task.Intent.Mode, modeValidator)
	}
}

func TestConfigApply_ArchiveWithSnapshotGeneration(t *testing.T) {
	node := snapshotterNode()
	planner, _ := PlannerForNode(node)
	b := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	if string(task.Intent.Mode) != modeArchive {
		t.Errorf("Intent.Mode = %q, want %q", task.Intent.Mode, modeArchive)
	}
	if task.Intent.Overrides["storage.snapshot_interval"] != "2000" {
		t.Errorf("storage.snapshot_interval = %q", task.Intent.Overrides["storage.snapshot_interval"])
	}
	if task.Intent.Overrides["storage.snapshot_keep_recent"] != "5" {
		t.Errorf("storage.snapshot_keep_recent = %q", task.Intent.Overrides["storage.snapshot_keep_recent"])
	}
}

func TestConfigApply_StateSyncFieldsNotSet(t *testing.T) {
	node := snapshotNode()
	planner, _ := PlannerForNode(node)
	b := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	for _, key := range []string{"state_sync.enable", "state_sync.use_local_snapshot", "state_sync.trust_period"} {
		if _, ok := task.Intent.Overrides[key]; ok {
			t.Errorf("unexpected override %q: configure-state-sync task should own this field", key)
		}
	}
}

func TestConfigApply_FullNodeWithSnapshotGeneration(t *testing.T) {
	node := snapshotNode()
	node.Spec.FullNode.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{KeepRecent: 5}
	planner, _ := PlannerForNode(node)
	b := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	if task.Intent.Overrides["storage.pruning"] != "nothing" {
		t.Errorf("storage.pruning = %q", task.Intent.Overrides["storage.pruning"])
	}
	if task.Intent.Overrides["storage.snapshot_interval"] != "2000" {
		t.Errorf("storage.snapshot_interval = %q", task.Intent.Overrides["storage.snapshot_interval"])
	}
}

func TestConfigApply_GenesisNodeNoOverrides(t *testing.T) {
	node := genesisNode()
	planner, _ := PlannerForNode(node)
	b := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	if len(task.Intent.Overrides) != 0 {
		t.Errorf("expected no overrides for genesis node, got %v", task.Intent.Overrides)
	}
}

// --- Snapshot upload tests ---

func TestSnapshotUploadTask_WithDestination(t *testing.T) {
	b := snapshotUploadTaskFromSpec(snapshotterNode())
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
	if task := snapshotUploadTaskFromSpec(snapshotNode()); task != nil {
		t.Errorf("expected nil, got %v", task)
	}
}

// --- PlannerForNode dispatch tests ---

func TestPlannerForNode_FullNode(t *testing.T) {
	node := snapshotNode()
	p, err := PlannerForNode(node)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != modeFull {
		t.Errorf("Mode() = %q, want %q", p.Mode(), modeFull)
	}
}

func TestPlannerForNode_Archive(t *testing.T) {
	node := snapshotterNode()
	p, err := PlannerForNode(node)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != modeArchive {
		t.Errorf("Mode() = %q, want %q", p.Mode(), modeArchive)
	}
}

func TestPlannerForNode_Validator(t *testing.T) {
	node := genesisNode()
	p, err := PlannerForNode(node)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != modeValidator {
		t.Errorf("Mode() = %q, want %q", p.Mode(), modeValidator)
	}
}

func TestPlannerForNode_NoSubSpec(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test",
			Image:   "sei:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{ChainID: "test"},
		},
	}
	_, err := PlannerForNode(node)
	if err == nil {
		t.Error("expected error for node with no sub-spec")
	}
}
