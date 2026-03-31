package node

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

// monitorReplayerNode returns a replayer node with canonicalRpc set,
// placing result-export in monitor (comparison) mode.
func monitorReplayerNode() *seiv1alpha1.SeiNode {
	node := replayerNode()
	node.Spec.Replayer.ResultExport = &seiv1alpha1.ResultExportConfig{
		CanonicalRPC: "http://canonical-rpc:26657",
	}
	return node
}

// --- ensureMonitorTask tests ---

func TestEnsureMonitorTask_SubmitsOnce(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{submitID: taskID}
	node := monitorReplayerNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	req := planner.ResultExportMonitorTask(node)
	if req == nil {
		t.Fatal("expected non-nil monitor task request")
	}

	if err := r.ensureMonitorTask(ctx, node, mock, *req); err != nil {
		t.Fatalf("ensureMonitorTask: %v", err)
	}

	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submission, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != planner.TaskResultExport {
		t.Errorf("submitted type = %q, want %q", mock.submitted[0].Type, planner.TaskResultExport)
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
		t.Errorf("MonitorTasks ID = %q, want %q", mt.ID, taskID.String())
	}
	if mt.Status != seiv1alpha1.TaskPending {
		t.Errorf("MonitorTasks status = %q, want Pending", mt.Status)
	}
	if mt.SubmittedAt.IsZero() {
		t.Error("expected SubmittedAt to be set")
	}
}

func TestEnsureMonitorTask_Idempotent(t *testing.T) {
	existingID := uuid.New()
	mock := &mockSidecarClient{}
	node := monitorReplayerNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}
	node.Status.MonitorTasks = map[string]seiv1alpha1.MonitorTask{
		planner.TaskResultExport: {
			ID:          existingID.String(),
			Status:      seiv1alpha1.TaskPending,
			SubmittedAt: metav1.Now(),
		},
	}

	r, _ := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	req := planner.ResultExportMonitorTask(node)
	if err := r.ensureMonitorTask(ctx, node, mock, *req); err != nil {
		t.Fatalf("ensureMonitorTask: %v", err)
	}

	if len(mock.submitted) != 0 {
		t.Errorf("expected no submissions for existing task, got %d", len(mock.submitted))
	}
}

// --- pollMonitorTasks tests ---

