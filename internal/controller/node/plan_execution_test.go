package node

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	testBootstrapImage   = "sei:bootstrap"
	testBootstrapImageV1 = "sei:bootstrap-v1"
	testSnapshotRegion   = "eu-central-1"
)

type mockSidecarClient struct {
	submitted []sidecar.TaskRequest
	submitErr error
	submitID  uuid.UUID

	taskResults map[uuid.UUID]*sidecar.TaskResult
	getTaskErr  error

	listedTasks []sidecar.TaskResult
	listTaskErr error
}

func (m *mockSidecarClient) SubmitTask(_ context.Context, task sidecar.TaskRequest) (uuid.UUID, error) {
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

func (m *mockSidecarClient) ListTasks(_ context.Context) ([]sidecar.TaskResult, error) {
	if m.listTaskErr != nil {
		return nil, m.listTaskErr
	}
	if m.listedTasks != nil {
		return m.listedTasks, nil
	}
	return []sidecar.TaskResult{}, nil
}

func strPtr(s string) *string { return &s }

func completedResult(id uuid.UUID, taskType string, taskErr *string) *sidecar.TaskResult {
	now := time.Now()
	status := sidecar.Completed
	if taskErr != nil && *taskErr != "" {
		status = sidecar.Failed
	}
	return &sidecar.TaskResult{
		Id:          id,
		Type:        taskType,
		Status:      status,
		SubmittedAt: now.Add(-10 * time.Second),
		CompletedAt: &now,
		Error:       taskErr,
	}
}

func runningResult(id uuid.UUID, taskType string) *sidecar.TaskResult {
	return &sidecar.TaskResult{
		Id:          id,
		Type:        taskType,
		Status:      sidecar.Running,
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
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(100),
		Platform: DefaultPlatformConfig(),
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

// --- Test node constructors ---

func snapshotNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   "sei:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{},
			FullNode: &seiv1alpha1.FullNodeSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{
					S3:          &seiv1alpha1.S3SnapshotSource{TargetHeight: 100000000},
					TrustPeriod: "9999h0m0s",
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
				S3: &seiv1alpha1.GenesisS3Source{URI: "s3://sei-testnet-genesis-config/atlantic-2/genesis.json", Region: "us-east-2"},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{
				Peers: []seiv1alpha1.PeerSource{
					{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"ChainIdentifier": "atlantic-2"}}},
				},
				Snapshot: &seiv1alpha1.SnapshotSource{
					StateSync: &seiv1alpha1.StateSyncSource{},
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
				PVC: &seiv1alpha1.GenesisPVCSource{DataPVC: "data-pvc"},
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
			Genesis: seiv1alpha1.GenesisConfiguration{},
			Archive: &seiv1alpha1.ArchiveSpec{
				Peers: []seiv1alpha1.PeerSource{{
					EC2Tags: &seiv1alpha1.EC2TagsPeerSource{
						Region: "eu-central-1",
						Tags:   map[string]string{"ChainIdentifier": "atlantic-2"},
					},
				}},
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

func replayerNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-replayer", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "pacific-1",
			Image:   "sei:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{
				S3: &seiv1alpha1.GenesisS3Source{URI: "s3://sei-testnet-genesis-config/pacific-1/genesis.json", Region: "us-east-2"},
			},
			Replayer: &seiv1alpha1.ReplayerSpec{
				Snapshot: seiv1alpha1.SnapshotSource{
					S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 100000000},
				},
				Peers: []seiv1alpha1.PeerSource{
					{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"ChainIdentifier": "pacific-1"}}},
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

// --- Bootstrap mode tests ---

func TestBootstrapMode(t *testing.T) {
	tests := []struct {
		name string
		snap *seiv1alpha1.SnapshotSource
		want string
	}{
		{"snapshot", &seiv1alpha1.SnapshotSource{S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 1}}, "snapshot"},
		{"state-sync", &seiv1alpha1.SnapshotSource{StateSync: &seiv1alpha1.StateSyncSource{}}, "state-sync"},
		{"genesis", nil, "genesis"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bootstrapMode(tt.snap); got != tt.want {
				t.Errorf("bootstrapMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Plan building tests ---

func TestBuildPlan_Snapshot(t *testing.T) {
	planner, _ := PlannerForNode(snapshotNode(), testSnapshotRegion)
	plan := planner.BuildPlan(snapshotNode())
	got := taskTypes(plan)
	want := []string{taskSnapshotRestore, taskConfigureGenesis, taskConfigApply, taskConfigureStateSync, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_SnapshotWithPeers(t *testing.T) {
	node := snapshotNode()
	node.Spec.FullNode.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"Chain": "atlantic-2"}}},
	}
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	plan := planner.BuildPlan(node)
	got := taskTypes(plan)
	want := []string{taskSnapshotRestore, taskConfigureGenesis, taskConfigApply, taskDiscoverPeers, taskConfigureStateSync, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_StateSync(t *testing.T) {
	planner, _ := PlannerForNode(peerSyncNode(), testSnapshotRegion)
	plan := planner.BuildPlan(peerSyncNode())
	got := taskTypes(plan)
	want := []string{taskConfigureGenesis, taskConfigApply, taskDiscoverPeers, taskConfigureStateSync, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_Genesis(t *testing.T) {
	planner, _ := PlannerForNode(genesisNode(), testSnapshotRegion)
	plan := planner.BuildPlan(genesisNode())
	got := taskTypes(plan)
	want := []string{taskConfigureGenesis, taskConfigApply, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_GenesisWithPeers(t *testing.T) {
	node := genesisNode()
	node.Spec.Validator.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"Chain": "arctic-1"}}},
	}
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	plan := planner.BuildPlan(node)
	got := taskTypes(plan)
	want := []string{taskConfigureGenesis, taskConfigApply, taskDiscoverPeers, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_Replayer(t *testing.T) {
	node := replayerNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	plan := planner.BuildPlan(node)
	got := taskTypes(plan)
	want := []string{taskSnapshotRestore, taskConfigureGenesis, taskConfigApply, taskDiscoverPeers, taskConfigureStateSync, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_Archive(t *testing.T) {
	node := snapshotterNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	plan := planner.BuildPlan(node)
	got := taskTypes(plan)
	want := []string{taskConfigureGenesis, taskConfigApply, taskDiscoverPeers, taskConfigureStateSync, taskConfigValidate, taskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildTask_Replayer_DiscoverPeers(t *testing.T) {
	node := replayerNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, err := planner.BuildTask(node, taskDiscoverPeers)
	if err != nil {
		t.Fatalf("BuildTask error: %v", err)
	}
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

func TestBuildTask_Replayer_ConfigureStateSync(t *testing.T) {
	node := replayerNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, err := planner.BuildTask(node, taskConfigureStateSync)
	if err != nil {
		t.Fatalf("BuildTask error: %v", err)
	}
	task, ok := b.(sidecar.ConfigureStateSyncTask)
	if !ok {
		t.Fatalf("expected ConfigureStateSyncTask, got %T", b)
	}
	if !task.UseLocalSnapshot {
		t.Error("UseLocalSnapshot should be true for S3 snapshot source")
	}
}

func TestPlannerForNode_Replayer(t *testing.T) {
	node := replayerNode()
	p, err := PlannerForNode(node, testSnapshotRegion)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != string(seiconfig.ModeFull) {
		t.Errorf("Mode() = %q, want %q", p.Mode(), string(seiconfig.ModeFull))
	}
}

// --- Config overrides passthrough tests ---

func TestConfigApply_ReplayerPassesSpecOverrides(t *testing.T) {
	node := replayerNode()
	node.Spec.Overrides = map[string]string{
		"giga_executor.enabled": "true",
		"evm.http_port":         "8545",
	}
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskConfigApply)
	task, ok := b.(sidecar.ConfigApplyTask)
	if !ok {
		t.Fatalf("expected ConfigApplyTask, got %T", b)
	}
	if task.Intent.Overrides["giga_executor.enabled"] != "true" {
		t.Errorf("giga_executor.enabled = %q, want %q", task.Intent.Overrides["giga_executor.enabled"], "true")
	}
	if task.Intent.Overrides["evm.http_port"] != "8545" {
		t.Errorf("evm.http_port = %q, want %q", task.Intent.Overrides["evm.http_port"], "8545")
	}
}

func TestConfigApply_FullNodeMergesOverrides(t *testing.T) {
	node := snapshotNode()
	node.Spec.Overrides = map[string]string{
		"giga_executor.enabled": "true",
	}
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskConfigApply)
	task, ok := b.(sidecar.ConfigApplyTask)
	if !ok {
		t.Fatalf("expected ConfigApplyTask, got %T", b)
	}
	if task.Intent.Overrides["giga_executor.enabled"] != "true" {
		t.Errorf("giga_executor.enabled = %q, want %q", task.Intent.Overrides["giga_executor.enabled"], "true")
	}
	if task.Intent.Mode != seiconfig.ModeFull {
		t.Errorf("Mode = %q, want %q", task.Intent.Mode, seiconfig.ModeFull)
	}
}

func TestConfigApply_UserOverridesTakePrecedence(t *testing.T) {
	node := snapshotterNode()
	node.Spec.Overrides = map[string]string{
		"storage.snapshot_keep_recent": "99",
	}
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskConfigApply)
	task, ok := b.(sidecar.ConfigApplyTask)
	if !ok {
		t.Fatalf("expected ConfigApplyTask, got %T", b)
	}
	if task.Intent.Overrides["storage.snapshot_keep_recent"] != "99" {
		t.Errorf("user override should take precedence, got %q", task.Intent.Overrides["storage.snapshot_keep_recent"])
	}
}

func TestConfigApply_ReplayerDefaultOverrides(t *testing.T) {
	node := replayerNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	if string(task.Intent.Mode) != string(seiconfig.ModeFull) {
		t.Errorf("Intent.Mode = %q, want %q", task.Intent.Mode, seiconfig.ModeFull)
	}
	checks := map[string]string{
		keyConcurrencyWorkers: defaultConcurrencyWorkers,
		keyPruning:            valCustom,
		keyPruningKeepRecent:  "86400",
		keyPruningKeepEvery:   "500",
		keyPruningInterval:    "10",

		keySCAsyncCommitBuffer:       "100",
		keySCSnapshotKeepRecent:      "2",
		keySCSnapshotMinTimeInterval: "3600",
	}
	for k, want := range checks {
		if got := task.Intent.Overrides[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
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
	planner, _ := PlannerForNode(snapshotNode(), testSnapshotRegion)
	plan := planner.BuildPlan(snapshotNode())
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
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	_, err := r.reconcilePending(ctx, node, planner)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	node = fetchNode(t, c, node.Name, node.Namespace)
	planner, _ = PlannerForNode(node, testSnapshotRegion)
	sc := r.buildSidecarClient(node)
	_, err = r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
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
	if mock.submitted[0].Type != taskSnapshotRestore {
		t.Errorf("submitted task = %q, want %q", mock.submitted[0].Type, taskSnapshotRestore)
	}
}

func TestReconcile_SubmitsFirstPendingTask(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{submitID: taskID}
	node := snapshotNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	node.Status.InitPlan = planner.BuildPlan(node)
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	sc := r.buildSidecarClient(node)
	_, err := r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
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

func TestSubmitTask_AdoptsOrphanedTask(t *testing.T) {
	orphanedID := uuid.New()
	mock := &mockSidecarClient{
		listedTasks: []sidecar.TaskResult{
			*runningResult(orphanedID, taskSnapshotRestore),
		},
	}
	node := snapshotNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	node.Status.InitPlan = planner.BuildPlan(node)

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	sc := r.buildSidecarClient(node)
	_, err := r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	if len(mock.submitted) != 0 {
		t.Fatalf("expected 0 submissions (should adopt), got %d", len(mock.submitted))
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	task := updated.Status.InitPlan.Tasks[0]
	if task.Status != seiv1alpha1.PlannedTaskSubmitted {
		t.Errorf("task status = %q, want Submitted", task.Status)
	}
	if task.TaskID != orphanedID.String() {
		t.Errorf("taskID = %q, want %q (adopted orphan)", task.TaskID, orphanedID.String())
	}
}

func TestSubmitTask_ListTasksError_Requeues(t *testing.T) {
	mock := &mockSidecarClient{
		listTaskErr: fmt.Errorf("connection refused"),
	}
	node := snapshotNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	node.Status.InitPlan = planner.BuildPlan(node)

	r, _ := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	sc := r.buildSidecarClient(node)
	result, err := r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != taskPollInterval {
		t.Errorf("requeue = %v, want %v", result.RequeueAfter, taskPollInterval)
	}
	if len(mock.submitted) != 0 {
		t.Fatalf("expected 0 submissions when ListTasks fails, got %d", len(mock.submitted))
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
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	node.Status.InitPlan = planner.BuildPlan(node)
	node.Status.InitPlan.Tasks[0].Status = seiv1alpha1.PlannedTaskSubmitted
	node.Status.InitPlan.Tasks[0].TaskID = taskID.String()

	r, _ := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	sc := r.buildSidecarClient(node)
	result, err := r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != taskPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, taskPollInterval)
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
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	node.Status.InitPlan = planner.BuildPlan(node)
	node.Status.InitPlan.Tasks[0].Status = seiv1alpha1.PlannedTaskSubmitted
	node.Status.InitPlan.Tasks[0].TaskID = taskID.String()

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	sc := r.buildSidecarClient(node)
	result, err := r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != immediateRequeue {
		t.Errorf("expected immediate requeue after task completion, got %v", result.RequeueAfter)
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
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	node.Status.InitPlan = planner.BuildPlan(node)
	node.Status.InitPlan.Tasks[0].Status = seiv1alpha1.PlannedTaskSubmitted
	node.Status.InitPlan.Tasks[0].TaskID = taskID.String()

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	sc := r.buildSidecarClient(node)
	result, err := r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
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
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	node.Status.InitPlan = planner.BuildPlan(node)
	for i := range node.Status.InitPlan.Tasks {
		node.Status.InitPlan.Tasks[i].Status = seiv1alpha1.PlannedTaskComplete
	}

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	sc := r.buildSidecarClient(node)
	result, err := r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when marking plan complete")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.InitPlan.Phase != seiv1alpha1.TaskPlanComplete {
		t.Errorf("plan phase = %q, want Complete", updated.Status.InitPlan.Phase)
	}
}

func TestReconcile_FailedPlan_NoOps(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	node.Status.InitPlan = planner.BuildPlan(node)
	node.Status.InitPlan.Phase = seiv1alpha1.TaskPlanFailed

	r, _ := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	sc := r.buildSidecarClient(node)
	result, err := r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
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
	node.Status.Phase = seiv1alpha1.PhaseRunning

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	result, err := r.reconcileRunning(ctx, node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != statusPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, statusPollInterval)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 scheduled task submitted, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != taskSnapshotUpload {
		t.Errorf("task type = %q, want %q", mock.submitted[0].Type, taskSnapshotUpload)
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
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.ScheduledTasks = map[string]string{
		taskSnapshotUpload: uuid.New().String(),
	}

	r, _ := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	_, err := r.reconcileRunning(ctx, node)
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
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	node.Status.InitPlan = planner.BuildPlan(node)

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	sc := r.buildSidecarClient(node)
	result, err := r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != taskPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, taskPollInterval)
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
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	node.Status.InitPlan = planner.BuildPlan(node)
	node.Status.InitPlan.Tasks[0].Status = seiv1alpha1.PlannedTaskSubmitted
	node.Status.InitPlan.Tasks[0].TaskID = taskID.String()

	r, _ := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	sc := r.buildSidecarClient(node)
	result, err := r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != taskPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, taskPollInterval)
	}
}

// --- TaskBuilder tests ---

func TestBuildTask_SnapshotRestore(t *testing.T) {
	node := snapshotNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskSnapshotRestore)
	task, ok := b.(sidecar.SnapshotRestoreTask)
	if !ok {
		t.Fatalf("expected SnapshotRestoreTask, got %T", b)
	}
	if task.Bucket != "atlantic-2-snapshots" {
		t.Errorf("Bucket = %q, want %q", task.Bucket, "atlantic-2-snapshots")
	}
	if task.Region != testSnapshotRegion {
		t.Errorf("Region = %q, want %q", task.Region, testSnapshotRegion)
	}
}

func TestBuildTask_DiscoverPeers_EC2Tags(t *testing.T) {
	node := snapshotNode()
	node.Spec.FullNode.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"ChainIdentifier": "atlantic-2"}}},
	}
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskDiscoverPeers)
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
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskMarkReady)
	if _, ok := b.(sidecar.MarkReadyTask); !ok {
		t.Errorf("expected MarkReadyTask, got %T", b)
	}
}

// --- ConfigApply tests ---

func TestConfigApply_FullNodeMode(t *testing.T) {
	node := genesisNode()
	node.Spec.Validator = nil
	node.Spec.FullNode = &seiv1alpha1.FullNodeSpec{}
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	if string(task.Intent.Mode) != string(seiconfig.ModeFull) {
		t.Errorf("Intent.Mode = %q, want %q", task.Intent.Mode, string(seiconfig.ModeFull))
	}
}

func TestConfigApply_ValidatorMode(t *testing.T) {
	node := genesisNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	if string(task.Intent.Mode) != string(seiconfig.ModeValidator) {
		t.Errorf("Intent.Mode = %q, want %q", task.Intent.Mode, string(seiconfig.ModeValidator))
	}
}

func TestConfigApply_ArchiveWithSnapshotGeneration(t *testing.T) {
	node := snapshotterNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	if string(task.Intent.Mode) != string(seiconfig.ModeArchive) {
		t.Errorf("Intent.Mode = %q, want %q", task.Intent.Mode, string(seiconfig.ModeArchive))
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
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	for _, key := range []string{"state_sync.enable", "state_sync.use_local_snapshot", "state_sync.trust_period"} {
		if _, ok := task.Intent.Overrides[key]; ok {
			t.Errorf("unexpected override %q: configure-state-sync task should own this field", key)
		}
	}
}

func TestConfigApply_FullNodeSnapshotProducerProfile(t *testing.T) {
	node := snapshotNode()
	node.Spec.FullNode.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{KeepRecent: 5}
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	checks := map[string]string{
		keyPruning:            valCustom,
		keyPruningKeepRecent:  "50000",
		keyPruningKeepEvery:   "0",
		keyPruningInterval:    "10",
		keyMinRetainBlocks:    "50000",
		keyConcurrencyWorkers: defaultConcurrencyWorkers,
		keySnapshotInterval:   "2000",
		keySnapshotKeepRecent: "5",
	}
	for k, want := range checks {
		if got := task.Intent.Overrides[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestConfigApply_FullNodeRPCProfile(t *testing.T) {
	node := genesisNode()
	node.Spec.Validator = nil
	node.Spec.FullNode = &seiv1alpha1.FullNodeSpec{}
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	checks := map[string]string{
		keyPruning:            valCustom,
		keyPruningKeepRecent:  "86400",
		keyPruningKeepEvery:   "500",
		keyPruningInterval:    "10",
		keyConcurrencyWorkers: defaultConcurrencyWorkers,
	}
	for k, want := range checks {
		if got := task.Intent.Overrides[k]; got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if _, ok := task.Intent.Overrides[keyMinRetainBlocks]; ok {
		t.Errorf("RPC profile should not set min_retain_blocks (use ModeFull default)")
	}
}

func TestConfigApply_ArchiveDefaultOverrides(t *testing.T) {
	node := snapshotterNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	if task.Intent.Overrides[keyConcurrencyWorkers] != defaultConcurrencyWorkers {
		t.Errorf("concurrency_workers = %q, want %q", task.Intent.Overrides[keyConcurrencyWorkers], defaultConcurrencyWorkers)
	}
	if _, ok := task.Intent.Overrides[keyPruning]; ok {
		t.Errorf("archive should not set pruning override (ModeArchive handles it), got %q", task.Intent.Overrides[keyPruning])
	}
}

func TestConfigApply_GenesisNodeNoOverrides(t *testing.T) {
	node := genesisNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskConfigApply)
	task := b.(sidecar.ConfigApplyTask)
	if len(task.Intent.Overrides) != 0 {
		t.Errorf("expected no overrides for genesis node, got %v", task.Intent.Overrides)
	}
}

// --- Snapshot upload tests ---

func TestSnapshotUploadScheduledTask_WithDestination(t *testing.T) {
	builder := snapshotUploadScheduledTask(snapshotterNode())
	if builder == nil {
		t.Fatal("expected non-nil builder")
	}
	task := builder.(sidecar.SnapshotUploadTask)
	if task.Bucket != "atlantic-2-snapshots" {
		t.Errorf("Bucket = %q, want %q", task.Bucket, "atlantic-2-snapshots")
	}
	if task.Schedule == nil || task.Schedule.Cron == nil {
		t.Fatal("expected schedule config on task")
	}
	if *task.Schedule.Cron != defaultSnapshotUploadCron {
		t.Errorf("Cron = %q, want %q", *task.Schedule.Cron, defaultSnapshotUploadCron)
	}
}

func TestSnapshotUploadScheduledTask_NoDestination(t *testing.T) {
	builder := snapshotUploadScheduledTask(snapshotNode())
	if builder != nil {
		t.Errorf("expected nil builder, got %v", builder)
	}
}

// --- PlannerForNode dispatch tests ---

func TestPlannerForNode_FullNode(t *testing.T) {
	node := snapshotNode()
	p, err := PlannerForNode(node, testSnapshotRegion)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != string(seiconfig.ModeFull) {
		t.Errorf("Mode() = %q, want %q", p.Mode(), string(seiconfig.ModeFull))
	}
}

func TestPlannerForNode_Archive(t *testing.T) {
	node := snapshotterNode()
	p, err := PlannerForNode(node, testSnapshotRegion)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != string(seiconfig.ModeArchive) {
		t.Errorf("Mode() = %q, want %q", p.Mode(), string(seiconfig.ModeArchive))
	}
}

func TestPlannerForNode_Validator(t *testing.T) {
	node := genesisNode()
	p, err := PlannerForNode(node, testSnapshotRegion)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != string(seiconfig.ModeValidator) {
		t.Errorf("Mode() = %q, want %q", p.Mode(), string(seiconfig.ModeValidator))
	}
}

func TestPlannerForNode_NoSubSpec(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test",
			Image:   "sei:latest",
			Genesis: seiv1alpha1.GenesisConfiguration{},
		},
	}
	_, err := PlannerForNode(node, testSnapshotRegion)
	if err == nil {
		t.Error("expected error for node with no sub-spec")
	}
}

// --- Result export tests ---

func TestResultExportScheduledTask_ReplayerWithExport(t *testing.T) {
	node := replayerNode()
	node.Spec.Replayer.ResultExport = &seiv1alpha1.ResultExportConfig{}
	builder := resultExportScheduledTask(node)
	if builder == nil {
		t.Fatal("expected non-nil builder")
	}
	task, ok := builder.(sidecar.ResultExportTask)
	if !ok {
		t.Fatalf("expected ResultExportTask, got %T", builder)
	}
	if task.Bucket != "sei-node-mvp" {
		t.Errorf("Bucket = %q, want %q", task.Bucket, "sei-node-mvp")
	}
	if task.Prefix != "shadow-results/pacific-1/" {
		t.Errorf("Prefix = %q, want %q", task.Prefix, "shadow-results/pacific-1/")
	}
	if task.Region != resultExportRegion {
		t.Errorf("Region = %q, want %q", task.Region, resultExportRegion)
	}
	if task.Schedule == nil || task.Schedule.Cron == nil {
		t.Fatal("expected schedule config on task")
	}
	if *task.Schedule.Cron != defaultResultExportCron {
		t.Errorf("Cron = %q, want %q", *task.Schedule.Cron, defaultResultExportCron)
	}
}

func TestResultExportScheduledTask_ReplayerWithoutExport(t *testing.T) {
	node := replayerNode()
	builder := resultExportScheduledTask(node)
	if builder != nil {
		t.Errorf("expected nil builder, got %v", builder)
	}
}

func TestResultExportScheduledTask_FullNode(t *testing.T) {
	node := snapshotNode()
	builder := resultExportScheduledTask(node)
	if builder != nil {
		t.Errorf("expected nil builder for full node, got %v", builder)
	}
}

// --- Replayer validation tests ---

func TestReplayerPlanner_Validate_RequiresTargetHeight(t *testing.T) {
	node := replayerNode()
	node.Spec.Replayer.Snapshot.S3.TargetHeight = 0
	planner, err := PlannerForNode(node, testSnapshotRegion)
	if err != nil {
		t.Fatal(err)
	}
	if err := planner.Validate(node); err == nil {
		t.Error("expected error when targetHeight is 0")
	}
}

// --- Replayer BuildTask tests ---

func TestBuildTask_Replayer_SnapshotRestore_InferredBucket(t *testing.T) {
	node := replayerNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	b, _ := planner.BuildTask(node, taskSnapshotRestore)
	task, ok := b.(sidecar.SnapshotRestoreTask)
	if !ok {
		t.Fatalf("expected SnapshotRestoreTask, got %T", b)
	}
	if task.Bucket != "pacific-1-snapshots" {
		t.Errorf("Bucket = %q, want %q", task.Bucket, "pacific-1-snapshots")
	}
	if task.Prefix != "state-sync/" {
		t.Errorf("Prefix = %q, want %q", task.Prefix, "state-sync/")
	}
	if task.Region != testSnapshotRegion {
		t.Errorf("Region = %q, want %q", task.Region, testSnapshotRegion)
	}
}

// --- Reconcile replayer runtime task tests ---

func TestReconcile_CompletePlan_SubmitsResultExportForReplayer(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{submitID: taskID}
	node := replayerNode()
	node.Spec.Replayer.ResultExport = &seiv1alpha1.ResultExportConfig{}
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}

	r, c := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileRunning(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 scheduled task submitted, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != taskResultExport {
		t.Errorf("task type = %q, want %q", mock.submitted[0].Type, taskResultExport)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.ScheduledTasks == nil {
		t.Fatal("expected ScheduledTasks to be set")
	}
	if got := updated.Status.ScheduledTasks[taskResultExport]; got != taskID.String() {
		t.Errorf("ScheduledTasks[%s] = %q, want %q", taskResultExport, got, taskID.String())
	}
}

// --- Phase transition tests ---

func TestReconcilePending_NoBootstrap_SetsPreInitializingWithEmptyPlan(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	r, c := newProgressionReconciler(t, mock, node)

	planner, _ := PlannerForNode(node, testSnapshotRegion)
	result, err := r.reconcilePending(context.Background(), node, planner)
	if err != nil {
		t.Fatalf("reconcilePending error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after reconcilePending")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhasePreInitializing {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhasePreInitializing)
	}
	if updated.Status.InitPlan == nil {
		t.Fatal("expected InitPlan to be created")
	}
	if updated.Status.PreInitPlan == nil {
		t.Fatal("expected PreInitPlan to be created (empty)")
	}
	if len(updated.Status.PreInitPlan.Tasks) != 0 {
		t.Errorf("expected empty PreInitPlan tasks, got %d", len(updated.Status.PreInitPlan.Tasks))
	}
}

func TestReconcilePending_WithBootstrap_SetsPreInitializing(t *testing.T) {
	mock := &mockSidecarClient{}
	node := replayerNode()
	node.Spec.Replayer.Snapshot.BootstrapImage = testBootstrapImage
	r, c := newProgressionReconciler(t, mock, node)

	planner, _ := PlannerForNode(node, testSnapshotRegion)
	result, err := r.reconcilePending(context.Background(), node, planner)
	if err != nil {
		t.Fatalf("reconcilePending error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after reconcilePending")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhasePreInitializing {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhasePreInitializing)
	}
	if updated.Status.PreInitPlan == nil {
		t.Fatal("expected PreInitPlan to be created")
	}
	if updated.Status.InitPlan == nil {
		t.Fatal("expected InitPlan to be created")
	}
}

func TestReconcileInitializing_PlanComplete_TransitionsToRunning(t *testing.T) {
	mock := &mockSidecarClient{}
	node := genesisNode()
	node.Status.Phase = seiv1alpha1.PhaseInitializing
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanComplete,
	}
	r, c := newProgressionReconciler(t, mock, node)

	planner, _ := PlannerForNode(node, testSnapshotRegion)
	result, err := r.reconcileInitializing(context.Background(), node, planner)
	if err != nil {
		t.Fatalf("reconcileInitializing error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after plan complete")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhaseRunning {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhaseRunning)
	}
}

func TestReconcileInitializing_PlanFailed_TransitionsToFailed(t *testing.T) {
	mock := &mockSidecarClient{}
	node := genesisNode()
	node.Status.Phase = seiv1alpha1.PhaseInitializing
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanFailed,
	}
	r, c := newProgressionReconciler(t, mock, node)

	planner, _ := PlannerForNode(node, testSnapshotRegion)
	result, err := r.reconcileInitializing(context.Background(), node, planner)
	if err != nil {
		t.Fatalf("reconcileInitializing error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after plan failed")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhaseFailed {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhaseFailed)
	}
}

// --- PreInitPlan builder tests ---

func TestBuildPreInitPlan_DoesNotIncludeAwaitCondition(t *testing.T) {
	node := bootstrapReplayerNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	plan := buildPreInitPlan(node, planner)

	for _, task := range plan.Tasks {
		if task.Type == taskAwaitCondition {
			t.Errorf("pre-init plan should not contain await-condition (halt-height is used instead)")
		}
	}
	lastTask := plan.Tasks[len(plan.Tasks)-1]
	if lastTask.Type != taskMarkReady {
		t.Errorf("last task type = %q, want %q", lastTask.Type, taskMarkReady)
	}
}

func TestBuildPreInitPlan_NoBootstrap_ReturnsEmptyPlan(t *testing.T) {
	node := snapshotNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	plan := buildPreInitPlan(node, planner)

	if len(plan.Tasks) != 0 {
		t.Errorf("expected 0 tasks for non-bootstrap node, got %d", len(plan.Tasks))
	}
	if plan.Phase != seiv1alpha1.TaskPlanActive {
		t.Errorf("Phase = %q, want Active", plan.Phase)
	}
}

func TestBuildSharedTask_AwaitCondition(t *testing.T) {
	node := replayerNode()
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	builder, err := planner.BuildTask(node, taskAwaitCondition)
	if err != nil {
		t.Fatalf("BuildTask error: %v", err)
	}

	task, ok := builder.(sidecar.AwaitConditionTask)
	if !ok {
		t.Fatalf("expected AwaitConditionTask, got %T", builder)
	}
	if task.Condition != sidecar.ConditionHeight {
		t.Errorf("Condition = %q, want %q", task.Condition, sidecar.ConditionHeight)
	}
	if task.TargetHeight != 100000000 {
		t.Errorf("TargetHeight = %d, want %d", task.TargetHeight, 100000000)
	}
	if task.Action != sidecar.ActionSIGTERM {
		t.Errorf("Action = %q, want %q", task.Action, sidecar.ActionSIGTERM)
	}
}

// --- Job builder tests ---

func TestGeneratePreInitJob(t *testing.T) {
	node := replayerNode()
	node.Spec.Replayer.Snapshot.BootstrapImage = testBootstrapImageV1
	job := generatePreInitJob(node, DefaultPlatformConfig())

	if job.Name != "test-replayer-pre-init" {
		t.Errorf("Job name = %q, want %q", job.Name, "test-replayer-pre-init")
	}
	if job.Namespace != "default" {
		t.Errorf("Job namespace = %q, want %q", job.Namespace, "default")
	}

	if job.Labels[componentLabel] != "pre-init" {
		t.Errorf("Job label %s = %q, want %q", componentLabel, job.Labels[componentLabel], "pre-init")
	}

	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Errorf("TTLSecondsAfterFinished = %v, want 3600", job.Spec.TTLSecondsAfterFinished)
	}

	spec := job.Spec.Template.Spec
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", spec.RestartPolicy)
	}
	if spec.ShareProcessNamespace == nil || !*spec.ShareProcessNamespace {
		t.Error("expected ShareProcessNamespace to be true")
	}
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != preInitTerminationGracePeriod {
		t.Errorf("TerminationGracePeriodSeconds = %v, want %d", spec.TerminationGracePeriodSeconds, preInitTerminationGracePeriod)
	}

	if spec.Hostname != preInitPodHostname {
		t.Errorf("Hostname = %q, want %q", spec.Hostname, preInitPodHostname)
	}
	if spec.Subdomain != preInitJobName(node) {
		t.Errorf("Subdomain = %q, want %q", spec.Subdomain, preInitJobName(node))
	}

	if len(spec.InitContainers) != 2 {
		t.Fatalf("expected 2 init containers, got %d", len(spec.InitContainers))
	}
	if spec.InitContainers[0].Name != "seid-init" {
		t.Errorf("first init container name = %q, want seid-init", spec.InitContainers[0].Name)
	}
	if spec.InitContainers[0].Image != testBootstrapImageV1 {
		t.Errorf("seid-init image = %q, want sei:bootstrap-v1", spec.InitContainers[0].Image)
	}
	if spec.InitContainers[1].Name != "sei-sidecar" {
		t.Errorf("second init container name = %q, want sei-sidecar", spec.InitContainers[1].Name)
	}

	if len(spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(spec.Containers))
	}
	if spec.Containers[0].Name != "seid" {
		t.Errorf("main container name = %q, want seid", spec.Containers[0].Name)
	}
	if spec.Containers[0].Image != testBootstrapImageV1 {
		t.Errorf("main container image = %q, want sei:bootstrap-v1", spec.Containers[0].Image)
	}

	if len(spec.Volumes) != 1 || spec.Volumes[0].Name != "data" {
		t.Error("expected a single 'data' volume")
	}
}

func TestGeneratePreInitJob_SidecarResources(t *testing.T) {
	node := replayerNode()
	node.Spec.Replayer.Snapshot.BootstrapImage = testBootstrapImageV1
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{
		Resources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}
	job := generatePreInitJob(node, DefaultPlatformConfig())
	spec := job.Spec.Template.Spec

	if len(spec.InitContainers) < 2 {
		t.Fatalf("expected at least 2 init containers, got %d", len(spec.InitContainers))
	}
	sc := spec.InitContainers[1]
	if sc.Name != "sei-sidecar" {
		t.Fatalf("expected sei-sidecar, got %q", sc.Name)
	}
	cpuReq := sc.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "500m" {
		t.Errorf("sidecar CPU request = %q, want %q", cpuReq.String(), "500m")
	}
	memReq := sc.Resources.Requests[corev1.ResourceMemory]
	if memReq.String() != "512Mi" {
		t.Errorf("sidecar memory request = %q, want %q", memReq.String(), "512Mi")
	}
}

func TestGeneratePreInitService(t *testing.T) {
	node := replayerNode()
	svc := generatePreInitService(node)

	if svc.Name != "test-replayer-pre-init" {
		t.Errorf("Service name = %q, want %q", svc.Name, "test-replayer-pre-init")
	}
	if svc.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("ClusterIP = %q, want None", svc.Spec.ClusterIP)
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Error("expected PublishNotReadyAddresses to be true")
	}
	if svc.Spec.Selector[componentLabel] != "pre-init" {
		t.Errorf("selector %s = %q, want %q", componentLabel, svc.Spec.Selector[componentLabel], "pre-init")
	}
}

func TestPreInitSidecarURL(t *testing.T) {
	node := replayerNode()
	got := preInitSidecarURL(node)
	want := "http://seid.test-replayer-pre-init.default.svc.cluster.local:7777"
	if got != want {
		t.Errorf("preInitSidecarURL() = %q, want %q", got, want)
	}
}

// --- executePlan nil guard ---

func TestExecutePlan_NilPlan_ReturnsError(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	r, _ := newProgressionReconciler(t, mock, node)
	planner, _ := PlannerForNode(node, testSnapshotRegion)
	sc := r.buildSidecarClient(node)

	_, err := r.executePlan(context.Background(), node, nil, planner, sc)
	if err == nil {
		t.Fatal("expected error for nil plan")
	}
}

// --- Nil sidecar client handling ---

func TestReconcileInitializing_NilSidecarClient_Requeues(t *testing.T) {
	node := genesisNode()
	node.Status.Phase = seiv1alpha1.PhaseInitializing
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{
			{Type: taskConfigApply, Status: seiv1alpha1.PlannedTaskPending},
		},
	}

	r, _ := newProgressionReconciler(t, nil, node)
	r.BuildSidecarClientFn = func(_ *seiv1alpha1.SeiNode) SidecarStatusClient { return nil }

	planner, _ := PlannerForNode(node, testSnapshotRegion)
	result, err := r.reconcileInitializing(context.Background(), node, planner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != taskPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, taskPollInterval)
	}
}

// --- reconcilePreInitializing flow ---

func bootstrapReplayerNode() *seiv1alpha1.SeiNode {
	n := replayerNode()
	n.Spec.Replayer.Snapshot.BootstrapImage = testBootstrapImage
	return n
}

func TestReconcilePreInitializing_CreatesServiceAndJob(t *testing.T) {
	mock := &mockSidecarClient{}
	node := bootstrapReplayerNode()
	node.Status.Phase = seiv1alpha1.PhasePreInitializing

	planner, _ := PlannerForNode(node, testSnapshotRegion)
	node.Status.PreInitPlan = buildPreInitPlan(node, planner)
	node.Status.InitPlan = planner.BuildPlan(node)

	r, c := newProgressionReconciler(t, mock, node)

	planner, _ = PlannerForNode(node, testSnapshotRegion)
	_, err := r.reconcilePreInitializing(context.Background(), node, planner)
	if err != nil {
		t.Fatalf("error: %v", err)
	}

	svc := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{
		Name: preInitJobName(node), Namespace: node.Namespace,
	}, svc); err != nil {
		t.Fatalf("expected pre-init Service to exist: %v", err)
	}

	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 task submitted, got %d", len(mock.submitted))
	}
}

