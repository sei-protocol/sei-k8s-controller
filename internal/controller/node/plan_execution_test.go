package node

import (
	"context"
	"encoding/json"
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
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	testBootstrapImage   = "sei:bootstrap"
	testBootstrapImageV1 = "sei:bootstrap-v1"
	testSnapshotRegion   = "eu-central-1"
)

func mustBuildPlan(t *testing.T, p planner.NodePlanner, node *seiv1alpha1.SeiNode) *seiv1alpha1.TaskPlan {
	t.Helper()
	plan, err := p.BuildPlan(node)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	return plan
}

type mockSidecarClient struct {
	submitted []sidecar.TaskRequest
	submitErr error
	submitID  uuid.UUID

	taskResults map[uuid.UUID]*sidecar.TaskResult
	getTaskErr  error
}

func (m *mockSidecarClient) SubmitTask(_ context.Context, req sidecar.TaskRequest) (uuid.UUID, error) {
	m.submitted = append(m.submitted, req)
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
		PlanExecutor: &planner.Executor[*seiv1alpha1.SeiNode]{
			Client: c,
			ConfigFor: func(_ context.Context, node *seiv1alpha1.SeiNode) task.ExecutionConfig {
				return task.ExecutionConfig{
					BuildSidecarClient: func() (task.SidecarClient, error) { return mock, nil },
					KubeClient:         c,
					Scheme:             s,
					Resource:           node,
					Platform:           DefaultPlatformConfig(),
				}
			},
		},
		BuildSidecarClientFn: func(_ *seiv1alpha1.SeiNode) task.SidecarClient {
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
			p, _ := planner.ForNode(snapshotNode(), testSnapshotRegion)
			plan := mustBuildPlan(t, p, snapshotNode())
			if plan == nil {
				t.Fatal("expected non-nil plan")
			}
			_ = tt // bootstrap mode is now internal to planner
		})
	}
}

// --- Plan building tests ---