func TestPollMonitorTasks_Completed(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{
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

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	requeue, err := r.pollMonitorTasks(ctx, node, mock)
	if err != nil {
		t.Fatalf("pollMonitorTasks: %v", err)
	}
	if !requeue {
		t.Error("expected requeue=true on task completion")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	mt := updated.Status.MonitorTasks[planner.TaskResultExport]
	if mt.Status != seiv1alpha1.TaskComplete {
		t.Errorf("monitor task status = %q, want Complete", mt.Status)
	}
	if mt.Error != "" {
		t.Errorf("monitor task error = %q, want empty", mt.Error)
	}
	if mt.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, ConditionResultExportComplete)
	if cond == nil {
		t.Fatal("expected ResultExportComplete condition")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("condition status = %q, want True", cond.Status)
	}
	if cond.Reason != ReasonDivergenceDetected {
		t.Errorf("condition reason = %q, want %q", cond.Reason, ReasonDivergenceDetected)
	}
}

func TestPollMonitorTasks_Failed(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{
		taskResults: map[uuid.UUID]*sidecar.TaskResult{
			taskID: completedResult(taskID, planner.TaskResultExport, strPtr("apphash mismatch")),
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

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	requeue, err := r.pollMonitorTasks(ctx, node, mock)
	if err != nil {
		t.Fatalf("pollMonitorTasks: %v", err)
	}
	if !requeue {
		t.Error("expected requeue=true on task failure")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	mt := updated.Status.MonitorTasks[planner.TaskResultExport]
	if mt.Status != seiv1alpha1.TaskFailed {
		t.Errorf("monitor task status = %q, want Failed", mt.Status)
	}
	if mt.Error != "apphash mismatch" {
		t.Errorf("monitor task error = %q, want %q", mt.Error, "apphash mismatch")
	}
	if mt.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, ConditionResultExportComplete)
	if cond == nil {
		t.Fatal("expected ResultExportComplete condition")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("condition status = %q, want False", cond.Status)
	}
	if cond.Reason != ReasonTaskFailed {
		t.Errorf("condition reason = %q, want %q", cond.Reason, ReasonTaskFailed)
	}
}

func TestPollMonitorTasks_StillRunning(t *testing.T) {
	taskID := uuid.New()
	now := time.Now()
	mock := &mockSidecarClient{
		taskResults: map[uuid.UUID]*sidecar.TaskResult{
			taskID: {
				Id:          taskID,
				Type:        planner.TaskResultExport,
				Status:      sidecar.Running,
				SubmittedAt: now.Add(-10 * time.Second),
			},
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

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	requeue, err := r.pollMonitorTasks(ctx, node, mock)
	if err != nil {
		t.Fatalf("pollMonitorTasks: %v", err)
	}
	if requeue {
		t.Error("expected requeue=false for still-running task")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	mt := updated.Status.MonitorTasks[planner.TaskResultExport]
	if mt.Status != seiv1alpha1.TaskPending {
		t.Errorf("monitor task status = %q, want Pending", mt.Status)
	}
}

func TestPollMonitorTasks_SkipsCompletedTasks(t *testing.T) {
	taskID := uuid.New()
	now := metav1.Now()
	mock := &mockSidecarClient{}
	node := monitorReplayerNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}
	node.Status.MonitorTasks = map[string]seiv1alpha1.MonitorTask{
		planner.TaskResultExport: {
			ID:          taskID.String(),
			Status:      seiv1alpha1.TaskComplete,
			SubmittedAt: metav1.NewTime(now.Add(-10 * time.Minute)),
			CompletedAt: &now,
		},
	}

	r, _ := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	requeue, err := r.pollMonitorTasks(ctx, node, mock)
	if err != nil {
		t.Fatalf("pollMonitorTasks: %v", err)
	}
	if requeue {
		t.Error("expected requeue=false for already-completed task")
	}
}

func TestPollMonitorTasks_SidecarLostTask(t *testing.T) {
	taskID := uuid.New()
	// Empty mock returns ErrNotFound for any GetTask call, simulating a
	// sidecar restart that lost the task's in-memory state.
	mock := &mockSidecarClient{}
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

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	requeue, err := r.pollMonitorTasks(ctx, node, mock)
	if err != nil {
		t.Fatalf("pollMonitorTasks: %v", err)
	}
	if !requeue {
		t.Error("expected requeue=true when sidecar lost task")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	mt := updated.Status.MonitorTasks[planner.TaskResultExport]
	if mt.Status != seiv1alpha1.TaskFailed {
		t.Errorf("monitor task status = %q, want Failed", mt.Status)
	}
	if mt.Error == "" {
		t.Error("expected non-empty error for lost task")
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, ConditionResultExportComplete)
	if cond == nil {
		t.Fatal("expected ResultExportComplete condition")
	}
	if cond.Reason != ReasonTaskLost {
		t.Errorf("condition reason = %q, want %q", cond.Reason, ReasonTaskLost)
	}
}

// --- ensureMonitorTask error handling ---

func TestEnsureMonitorTask_SubmitError(t *testing.T) {
	mock := &mockSidecarClient{submitErr: fmt.Errorf("connection refused")}
	node := monitorReplayerNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}

	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()

	req := planner.ResultExportMonitorTask(node)
	err := r.ensureMonitorTask(ctx, node, mock, *req)
	if err == nil {
		t.Fatal("expected error from failed submit")
	}

	// MonitorTasks should NOT be set — the submit failed before patching.
	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.MonitorTasks != nil {
		t.Errorf("expected nil MonitorTasks after submit failure, got %v", updated.Status.MonitorTasks)
	}
}

// --- pollMonitorTasks edge cases ---

func TestPollMonitorTasks_EmptyMap(t *testing.T) {
	mock := &mockSidecarClient{}
	node := monitorReplayerNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}

	r, _ := newProgressionReconciler(t, mock, node)

	requeue, err := r.pollMonitorTasks(context.Background(), node, mock)
	if err != nil {
		t.Fatalf("pollMonitorTasks: %v", err)
	}
	if requeue {
		t.Error("expected requeue=false for empty MonitorTasks")
	}
}

func TestPollMonitorTasks_InvalidUUID(t *testing.T) {
	mock := &mockSidecarClient{}
	node := monitorReplayerNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}
	node.Status.MonitorTasks = map[string]seiv1alpha1.MonitorTask{
		planner.TaskResultExport: {
			ID:          "not-a-uuid",
			Status:      seiv1alpha1.TaskPending,
			SubmittedAt: metav1.Now(),
		},
	}

	r, c := newProgressionReconciler(t, mock, node)

	requeue, err := r.pollMonitorTasks(context.Background(), node, mock)
	if err != nil {
		t.Fatalf("pollMonitorTasks: %v", err)
	}
	if !requeue {
		t.Error("expected requeue=true for invalid UUID failure")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	mt := updated.Status.MonitorTasks[planner.TaskResultExport]
	if mt.Status != seiv1alpha1.TaskFailed {
		t.Errorf("status = %q, want Failed", mt.Status)
	}
	if mt.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, ConditionResultExportComplete)
	if cond == nil {
		t.Fatal("expected ResultExportComplete condition")
	}
	if cond.Reason != ReasonTaskFailed {
		t.Errorf("condition reason = %q, want %q", cond.Reason, ReasonTaskFailed)
	}
}

func TestPollMonitorTasks_TransientGetTaskError(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{
		getTaskErr: fmt.Errorf("connection timeout"),
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

	r, c := newProgressionReconciler(t, mock, node)

	requeue, err := r.pollMonitorTasks(context.Background(), node, mock)
	if err != nil {
		t.Fatalf("pollMonitorTasks: %v", err)
	}
	if requeue {
		t.Error("expected requeue=false for transient error (will retry next reconcile)")
	}

	// Task should remain Pending — transient errors don't change status.
	updated := fetchNode(t, c, node.Name, node.Namespace)
	mt := updated.Status.MonitorTasks[planner.TaskResultExport]
	if mt.Status != seiv1alpha1.TaskPending {
		t.Errorf("status = %q, want Pending", mt.Status)
	}
}

func TestPollMonitorTasks_FailedWithUnknownError(t *testing.T) {
	taskID := uuid.New()
	now := time.Now()
	mock := &mockSidecarClient{
		taskResults: map[uuid.UUID]*sidecar.TaskResult{
			taskID: {
				Id:          taskID,
				Type:        planner.TaskResultExport,
				Status:      sidecar.Failed,
				SubmittedAt: now.Add(-10 * time.Second),
				CompletedAt: &now,
				Error:       nil,
			},
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

	r, c := newProgressionReconciler(t, mock, node)

	requeue, err := r.pollMonitorTasks(context.Background(), node, mock)
	if err != nil {
		t.Fatalf("pollMonitorTasks: %v", err)
	}
	if !requeue {
		t.Error("expected requeue=true on task failure")
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	mt := updated.Status.MonitorTasks[planner.TaskResultExport]
	if mt.Status != seiv1alpha1.TaskFailed {
		t.Errorf("status = %q, want Failed", mt.Status)
	}
	if mt.Error != "task failed with unknown error" {
		t.Errorf("error = %q, want %q", mt.Error, "task failed with unknown error")
	}
}

// --- Reconcile integration tests ---

func TestReconcileRunning_MonitorMode_SubmitsMonitorTask(t *testing.T) {
	taskID := uuid.New()
	mock := &mockSidecarClient{submitID: taskID}
	node := monitorReplayerNode()
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.InitPlan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanComplete}

	r, c := newProgressionReconciler(t, mock, node)

	_, err := r.reconcileRunning(context.Background(), node)
	if err != nil {
		t.Fatalf("reconcileRunning: %v", err)
	}

	if len(mock.submitted) != 1 {
		t.Fatalf("expected 1 submission, got %d", len(mock.submitted))
	}
	if mock.submitted[0].Type != planner.TaskResultExport {
		t.Errorf("submitted type = %q, want %q", mock.submitted[0].Type, planner.TaskResultExport)
	}

	updated := fetchNode(t, c, node.Name, node.Namespace)
	if updated.Status.MonitorTasks == nil {
		t.Fatal("expected MonitorTasks to be set")
	}
	if _, ok := updated.Status.MonitorTasks[planner.TaskResultExport]; !ok {
		t.Errorf("expected MonitorTasks[%s] to exist", planner.TaskResultExport)
	}
}

// --- Planner builder tests ---

func TestResultExportMonitorTask_ReturnsRequest(t *testing.T) {
	node := monitorReplayerNode()
	req := planner.ResultExportMonitorTask(node)
	if req == nil {
		t.Fatal("expected non-nil TaskRequest")
	}
	if req.Type != planner.TaskResultExport {
		t.Errorf("Type = %q, want %q", req.Type, planner.TaskResultExport)
	}
	if req.Params == nil {
		t.Fatal("expected non-nil params")
	}
	params := *req.Params
	if params["canonicalRpc"] != "http://canonical-rpc:26657" {
		t.Errorf("canonicalRpc = %v, want %q", params["canonicalRpc"], "http://canonical-rpc:26657")
	}
	if _, ok := params["bucket"]; !ok {
		t.Error("expected bucket param")
	}
}

func TestResultExportMonitorTask_NilWithoutResultExport(t *testing.T) {
	node := replayerNode()
	req := planner.ResultExportMonitorTask(node)
	if req != nil {
		t.Errorf("expected nil, got %v", req)
	}
}

func TestResultExportMonitorTask_NilForNonReplayer(t *testing.T) {
	node := snapshotNode()
	req := planner.ResultExportMonitorTask(node)
	if req != nil {
		t.Errorf("expected nil, got %v", req)
	}
}