func TestReconcilePreInitializing_PlanComplete_CleansUpAndTransitions(t *testing.T) {
	mock := &mockSidecarClient{}
	node := bootstrapReplayerNode()
	node.Status.Phase = seiv1alpha1.PhasePreInitializing
	node.Status.PreInitPlan = &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanComplete,
	}
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive}

	r, c := newProgressionReconciler(t, mock, node)

	svc := generatePreInitService(node)
	if err := c.Create(context.Background(), svc); err != nil {
		t.Fatalf("creating service: %v", err)
	}

	planner, _ := PlannerForNode(node, testSnapshotRegion)
	result, err := r.reconcilePreInitializing(context.Background(), node, planner)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after plan complete")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhaseInitializing {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhaseInitializing)
	}
}

func TestCleanupPreInit_WaitsForJobDeletion(t *testing.T) {
	mock := &mockSidecarClient{}
	node := bootstrapReplayerNode()
	node.Status.Phase = seiv1alpha1.PhasePreInitializing
	node.Status.PreInitPlan = &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanComplete,
	}
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive}

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	svc := generatePreInitService(node)
	if err := c.Create(ctx, svc); err != nil {
		t.Fatalf("creating service: %v", err)
	}

	job := generatePreInitJob(node, DefaultPlatformConfig())
	if err := c.Create(ctx, job); err != nil {
		t.Fatalf("creating job: %v", err)
	}

	planner, _ := PlannerForNode(node, testSnapshotRegion)

	// First call finds the Job, issues the delete, and returns an error to
	// requeue (the fake client deletes synchronously but cleanupPreInit
	// always returns an error on the same pass where it issues the delete).
	_, err := r.reconcilePreInitializing(ctx, node, planner)
	if err == nil {
		t.Fatal("expected error while Job is still being deleted")
	}

	// Node should still be in PreInitializing.
	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhasePreInitializing {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhasePreInitializing)
	}

	// Second call: Job is now gone (fake client deleted it synchronously),
	// so cleanupPreInit succeeds and the phase transitions.
	updated = fetchNode(t, c, node.Name, node.Namespace)
	planner, _ = PlannerForNode(updated, testSnapshotRegion)
	result, err := r.reconcilePreInitializing(ctx, updated, planner)
	if err != nil {
		t.Fatalf("unexpected error after Job deleted: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after transition")
	}

	updated = fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhaseInitializing {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhaseInitializing)
	}
}

