package planner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"maps"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// mockSidecarClient returns ErrNotFound for GetTask until a task has been
// submitted. After SubmitTask, the postSubmitResults map is merged into
// the active results, simulating the sidecar processing the task.
type mockSidecarClient struct {
	submitted []sidecar.TaskRequest
	submitErr error
	submitID  uuid.UUID

	postSubmitResults map[uuid.UUID]*sidecar.TaskResult
	activeResults     map[uuid.UUID]*sidecar.TaskResult
	getTaskErr        error
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
	if m.activeResults == nil {
		m.activeResults = make(map[uuid.UUID]*sidecar.TaskResult)
	}
	maps.Copy(m.activeResults, m.postSubmitResults)
	return id, nil
}

func (m *mockSidecarClient) GetTask(_ context.Context, id uuid.UUID) (*sidecar.TaskResult, error) {
	if m.getTaskErr != nil {
		return nil, m.getTaskErr
	}
	if m.activeResults != nil {
		if r, ok := m.activeResults[id]; ok {
			return r, nil
		}
	}
	return nil, sidecar.ErrNotFound
}

func (m *mockSidecarClient) Healthz(_ context.Context) (bool, error) {
	return true, nil
}

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
		ObjectMeta: metav1.ObjectMeta{Name: "retry-node", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-chain",
			Image:   "sei:latest",
			Sidecar: &seiv1alpha1.SidecarConfig{Image: "sidecar:latest", Port: 7777},
		},
	}
}

func configGenesisParams(t *testing.T) *apiextensionsv1.JSON {
	t.Helper()
	raw, err := json.Marshal(task.ConfigureGenesisParams{})
	if err != nil {
		t.Fatal(err)
	}
	return &apiextensionsv1.JSON{Raw: raw}
}

func nodeExecutor(c *fake.ClientBuilder, s *k8sruntime.Scheme, mock *mockSidecarClient) *Executor[*seiv1alpha1.SeiNode] {
	fc := c.Build()
	return &Executor[*seiv1alpha1.SeiNode]{
		ConfigFor: func(_ context.Context, node *seiv1alpha1.SeiNode) task.ExecutionConfig {
			return task.ExecutionConfig{
				BuildSidecarClient: func() (task.SidecarClient, error) { return mock, nil },
				KubeClient:         fc,
				Scheme:             s,
				Resource:           node,
			}
		},
	}
}

func TestExecutePlan_RetryOnFailure(t *testing.T) {
	s := testScheme(t)
	node := testNode()

	const planID = "test-plan-retry"
	taskID := task.DeterministicTaskID(planID, sidecar.TaskTypeConfigureGenesis, 0)
	parsedID, _ := uuid.Parse(taskID)

	failedResult := &sidecar.TaskResult{
		Id:          parsedID,
		Type:        sidecar.TaskTypeConfigureGenesis,
		Status:      sidecar.Failed,
		SubmittedAt: time.Now().Add(-5 * time.Second),
		Error:       strPtr("genesis.json not found in S3"),
	}

	mock := &mockSidecarClient{
		postSubmitResults: map[uuid.UUID]*sidecar.TaskResult{parsedID: failedResult},
	}

	node.Status.Plan = &seiv1alpha1.TaskPlan{
		ID:    planID,
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{
			{
				Type:       sidecar.TaskTypeConfigureGenesis,
				ID:         taskID,
				Status:     seiv1alpha1.TaskPending,
				Params:     configGenesisParams(t),
				MaxRetries: 5,
			},
		},
	}

	builder := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node).
		WithStatusSubresource(&seiv1alpha1.SeiNode{})
	executor := nodeExecutor(builder, s, mock)

	ctx := context.Background()
	result, err := executor.ExecutePlan(ctx, node, node.Status.Plan)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}

	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submission, got %d", len(mock.submitted))
	}

	// Executor mutates in-memory — assert directly on the node.
	tsk := &node.Status.Plan.Tasks[0]
	if tsk.Status != seiv1alpha1.TaskPending {
		t.Errorf("task status = %q, want Pending (reset for retry)", tsk.Status)
	}
	if tsk.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", tsk.RetryCount)
	}
	if tsk.ID != taskID {
		t.Errorf("task ID changed after retry: got %q, want %q", tsk.ID, taskID)
	}
	if node.Status.Plan.Phase == seiv1alpha1.TaskPlanFailed {
		t.Error("plan should NOT be failed — retries remain")
	}
	if result.RequeueAfter == 0 {
		t.Error("expected non-zero requeue for retry backoff")
	}
}