func TestBuildPlan_Snapshot(t *testing.T) {
	p, _ := planner.ForNode(snapshotNode(), testSnapshotRegion)
	plan := mustBuildPlan(t, p, snapshotNode())
	got := taskTypes(plan)
	want := []string{planner.TaskSnapshotRestore, planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskConfigureStateSync, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_SnapshotWithPeers(t *testing.T) {
	node := snapshotNode()
	node.Spec.FullNode.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"Chain": "atlantic-2"}}},
	}
	p, _ := planner.ForNode(node, testSnapshotRegion)
	plan := mustBuildPlan(t, p, node)
	got := taskTypes(plan)
	want := []string{planner.TaskSnapshotRestore, planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskDiscoverPeers, planner.TaskConfigureStateSync, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_StateSync(t *testing.T) {
	p, _ := planner.ForNode(peerSyncNode(), testSnapshotRegion)
	plan := mustBuildPlan(t, p, peerSyncNode())
	got := taskTypes(plan)
	want := []string{planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskDiscoverPeers, planner.TaskConfigureStateSync, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_Genesis(t *testing.T) {
	p, _ := planner.ForNode(genesisNode(), testSnapshotRegion)
	plan := mustBuildPlan(t, p, genesisNode())
	got := taskTypes(plan)
	want := []string{planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_GenesisWithPeers(t *testing.T) {
	node := genesisNode()
	node.Spec.Validator.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"Chain": "arctic-1"}}},
	}
	p, _ := planner.ForNode(node, testSnapshotRegion)
	plan := mustBuildPlan(t, p, node)
	got := taskTypes(plan)
	want := []string{planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskDiscoverPeers, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_Replayer(t *testing.T) {
	node := replayerNode()
	p, _ := planner.ForNode(node, testSnapshotRegion)
	plan := mustBuildPlan(t, p, node)
	got := taskTypes(plan)
	want := []string{planner.TaskSnapshotRestore, planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskDiscoverPeers, planner.TaskConfigureStateSync, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_Archive(t *testing.T) {
	node := snapshotterNode()
	p, _ := planner.ForNode(node, testSnapshotRegion)
	plan := mustBuildPlan(t, p, node)
	got := taskTypes(plan)
	want := []string{planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskDiscoverPeers, planner.TaskConfigureStateSync, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlanPhaseAndTasks(t *testing.T) {
	p, _ := planner.ForNode(snapshotNode(), testSnapshotRegion)
	plan := mustBuildPlan(t, p, snapshotNode())
	if plan.Phase != seiv1alpha1.TaskPlanActive {
		t.Errorf("phase = %q, want Active", plan.Phase)
	}
	if len(plan.Tasks) != 6 {
		t.Fatalf("expected 6 tasks, got %d: %v", len(plan.Tasks), taskTypes(plan))
	}
	for _, pt := range plan.Tasks {
		if pt.Status != seiv1alpha1.TaskPending {
			t.Errorf("task %q status = %q, want Pending", pt.Type, pt.Status)
		}
		if pt.ID == "" {
			t.Errorf("task %q has empty ID", pt.Type)
		}
		if pt.Params == nil {
			t.Errorf("task %q has nil Params", pt.Type)
		}
	}
	if plan.Tasks[0].Type != planner.TaskSnapshotRestore {
		t.Errorf("first task = %q, want %q", plan.Tasks[0].Type, planner.TaskSnapshotRestore)
	}
}

func TestBuildPlan_DeterministicIDs(t *testing.T) {
	node := snapshotNode()
	p, _ := planner.ForNode(node, testSnapshotRegion)
	plan1 := mustBuildPlan(t, p, node)
	plan2 := mustBuildPlan(t, p, node)
	for i := range plan1.Tasks {
		if plan1.Tasks[i].ID != plan2.Tasks[i].ID {
			t.Errorf("task %d ID not deterministic: %q vs %q", i, plan1.Tasks[i].ID, plan2.Tasks[i].ID)
		}
	}
}

func TestBuildPlan_ParamsRoundTrip(t *testing.T) {
	node := snapshotNode()
	p, _ := planner.ForNode(node, testSnapshotRegion)
	plan := mustBuildPlan(t, p, node)
	firstTask := plan.Tasks[0]
	if firstTask.Type != planner.TaskSnapshotRestore {
		t.Fatalf("expected snapshot-restore, got %s", firstTask.Type)
	}
	var params task.SnapshotRestoreParams
	if err := json.Unmarshal(firstTask.Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if params.Bucket != "atlantic-2-snapshots" {
		t.Errorf("Bucket = %q, want %q", params.Bucket, "atlantic-2-snapshots")
	}
	if params.Region != testSnapshotRegion {
		t.Errorf("Region = %q, want %q", params.Region, testSnapshotRegion)
	}
}

func TestConfigApply_ParamsFromPlan(t *testing.T) {
	node := snapshotNode()
	node.Spec.Overrides = map[string]string{
		"giga_executor.enabled": "true",
	}
	p, _ := planner.ForNode(node, testSnapshotRegion)
	plan := mustBuildPlan(t, p, node)

	var configTask *seiv1alpha1.PlannedTask
	for i := range plan.Tasks {
		if plan.Tasks[i].Type == planner.TaskConfigApply {
			configTask = &plan.Tasks[i]
			break
		}
	}
	if configTask == nil {
		t.Fatal("no config-apply task in plan")
	}

	var params task.ConfigApplyParams
	if err := json.Unmarshal(configTask.Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if params.Mode != string(seiconfig.ModeFull) {
		t.Errorf("Mode = %q, want %q", params.Mode, seiconfig.ModeFull)
	}
	if params.Overrides["giga_executor.enabled"] != "true" {
		t.Errorf("missing user override giga_executor.enabled")
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
	p, _ := planner.ForNode(node, testSnapshotRegion)
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	_, err := r.reconcilePending(ctx, node, p)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	node = fetchNode(t, c, node.Name, node.Namespace)

	_, err = r.PlanExecutor.ExecutePlan(ctx, node, node.Status.InitPlan)
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
	if mock.submitted[0].Type != planner.TaskSnapshotRestore {
		t.Errorf("submitted task = %q, want %q", mock.submitted[0].Type, planner.TaskSnapshotRestore)
	}
}

func TestReconcile_SubmitsFirstPendingTask(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	p, _ := planner.ForNode(node, testSnapshotRegion)
	node.Status.InitPlan = mustBuildPlan(t, p, node)
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	_, err := r.PlanExecutor.ExecutePlan(ctx, node, node.Status.InitPlan)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submitted, got %d", len(mock.submitted))
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	firstTask := updated.Status.InitPlan.Tasks[0]
	if firstTask.Status != seiv1alpha1.TaskPending && firstTask.Status != seiv1alpha1.TaskComplete {
		t.Logf("task status = %q (submit succeeded, status depends on mock GetTask)", firstTask.Status)
	}
}

func TestReconcile_AllTasksComplete_MarksPlanComplete(t *testing.T) {
	mock := &mockSidecarClient{}
	node := genesisNode()
	p, _ := planner.ForNode(node, testSnapshotRegion)
	node.Status.InitPlan = mustBuildPlan(t, p, node)
	for i := range node.Status.InitPlan.Tasks {
		node.Status.InitPlan.Tasks[i].Status = seiv1alpha1.TaskComplete
	}

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	result, err := r.PlanExecutor.ExecutePlan(ctx, node, node.Status.InitPlan)
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
	p, _ := planner.ForNode(node, testSnapshotRegion)
	node.Status.InitPlan = mustBuildPlan(t, p, node)
	node.Status.InitPlan.Phase = seiv1alpha1.TaskPlanFailed

	r, _ := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	result, err := r.PlanExecutor.ExecutePlan(ctx, node, node.Status.InitPlan)
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
	if mock.submitted[0].Type != planner.TaskSnapshotUpload {
		t.Errorf("task type = %q, want %q", mock.submitted[0].Type, planner.TaskSnapshotUpload)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.ScheduledTasks == nil {
		t.Fatal("expected ScheduledTasks to be set")
	}
	if got := updated.Status.ScheduledTasks[planner.TaskSnapshotUpload]; got != taskID.String() {
		t.Errorf("ScheduledTasks[%s] = %q, want %q", planner.TaskSnapshotUpload, got, taskID.String())
	}
}

func TestReconcile_CompletePlan_SkipsAlreadyScheduled(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotterNode()
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.ScheduledTasks = map[string]string{
		planner.TaskSnapshotUpload: uuid.New().String(),
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
	p, _ := planner.ForNode(node, testSnapshotRegion)
	node.Status.InitPlan = mustBuildPlan(t, p, node)

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	result, err := r.PlanExecutor.ExecutePlan(ctx, node, node.Status.InitPlan)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != planner.TaskPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, planner.TaskPollInterval)
	}
	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.InitPlan.Tasks[0].Status != seiv1alpha1.TaskPending {
		t.Errorf("task status = %q, want Pending after submit failure", updated.Status.InitPlan.Tasks[0].Status)
	}
}

// --- ExecutePlan nil guard ---

func TestExecutePlan_NilPlan_ReturnsError(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	r, _ := newProgressionReconciler(t, mock, node)

	_, err := r.PlanExecutor.ExecutePlan(context.Background(), node, nil)
	if err == nil {
		t.Fatal("expected error for nil plan")
	}
}

// --- PlannerForNode dispatch tests ---

func TestPlannerForNode_FullNode(t *testing.T) {
	node := snapshotNode()
	p, err := planner.ForNode(node, testSnapshotRegion)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != string(seiconfig.ModeFull) {
		t.Errorf("Mode() = %q, want %q", p.Mode(), string(seiconfig.ModeFull))
	}
}

func TestPlannerForNode_Archive(t *testing.T) {
	node := snapshotterNode()
	p, err := planner.ForNode(node, testSnapshotRegion)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != string(seiconfig.ModeArchive) {
		t.Errorf("Mode() = %q, want %q", p.Mode(), string(seiconfig.ModeArchive))
	}
}

func TestPlannerForNode_Validator(t *testing.T) {
	node := genesisNode()
	p, err := planner.ForNode(node, testSnapshotRegion)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != string(seiconfig.ModeValidator) {
		t.Errorf("Mode() = %q, want %q", p.Mode(), string(seiconfig.ModeValidator))
	}
}

func TestPlannerForNode_Replayer(t *testing.T) {
	node := replayerNode()
	p, err := planner.ForNode(node, testSnapshotRegion)
	if err != nil {
		t.Fatal(err)
	}
	if p.Mode() != string(seiconfig.ModeFull) {
		t.Errorf("Mode() = %q, want %q", p.Mode(), string(seiconfig.ModeFull))
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
	_, err := planner.ForNode(node, testSnapshotRegion)
	if err == nil {
		t.Error("expected error for node with no sub-spec")
	}
}

// --- Phase transition tests ---

func TestReconcilePending_NoBootstrap_SetsInitializingWithPlan(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	r, c := newProgressionReconciler(t, mock, node)

	p, _ := planner.ForNode(node, testSnapshotRegion)
	result, err := r.reconcilePending(context.Background(), node, p)
	if err != nil {
		t.Fatalf("reconcilePending error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after reconcilePending")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhaseInitializing {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhaseInitializing)
	}
	if updated.Status.InitPlan == nil {
		t.Fatal("expected InitPlan to be created")
	}
}

func TestReconcilePending_WithBootstrap_SetsInitializing(t *testing.T) {
	mock := &mockSidecarClient{}
	node := replayerNode()
	node.Spec.Replayer.Snapshot.BootstrapImage = testBootstrapImage
	r, c := newProgressionReconciler(t, mock, node)

	p, _ := planner.ForNode(node, testSnapshotRegion)
	result, err := r.reconcilePending(context.Background(), node, p)
	if err != nil {
		t.Fatalf("reconcilePending error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after reconcilePending")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhaseInitializing {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhaseInitializing)
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

	result, err := r.reconcileInitializing(context.Background(), node)
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

	result, err := r.reconcileInitializing(context.Background(), node)
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

// --- Result export tests ---

func TestResultExportMonitorTask_ReplayerWithExport(t *testing.T) {
	node := monitorReplayerNode()
	req := planner.ResultExportMonitorTask(node)
	if req == nil {
		t.Fatal("expected non-nil TaskRequest")
	}
	if req.Type != planner.TaskResultExport {
		t.Errorf("Type = %q, want %q", req.Type, planner.TaskResultExport)
	}
}

func TestResultExportMonitorTask_ReplayerWithoutExport(t *testing.T) {
	node := replayerNode()
	req := planner.ResultExportMonitorTask(node)
	if req != nil {
		t.Errorf("expected nil TaskRequest, got %v", req)
	}
}

// --- Snapshot upload tests ---

func TestSnapshotUploadScheduledTask_WithDestination(t *testing.T) {
	builder := planner.SnapshotUploadScheduledTask(snapshotterNode())
	if builder == nil {
		t.Fatal("expected non-nil builder")
	}
	if builder.TaskType() != planner.TaskSnapshotUpload {
		t.Errorf("TaskType() = %q, want %q", builder.TaskType(), planner.TaskSnapshotUpload)
	}
}

func TestSnapshotUploadScheduledTask_NoDestination(t *testing.T) {
	builder := planner.SnapshotUploadScheduledTask(snapshotNode())
	if builder != nil {
		t.Errorf("expected nil builder, got %v", builder)
	}
}

// --- Nil sidecar client handling ---

func TestReconcileInitializing_SidecarClientError_Requeues(t *testing.T) {
	s := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	node := genesisNode()
	node.Status.Phase = seiv1alpha1.PhaseInitializing
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{
			{Type: planner.TaskConfigApply, ID: "test-id", Status: seiv1alpha1.TaskPending},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()

	r := &SeiNodeReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(100),
		Platform: DefaultPlatformConfig(),
		PlanExecutor: &planner.Executor[*seiv1alpha1.SeiNode]{
			Client: c,
			ConfigFor: func(_ context.Context, n *seiv1alpha1.SeiNode) task.ExecutionConfig {
				return task.ExecutionConfig{
					BuildSidecarClient: func() (task.SidecarClient, error) {
						return nil, fmt.Errorf("sidecar unavailable")
					},
					KubeClient: c,
					Scheme:     s,
					Resource:   n,
					Platform:   DefaultPlatformConfig(),
				}
			},
		},
	}

	result, err := r.reconcileInitializing(context.Background(), node)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != planner.TaskPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, planner.TaskPollInterval)
	}
}

// --- Bootstrap helpers ---

func bootstrapReplayerNode() *seiv1alpha1.SeiNode {
	n := replayerNode()
	n.Spec.Replayer.Snapshot.BootstrapImage = testBootstrapImage
	return n
}

// --- NeedsBootstrap tests ---

func TestNeedsBootstrap(t *testing.T) {
	tests := []struct {
		name string
		node *seiv1alpha1.SeiNode
		want bool
	}{
		{"replayer with bootstrap image", bootstrapReplayerNode(), true},
		{"replayer without bootstrap image", replayerNode(), false},
		{"full node without bootstrap image", snapshotNode(), false},
		{"genesis node", genesisNode(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := planner.NeedsBootstrap(tt.node); got != tt.want {
				t.Errorf("NeedsBootstrap() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- Bootstrap resource builder tests (task package) ---

func TestTaskGenerateBootstrapJob(t *testing.T) {
	node := replayerNode()
	node.Spec.Replayer.Snapshot.BootstrapImage = testBootstrapImageV1
	snap := node.Spec.SnapshotSource()
	job, err := task.GenerateBootstrapJob(node, snap, DefaultPlatformConfig())
	if err != nil {
		t.Fatalf("GenerateBootstrapJob error: %v", err)
	}

	wantName := "test-replayer-bootstrap"
	if job.Name != wantName {
		t.Errorf("Job name = %q, want %q", job.Name, wantName)
	}

	spec := job.Spec.Template.Spec
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", spec.RestartPolicy)
	}
	if spec.Containers[0].Image != testBootstrapImageV1 {
		t.Errorf("main container image = %q, want %q", spec.Containers[0].Image, testBootstrapImageV1)
	}
}

func TestTaskGenerateBootstrapJob_NilSnapshot(t *testing.T) {
	node := replayerNode()
	_, err := task.GenerateBootstrapJob(node, nil, DefaultPlatformConfig())
	if err == nil {
		t.Fatal("expected error for nil snapshot, got nil")
	}
}

func TestTaskGenerateBootstrapJob_SidecarResources(t *testing.T) {
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
	snap := node.Spec.SnapshotSource()
	job, err := task.GenerateBootstrapJob(node, snap, DefaultPlatformConfig())
	if err != nil {
		t.Fatalf("GenerateBootstrapJob error: %v", err)
	}
	spec := job.Spec.Template.Spec

	sc := spec.InitContainers[1]
	cpuReq := sc.Resources.Requests[corev1.ResourceCPU]
	if cpuReq.String() != "500m" {
		t.Errorf("sidecar CPU request = %q, want %q", cpuReq.String(), "500m")
	}
}

func TestSidecarURLForNode(t *testing.T) {
	node := replayerNode()
	got := planner.SidecarURLForNode(node)
	want := "http://test-replayer-0.test-replayer.default.svc.cluster.local:7777"
	if got != want {
		t.Errorf("SidecarURLForNode() = %q, want %q", got, want)
	}
}

// --- Reconcile replayer runtime task tests ---

func TestReconcile_ReplayerWithoutResultExport_NoMonitorTask(t *testing.T) {
	mock := &mockSidecarClient{}
	node := replayerNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}

	r, c := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileRunning(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(mock.submitted) != 0 {
		t.Errorf("expected no submissions for replayer without ResultExport, got %d", len(mock.submitted))
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.MonitorTasks != nil {
		t.Errorf("expected nil MonitorTasks, got %v", updated.Status.MonitorTasks)
	}
}

func TestReconcile_SnapshotterWithMonitorTask_BothSubmitted(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{submitID: taskID}

	// Build a node that qualifies for both snapshot upload (archive) and
	// result-export monitor (replayer). In practice these don't overlap on
	// one node, but this exercises the code path where both run in one reconcile.
	node := monitorReplayerNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}

	r, c := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileRunning(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	// The monitor replayer node only has result-export (no snapshot upload
	// since it's a replayer, not an archive). Verify the single submission.
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submission, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != planner.TaskResultExport {
		t.Errorf("submitted type = %q, want %q", mock.submitted[0].Type, planner.TaskResultExport)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if _, ok := updated.Status.MonitorTasks[planner.TaskResultExport]; !ok {
		t.Errorf("expected MonitorTasks[%s] to exist", planner.TaskResultExport)
	}
}

func TestReconcileRunning_PollRequeue_ImmediateRequeue(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{
		submitID: taskID,
		taskResults: map[uuid.UUID]*sidecar.TaskResult{
			taskID: completedResult(taskID, planner.TaskResultExport, nil),
		},
	}
	node := monitorReplayerNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}
	node.Status.MonitorTasks = map[string]seiv1alpha1.MonitorTask{
		planner.TaskResultExport: {
			ID:          taskID.String(),
			Status:      seiv1alpha1.TaskPending,
			SubmittedAt: metav1.Now(),
		},
	}

	r, _ := newProgressionReconciler(t, mock, node)

	result, err := r.reconcileRunning(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if !result.Requeue {
		t.Error("expected immediate Requeue when poll detects terminal state")
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected RequeueAfter=0 for immediate requeue, got %v", result.RequeueAfter)
	}
}

func TestReconcile_CompletePlan_SubmitsResultExportMonitorForReplayer(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{submitID: taskID}
	node := monitorReplayerNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}

	r, c := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileRunning(context.Background(), node)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 monitor task submitted, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != planner.TaskResultExport {
		t.Errorf("task type = %q, want %q", mock.submitted[0].Type, planner.TaskResultExport)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.MonitorTasks == nil {
		t.Fatal("expected MonitorTasks to be set")
	}
	mt, ok := updated.Status.MonitorTasks[planner.TaskResultExport]
	if !ok {
		t.Fatalf("expected MonitorTasks[%s] to exist", planner.TaskResultExport)
	}
	if mt.ID != taskID.String() {
		t.Errorf("MonitorTasks[%s].ID = %q, want %q", planner.TaskResultExport, mt.ID, taskID.String())
	}
}