func TestReconcilePreInitializing_EmptyPlan_TransitionsToInitializing(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	node.Status.Phase = seiv1alpha1.PhasePreInitializing
	node.Status.PreInitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive}
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive}

	r, c := newProgressionReconciler(t, mock, node)

	planner, _ := PlannerForNode(node, testSnapshotRegion)

	// First call marks the empty plan complete.
	result, err := r.reconcilePreInitializing(context.Background(), node, planner)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after marking empty plan complete")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.PreInitPlan.Phase != seiv1alpha1.TaskPlanComplete {
		t.Errorf("PreInitPlan.Phase = %q, want Complete", updated.Status.PreInitPlan.Phase)
	}

	// Second call sees Complete and transitions to Initializing.
	result, err = r.reconcilePreInitializing(context.Background(), updated, planner)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after phase transition")
	}

	updated = fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhaseInitializing {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhaseInitializing)
	}

	if len(mock.submitted) != 0 {
		t.Errorf("expected no tasks submitted for empty plan, got %d", len(mock.submitted))
	}
}

func TestReconcilePreInitializing_PlanFailed_CleansUpAndFails(t *testing.T) {
	mock := &mockSidecarClient{}
	node := bootstrapReplayerNode()
	node.Status.Phase = seiv1alpha1.PhasePreInitializing
	node.Status.PreInitPlan = &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanFailed,
	}
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive}

	r, c := newProgressionReconciler(t, mock, node)

	planner, _ := PlannerForNode(node, testSnapshotRegion)
	result, err := r.reconcilePreInitializing(context.Background(), node, planner)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after plan failed")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhaseFailed {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhaseFailed)
	}
}

