package node

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	seiconfig "github.com/sei-protocol/sei-config"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	testBootstrapImage   = "sei:bootstrap"
	testBootstrapImageV1 = "sei:bootstrap-v1"
	testSnapshotRegion   = "eu-central-1"
)

func mustBuildPlan(t *testing.T, node *seiv1alpha1.SeiNode) *seiv1alpha1.TaskPlan {
	t.Helper()
	if err := planner.ResolvePlan(node); err != nil {
		t.Fatalf("ResolvePlan: %v", err)
	}
	return node.Status.Plan
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
		Platform: platformtest.Config(),
		PlanExecutor: &planner.Executor[*seiv1alpha1.SeiNode]{
			Client: c,
			ConfigFor: func(_ context.Context, node *seiv1alpha1.SeiNode) task.ExecutionConfig {
				return task.ExecutionConfig{
					BuildSidecarClient: func() (task.SidecarClient, error) { return mock, nil },
					KubeClient:         c,
					Scheme:             s,
					Resource:           node,
					Platform:           platformtest.Config(),
				}
			},
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
			Peers: []seiv1alpha1.PeerSource{
				{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"ChainIdentifier": "atlantic-2"}}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{
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
			ChainID:   "arctic-1",
			Image:     "sei:latest",
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
			Peers: []seiv1alpha1.PeerSource{{
				EC2Tags: &seiv1alpha1.EC2TagsPeerSource{
					Region: "eu-central-1",
					Tags:   map[string]string{"ChainIdentifier": "atlantic-2"},
				},
			}},
			Archive: &seiv1alpha1.ArchiveSpec{
				SnapshotGeneration: &seiv1alpha1.SnapshotGenerationConfig{
					KeepRecent: 5,
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
			Peers: []seiv1alpha1.PeerSource{
				{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"ChainIdentifier": "pacific-1"}}},
			},
			Replayer: &seiv1alpha1.ReplayerSpec{
				Snapshot: seiv1alpha1.SnapshotSource{
					S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 100000000},
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

// --- Plan building tests ---

func TestBuildPlan_Snapshot(t *testing.T) {
	plan := mustBuildPlan(t, snapshotNode())
	got := taskTypes(plan)
	want := []string{task.TaskTypeEnsureDataPVC, task.TaskTypeApplyStatefulSet, task.TaskTypeApplyService, planner.TaskSnapshotRestore, planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskConfigureStateSync, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_SnapshotWithPeers(t *testing.T) {
	node := snapshotNode()
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"Chain": "atlantic-2"}}},
	}
	plan := mustBuildPlan(t, node)
	got := taskTypes(plan)
	want := []string{task.TaskTypeEnsureDataPVC, task.TaskTypeApplyStatefulSet, task.TaskTypeApplyService, planner.TaskSnapshotRestore, planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskDiscoverPeers, planner.TaskConfigureStateSync, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_StateSync(t *testing.T) {
	plan := mustBuildPlan(t, peerSyncNode())
	got := taskTypes(plan)
	want := []string{task.TaskTypeEnsureDataPVC, task.TaskTypeApplyStatefulSet, task.TaskTypeApplyService, planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskDiscoverPeers, planner.TaskConfigureStateSync, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_Genesis(t *testing.T) {
	plan := mustBuildPlan(t, genesisNode())
	got := taskTypes(plan)
	want := []string{task.TaskTypeEnsureDataPVC, task.TaskTypeApplyStatefulSet, task.TaskTypeApplyService, planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_GenesisWithPeers(t *testing.T) {
	node := genesisNode()
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"Chain": "arctic-1"}}},
	}
	plan := mustBuildPlan(t, node)
	got := taskTypes(plan)
	want := []string{task.TaskTypeEnsureDataPVC, task.TaskTypeApplyStatefulSet, task.TaskTypeApplyService, planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskDiscoverPeers, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_Replayer(t *testing.T) {
	node := replayerNode()
	plan := mustBuildPlan(t, node)
	got := taskTypes(plan)
	want := []string{task.TaskTypeEnsureDataPVC, task.TaskTypeApplyStatefulSet, task.TaskTypeApplyService, planner.TaskSnapshotRestore, planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskDiscoverPeers, planner.TaskConfigureStateSync, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlan_Archive(t *testing.T) {
	node := snapshotterNode()
	plan := mustBuildPlan(t, node)
	got := taskTypes(plan)
	want := []string{task.TaskTypeEnsureDataPVC, task.TaskTypeApplyStatefulSet, task.TaskTypeApplyService, planner.TaskConfigureGenesis, planner.TaskConfigApply, planner.TaskDiscoverPeers, planner.TaskConfigValidate, planner.TaskMarkReady}
	assertProgression(t, got, want)
}

func TestBuildPlanPhaseAndTasks(t *testing.T) {
	plan := mustBuildPlan(t, snapshotNode())
	if plan.Phase != seiv1alpha1.TaskPlanActive {
		t.Errorf("phase = %q, want Active", plan.Phase)
	}
	if len(plan.Tasks) != 9 {
		t.Fatalf("expected 9 tasks, got %d: %v", len(plan.Tasks), taskTypes(plan))
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
	if plan.Tasks[0].Type != task.TaskTypeEnsureDataPVC {
		t.Errorf("first task = %q, want %q", plan.Tasks[0].Type, task.TaskTypeEnsureDataPVC)
	}
}

func TestBuildPlan_UniqueIDsAcrossRebuilds(t *testing.T) {
	node := snapshotNode()
	plan1 := mustBuildPlan(t, node)
	// Clear the plan so ResolvePlan builds a fresh one.
	node.Status.Plan = nil
	plan2 := mustBuildPlan(t, node)
	if plan1.ID == plan2.ID {
		t.Errorf("plan IDs should differ across rebuilds: both %q", plan1.ID)
	}
	for i := range plan1.Tasks {
		if plan1.Tasks[i].ID == plan2.Tasks[i].ID {
			t.Errorf("task %d ID should differ across rebuilds: both %q", i, plan1.Tasks[i].ID)
		}
	}
	// Verify task IDs are unique within a single plan.
	seen := map[string]bool{}
	for _, tsk := range plan1.Tasks {
		if seen[tsk.ID] {
			t.Errorf("duplicate task ID within plan: %q", tsk.ID)
		}
		seen[tsk.ID] = true
	}
}

func TestBuildPlan_ParamsRoundTrip(t *testing.T) {
	node := snapshotNode()
	plan := mustBuildPlan(t, node)

	// Find snapshot-restore task (now after infrastructure tasks).
	var snapshotTask *seiv1alpha1.PlannedTask
	for i := range plan.Tasks {
		if plan.Tasks[i].Type == planner.TaskSnapshotRestore {
			snapshotTask = &plan.Tasks[i]
			break
		}
	}
	if snapshotTask == nil {
		t.Fatal("expected snapshot-restore task in plan")
	}
	var params task.SnapshotRestoreParams
	if err := json.Unmarshal(snapshotTask.Params.Raw, &params); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if params.TargetHeight != 100000000 {
		t.Errorf("TargetHeight = %d, want 100000000", params.TargetHeight)
	}
}

func TestConfigApply_ParamsFromPlan(t *testing.T) {
	g := NewWithT(t)
	node := snapshotNode()
	node.Spec.Overrides = map[string]string{
		"giga_executor.enabled": "true",
	}
	plan := mustBuildPlan(t, node)

	var configTask *seiv1alpha1.PlannedTask
	for i := range plan.Tasks {
		if plan.Tasks[i].Type == planner.TaskConfigApply {
			configTask = &plan.Tasks[i]
			break
		}
	}
	g.Expect(configTask).NotTo(BeNil(), "no config-apply task in plan")

	var params task.ConfigApplyParams
	g.Expect(json.Unmarshal(configTask.Params.Raw, &params)).To(Succeed())
	g.Expect(params.Mode).To(Equal(string(seiconfig.ModeFull)))
	g.Expect(params.Overrides["giga_executor.enabled"]).To(Equal("true"))
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
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: node.Name, Namespace: node.Namespace}}

	// First Reconcile: ResolvePlan builds the plan, transitions to Initializing,
	// and executes the first task (ensure-data-pvc, which is fire-and-forget).
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Plan == nil {
		t.Fatal("expected plan to be created")
	}
	if updated.Status.Plan.Phase != seiv1alpha1.TaskPlanActive {
		t.Errorf("phase = %q, want Active", updated.Status.Plan.Phase)
	}
	if updated.Status.Phase != seiv1alpha1.PhaseInitializing {
		t.Errorf("node phase = %q, want Initializing", updated.Status.Phase)
	}
}

func TestReconcile_SubmitsFirstPendingTask(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	mustBuildPlan(t, node)
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	// First task is ensure-data-pvc (controller-side, no sidecar submission).
	_, err := r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	firstTask := updated.Status.Plan.Tasks[0]
	if firstTask.Type != task.TaskTypeEnsureDataPVC {
		t.Errorf("first task = %q, want %q", firstTask.Type, task.TaskTypeEnsureDataPVC)
	}
	if firstTask.Status != seiv1alpha1.TaskComplete {
		t.Errorf("first task status = %q, want Complete", firstTask.Status)
	}
}

func TestReconcile_AllTasksComplete_MarksPlanComplete(t *testing.T) {
	mock := &mockSidecarClient{}
	node := genesisNode()
	mustBuildPlan(t, node)
	for i := range node.Status.Plan.Tasks {
		node.Status.Plan.Tasks[i].Status = seiv1alpha1.TaskComplete
	}

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	result, err := r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when marking plan complete")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Plan.Phase != seiv1alpha1.TaskPlanComplete {
		t.Errorf("plan phase = %q, want Complete", updated.Status.Plan.Phase)
	}
}

func TestReconcile_FailedPlan_NoOps(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	mustBuildPlan(t, node)
	node.Status.Plan.Phase = seiv1alpha1.TaskPlanFailed

	r, _ := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	result, err := r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
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

func TestReconcile_SubmitError_RequeuesGracefully(t *testing.T) {
	mock := &mockSidecarClient{submitErr: fmt.Errorf("connection refused")}
	node := snapshotNode()
	mustBuildPlan(t, node)

	// Advance past the infrastructure tasks (they are controller-side, complete synchronously).
	for i := range node.Status.Plan.Tasks {
		if node.Status.Plan.Tasks[i].Type == task.TaskTypeEnsureDataPVC ||
			node.Status.Plan.Tasks[i].Type == task.TaskTypeApplyStatefulSet ||
			node.Status.Plan.Tasks[i].Type == task.TaskTypeApplyService {
			node.Status.Plan.Tasks[i].Status = seiv1alpha1.TaskComplete
		}
	}

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	// The first sidecar task (snapshot-restore) will fail to submit.
	result, err := r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if result.RequeueAfter != planner.TaskPollInterval {
		t.Errorf("RequeueAfter = %v, want %v", result.RequeueAfter, planner.TaskPollInterval)
	}
	updated := fetchNode(t, c, node.Name, node.Namespace)
	snapshotTask := findPlannedTask(updated.Status.Plan, planner.TaskSnapshotRestore)
	if snapshotTask == nil {
		t.Fatal("expected snapshot-restore task in plan")
	}
	if snapshotTask.Status != seiv1alpha1.TaskPending {
		t.Errorf("snapshot task status = %q, want Pending after submit failure", snapshotTask.Status)
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

// --- ResolvePlan dispatch tests ---

func TestResolvePlan_FullNode(t *testing.T) {
	node := snapshotNode()
	if err := planner.ResolvePlan(node); err != nil {
		t.Fatal(err)
	}
	if node.Status.Plan == nil {
		t.Fatal("expected non-nil plan")
	}
}

func TestResolvePlan_Archive(t *testing.T) {
	node := snapshotterNode()
	if err := planner.ResolvePlan(node); err != nil {
		t.Fatal(err)
	}
	if node.Status.Plan == nil {
		t.Fatal("expected non-nil plan")
	}
}

func TestResolvePlan_Validator(t *testing.T) {
	node := genesisNode()
	if err := planner.ResolvePlan(node); err != nil {
		t.Fatal(err)
	}
	if node.Status.Plan == nil {
		t.Fatal("expected non-nil plan")
	}
}

func TestResolvePlan_Replayer(t *testing.T) {
	node := replayerNode()
	if err := planner.ResolvePlan(node); err != nil {
		t.Fatal(err)
	}
	if node.Status.Plan == nil {
		t.Fatal("expected non-nil plan")
	}
}

func TestResolvePlan_NoSubSpec(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test",
			Image:   "sei:latest",
		},
	}
	err := planner.ResolvePlan(node)
	if err == nil {
		t.Error("expected error for node with no sub-spec")
	}
}

func TestResolvePlan_ResumesActivePlan(t *testing.T) {
	node := snapshotNode()
	node.Status.Plan = &seiv1alpha1.TaskPlan{
		ID:    "existing-plan",
		Phase: seiv1alpha1.TaskPlanActive,
	}
	if err := planner.ResolvePlan(node); err != nil {
		t.Fatal(err)
	}
	if node.Status.Plan.ID != "existing-plan" {
		t.Errorf("expected plan to be resumed, got new plan %q", node.Status.Plan.ID)
	}
}

// --- Phase transition tests ---

func TestReconcile_Pending_SetsInitializingWithPlan(t *testing.T) {
	mock := &mockSidecarClient{}
	node := snapshotNode()
	r, c := newProgressionReconciler(t, mock, node)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: node.Name, Namespace: node.Namespace}}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhaseInitializing {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhaseInitializing)
	}
	if updated.Status.Plan == nil {
		t.Fatal("expected plan to be created")
	}
}

func TestReconcile_Pending_WithBootstrap_SetsInitializing(t *testing.T) {
	mock := &mockSidecarClient{}
	node := replayerNode()
	node.Spec.Replayer.Snapshot.BootstrapImage = testBootstrapImage
	r, c := newProgressionReconciler(t, mock, node)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: node.Name, Namespace: node.Namespace}}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.Phase != seiv1alpha1.PhaseInitializing {
		t.Errorf("Phase = %q, want %q", updated.Status.Phase, seiv1alpha1.PhaseInitializing)
	}
	if updated.Status.Plan == nil {
		t.Fatal("expected plan to be created")
	}
}

func TestExecutePlan_AllComplete_TransitionsToTargetPhase(t *testing.T) {
	g := NewWithT(t)
	mock := &mockSidecarClient{}
	node := genesisNode()
	node.Status.Phase = seiv1alpha1.PhaseInitializing
	mustBuildPlan(t, node)
	// Pre-complete all tasks so ExecutePlan triggers plan completion.
	for i := range node.Status.Plan.Tasks {
		node.Status.Plan.Tasks[i].Status = seiv1alpha1.TaskComplete
	}

	r, c := newProgressionReconciler(t, mock, node)
	_, err := r.PlanExecutor.ExecutePlan(context.Background(), node, node.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())

	updated := fetchNode(t, c, node.Name, node.Namespace)
	g.Expect(updated.Status.Phase).To(Equal(seiv1alpha1.PhaseRunning), "executor should transition to TargetPhase")
	g.Expect(updated.Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
}

func TestExecutePlan_ConvergencePlan_NilsOnCompletion(t *testing.T) {
	g := NewWithT(t)
	mock := &mockSidecarClient{}
	node := snapshotNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.Plan = nil
	// Build a convergence plan for a Running node.
	if err := planner.ResolvePlan(node); err != nil {
		t.Fatal(err)
	}
	// Pre-complete all tasks.
	for i := range node.Status.Plan.Tasks {
		node.Status.Plan.Tasks[i].Status = seiv1alpha1.TaskComplete
	}

	r, c := newProgressionReconciler(t, mock, node)
	_, err := r.PlanExecutor.ExecutePlan(context.Background(), node, node.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())

	updated := fetchNode(t, c, node.Name, node.Namespace)
	g.Expect(updated.Status.Plan).To(BeNil(), "convergence plan should be nilled after completion")
	g.Expect(updated.Status.Phase).To(Equal(seiv1alpha1.PhaseRunning), "phase should stay Running")
}

func TestExecutePlan_TaskFailure_SetsPlanFailedCondition(t *testing.T) {
	g := NewWithT(t)
	mock := &mockSidecarClient{}
	node := snapshotNode()
	mustBuildPlan(t, node)
	// Advance past infrastructure tasks.
	for i := range node.Status.Plan.Tasks {
		if node.Status.Plan.Tasks[i].Type == task.TaskTypeEnsureDataPVC ||
			node.Status.Plan.Tasks[i].Type == task.TaskTypeApplyStatefulSet ||
			node.Status.Plan.Tasks[i].Type == task.TaskTypeApplyService {
			node.Status.Plan.Tasks[i].Status = seiv1alpha1.TaskComplete
		}
	}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	// Submit snapshot-restore.
	_, err := r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())

	// Fail the task via mock.
	ct := planner.CurrentTask(node.Status.Plan)
	g.Expect(ct).NotTo(BeNil())
	taskUUID, parseErr := uuid.Parse(ct.ID)
	g.Expect(parseErr).NotTo(HaveOccurred())
	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskUUID: completedResult(taskUUID, planner.TaskSnapshotRestore, strPtr("boom")),
	}
	node = fetchNode(t, c, node.Name, node.Namespace)
	_, err = r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())

	updated := fetchNode(t, c, node.Name, node.Namespace)
	g.Expect(updated.Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))

	// Verify PlanFailed condition was set.
	var found bool
	for _, cond := range updated.Status.Conditions {
		if cond.Type == planner.ConditionPlanFailed {
			g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(cond.Reason).To(Equal("TaskFailed"))
			g.Expect(cond.Message).To(ContainSubstring("boom"))
			found = true
		}
	}
	g.Expect(found).To(BeTrue(), "expected PlanFailed condition on node")
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
	node.Status.Plan = &seiv1alpha1.TaskPlan{
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
		Platform: platformtest.Config(),
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
					Platform:   platformtest.Config(),
				}
			},
		},
	}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: node.Name, Namespace: node.Namespace}})
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
	job, err := task.GenerateBootstrapJob(node, snap, platformtest.Config())
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
	_, err := task.GenerateBootstrapJob(node, nil, platformtest.Config())
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
	job, err := task.GenerateBootstrapJob(node, snap, platformtest.Config())
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