func TestExecutePlan_ExhaustedRetries_FailsPlan(t *testing.T) {
	s := testScheme(t)
	node := testNode()

	const planID = "test-plan-exhausted"
	taskID := task.DeterministicTaskID(planID, sidecar.TaskTypeConfigureGenesis, 0)
	parsedID, _ := uuid.Parse(taskID)

	failedResult := &sidecar.TaskResult{
		Id:          parsedID,
		Type:        sidecar.TaskTypeConfigureGenesis,
		Status:      sidecar.Failed,
		SubmittedAt: time.Now().Add(-5 * time.Second),
		Error:       strPtr("genesis.json not found"),
	}

	mock := &mockSidecarClient{
		postSubmitResults: map[uuid.UUID]*sidecar.TaskResult{parsedID: failedResult},
	}

	node.Status.Plan = &seiv1alpha1.TaskPlan{
		ID:    planID,
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{
			{
				Type:       sidecar.TaskTypeConfigureGenesis,
				ID:         taskID,
				Status:     seiv1alpha1.TaskPending,
				Params:     configGenesisParams(t),
				MaxRetries: 2,
				RetryCount: 2,
			},
		},
	}

	builder := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node).
		WithStatusSubresource(&seiv1alpha1.SeiNode{})
	executor := nodeExecutor(builder, s, mock)

	ctx := context.Background()
	_, err := executor.ExecutePlan(ctx, node, node.Status.Plan)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}

	// Executor mutates in-memory — assert directly on the node.
	if node.Status.Plan.Phase != seiv1alpha1.TaskPlanFailed {
		t.Errorf("plan phase = %q, want Failed", node.Status.Plan.Phase)
	}
	if node.Status.Plan.Tasks[0].Status != seiv1alpha1.TaskFailed {
		t.Errorf("task status = %q, want Failed", node.Status.Plan.Tasks[0].Status)
	}
	if node.Status.Plan.FailedTaskIndex == nil {
		t.Fatal("FailedTaskIndex should not be nil")
	}
	if *node.Status.Plan.FailedTaskIndex != 0 {
		t.Errorf("FailedTaskIndex = %d, want 0", *node.Status.Plan.FailedTaskIndex)
	}
	if node.Status.Plan.FailedTaskDetail == nil {
		t.Fatal("FailedTaskDetail should not be nil")
	}
	if node.Status.Plan.FailedTaskDetail.Type != sidecar.TaskTypeConfigureGenesis {
		t.Errorf("FailedTaskDetail.Type = %q, want %q", node.Status.Plan.FailedTaskDetail.Type, sidecar.TaskTypeConfigureGenesis)
	}
	if node.Status.Plan.FailedTaskDetail.RetryCount != 2 {
		t.Errorf("FailedTaskDetail.RetryCount = %d, want 2", node.Status.Plan.FailedTaskDetail.RetryCount)
	}
	if node.Status.Plan.FailedTaskDetail.MaxRetries != 2 {
		t.Errorf("FailedTaskDetail.MaxRetries = %d, want 2", node.Status.Plan.FailedTaskDetail.MaxRetries)
	}
}

func TestExecuteGroupPlan_CompletesSuccessfully(t *testing.T) {
	s := testScheme(t)
	group := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-group", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Replicas: 3,
		},
	}

	const planID = "test-group-plan"
	taskID := task.DeterministicTaskID(planID, sidecar.TaskTypeAssembleGenesis, 0)
	parsedID, _ := uuid.Parse(taskID)

	now := time.Now()
	completedResult := &sidecar.TaskResult{
		Id:          parsedID,
		Type:        sidecar.TaskTypeAssembleGenesis,
		Status:      sidecar.Completed,
		SubmittedAt: now.Add(-10 * time.Second),
		CompletedAt: &now,
	}

	mock := &mockSidecarClient{
		submitID:          parsedID,
		postSubmitResults: map[uuid.UUID]*sidecar.TaskResult{parsedID: completedResult},
	}

	assembleParams, _ := json.Marshal(task.AssembleAndUploadGenesisParams{
		Nodes: []task.GenesisNodeParam{{Name: "node-0"}, {Name: "node-1"}, {Name: "node-2"}},
	})

	group.Status.Plan = &seiv1alpha1.TaskPlan{
		ID:    planID,
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{
			{
				Type:   sidecar.TaskTypeAssembleGenesis,
				ID:     taskID,
				Status: seiv1alpha1.TaskPending,
				Params: &apiextensionsv1.JSON{Raw: assembleParams},
			},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(group).
		WithStatusSubresource(&seiv1alpha1.SeiNodeDeployment{}).
		Build()

	executor := &Executor[*seiv1alpha1.SeiNodeDeployment]{
		ConfigFor: func(_ context.Context, g *seiv1alpha1.SeiNodeDeployment) task.ExecutionConfig {
			return task.ExecutionConfig{
				BuildSidecarClient: func() (task.SidecarClient, error) { return mock, nil },
				KubeClient:         fc,
				Scheme:             s,
				Resource:           g,
			}
		},
	}

	ctx := context.Background()
	result, err := executor.ExecutePlan(ctx, group, group.Status.Plan)
	if err != nil {
		t.Fatalf("ExecutePlan: %v", err)
	}

	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submission, got %d", len(mock.submitted))
	}

	// Executor mutates in-memory — assert directly on the group.
	if group.Status.Plan.Tasks[0].Status != seiv1alpha1.TaskComplete {
		t.Errorf("task status = %q, want Complete", group.Status.Plan.Tasks[0].Status)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected non-zero RequeueAfter after task completion")
	}
}

func TestRetryBackoff(t *testing.T) {
	for i := range 6 {
		d := retryBackoff(i)
		if d > maxRetryBackoff {
			t.Errorf("attempt %d: backoff %v exceeds max %v", i, d, maxRetryBackoff)
		}
		if d <= 0 {
			t.Errorf("attempt %d: backoff should be positive", i)
		}
	}
}

func strPtr(s string) *string { return &s }