func TestReconcilePreInitializing_NilSidecarClient_Requeues(t *testing.T) {
	node := bootstrapReplayerNode()
	node.Status.Phase = seiv1alpha1.PhasePreInitializing

	planner, _ := PlannerForNode(node, testSnapshotRegion)
	node.Status.PreInitPlan = buildPreInitPlan(node, planner)
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive}

	r, _ := newProgressionReconciler(t, nil, node)
	r.BuildSidecarClientFn = func(_ *seiv1alpha1.SeiNode) SidecarStatusClient { return nil }

	planner, _ = PlannerForNode(node, testSnapshotRegion)
	result, err := r.reconcilePreInitializing(context.Background(), node, planner)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != taskPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, taskPollInterval)
	}
}

// --- needsPreInit helper tests ---

func TestNeedsPreInit(t *testing.T) {
	tests := []struct {
		name string
		node *seiv1alpha1.SeiNode
		want bool
	}{
		{
			"replayer with bootstrap image",
			func() *seiv1alpha1.SeiNode {
				n := replayerNode()
				n.Spec.Replayer.Snapshot.BootstrapImage = testBootstrapImage
				return n
			}(),
			true,
		},
		{
			"replayer without bootstrap image",
			replayerNode(),
			false,
		},
		{
			"full node without bootstrap image",
			snapshotNode(),
			false,
		},
		{
			"genesis node",
			genesisNode(),
			false,
		},
		{
			"full node with bootstrap image but no S3",
			func() *seiv1alpha1.SeiNode {
				n := snapshotNode()
				n.Spec.FullNode.Snapshot = &seiv1alpha1.SnapshotSource{
					BootstrapImage: testBootstrapImage,
					StateSync:      &seiv1alpha1.StateSyncSource{},
				}
				return n
			}(),
			false,
		},
		{
			"full node with bootstrap image and S3 but zero target height",
			func() *seiv1alpha1.SeiNode {
				n := snapshotNode()
				n.Spec.FullNode.Snapshot = &seiv1alpha1.SnapshotSource{
					BootstrapImage: testBootstrapImage,
					S3:             &seiv1alpha1.S3SnapshotSource{TargetHeight: 0},
				}
				return n
			}(),
			false,
		},
		{
			"full node with bootstrap image and valid S3",
			func() *seiv1alpha1.SeiNode {
				n := snapshotNode()
				n.Spec.FullNode.Snapshot.BootstrapImage = testBootstrapImage
				return n
			}(),
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsPreInit(tt.node); got != tt.want {
				t.Errorf("needsPreInit() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFullNodePlanner_Validate_BootstrapRequiresS3(t *testing.T) {
	node := snapshotNode()
	node.Spec.FullNode.Snapshot = &seiv1alpha1.SnapshotSource{
		BootstrapImage: testBootstrapImage,
		StateSync:      &seiv1alpha1.StateSyncSource{},
	}
	planner, err := PlannerForNode(node, testSnapshotRegion)
	if err != nil {
		t.Fatal(err)
	}
	if err := planner.Validate(node); err == nil {
		t.Error("expected validation error for bootstrapImage without S3")
	}
}

func TestValidatorPlanner_Validate_BootstrapRequiresS3(t *testing.T) {
	node := genesisNode()
	node.Spec.Validator.Snapshot = &seiv1alpha1.SnapshotSource{
		BootstrapImage: testBootstrapImage,
		StateSync:      &seiv1alpha1.StateSyncSource{},
	}
	planner, err := PlannerForNode(node, testSnapshotRegion)
	if err != nil {
		t.Fatal(err)
	}
	if err := planner.Validate(node); err == nil {
		t.Error("expected validation error for bootstrapImage without S3")
	}
}
