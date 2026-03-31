package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	ConditionResultExportComplete = "ResultExportComplete"
	ReasonDivergenceDetected      = "DivergenceDetected"
	ReasonTaskFailed              = "TaskFailed"
	ReasonTaskLost                = "TaskLost"
)

// ensureMonitorTasks submits all applicable monitor tasks for a node based
// on its spec. Each task is submitted exactly once (idempotent). Errors do
// not short-circuit — all applicable tasks are attempted.
func (r *SeiNodeReconciler) ensureMonitorTasks(ctx context.Context, node *seiv1alpha1.SeiNode, sc task.SidecarClient) error {
	var firstErr error

	if req := planner.SnapshotUploadMonitorTask(node); req != nil {
		if err := r.ensureMonitorTask(ctx, node, sc, *req); err != nil {
			firstErr = err
		}
	}

	if req := planner.ResultExportMonitorTask(node); req != nil {
		if err := r.ensureMonitorTask(ctx, node, sc, *req); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// ensureMonitorTask submits a monitor task exactly once, tracking it in
// node.Status.MonitorTasks. Idempotency: skips if the task type already exists.
func (r *SeiNodeReconciler) ensureMonitorTask(ctx context.Context, node *seiv1alpha1.SeiNode, sc task.SidecarClient, req sidecar.TaskRequest) error {
	if node.Status.MonitorTasks != nil {
		if _, ok := node.Status.MonitorTasks[req.Type]; ok {
			return nil
		}
	}

	id, err := sc.SubmitTask(ctx, req)
	if err != nil {
		return fmt.Errorf("submitting monitor task %s: %w", req.Type, err)
	}

	now := metav1.Now()
	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if node.Status.MonitorTasks == nil {
		node.Status.MonitorTasks = make(map[string]seiv1alpha1.MonitorTask)
	}
	node.Status.MonitorTasks[req.Type] = seiv1alpha1.MonitorTask{
		ID:          id.String(),
		Status:      seiv1alpha1.TaskPending,
		SubmittedAt: now,
	}
	emitMonitorTaskStatus(node.Namespace, node.Name, req.Type, string(seiv1alpha1.TaskPending))
	return r.Status().Patch(ctx, node, patch)
}

// pollMonitorTasks checks each pending monitor task via the sidecar API.
// When a task completes or fails, it patches the status, sets a Condition,
// and emits a Kubernetes Event. Returns true when a terminal state was
// observed, signaling the caller to requeue immediately.
func (r *SeiNodeReconciler) pollMonitorTasks(ctx context.Context, node *seiv1alpha1.SeiNode, sc task.SidecarClient) (bool, error) {
	if len(node.Status.MonitorTasks) == 0 {
		return false, nil
	}

	logger := log.FromContext(ctx)
	var patched bool
	var terminal bool

	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})

	for key, mt := range node.Status.MonitorTasks {
		if mt.Status != seiv1alpha1.TaskPending {
			continue
		}

		taskID, parseErr := uuid.Parse(mt.ID)
		if parseErr != nil {
			logger.Info("invalid monitor task UUID, marking failed", "task", key, "id", mt.ID)
			r.failMonitorTask(node, key, mt, fmt.Sprintf("invalid task UUID: %s", mt.ID), ReasonTaskFailed)
			patched = true
			terminal = true
			continue
		}

		result, err := sc.GetTask(ctx, taskID)
		if err != nil {
			if errors.Is(err, sidecar.ErrNotFound) {
				// The sidecar inserts a task into its active map synchronously
				// before returning from Submit. ErrNotFound after a successful
				// submission means the sidecar process restarted and lost the task.
				logger.Info("monitor task lost by sidecar", "task", key, "submittedAt", mt.SubmittedAt.Time)
				r.failMonitorTask(node, key, mt, "sidecar lost task (not found after successful submission)", ReasonTaskLost)
				patched = true
				terminal = true
				continue
			}
			logger.Info("monitor task poll error, will retry", "task", key, "error", err)
			continue
		}

		switch result.Status {
		case sidecar.Completed:
			now := metav1.Now()
			mt.Status = seiv1alpha1.TaskComplete
			mt.CompletedAt = &now
			node.Status.MonitorTasks[key] = mt
			patched = true
			terminal = true

			emitMonitorTaskTerminal(node.Namespace, node.Name, key, ReasonDivergenceDetected)
			emitMonitorTaskStatus(node.Namespace, node.Name, key, string(seiv1alpha1.TaskComplete))

			meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
				Type:               ConditionResultExportComplete,
				Status:             metav1.ConditionTrue,
				Reason:             ReasonDivergenceDetected,
				Message:            fmt.Sprintf("Monitor task %s completed — app-hash divergence detected", key),
				ObservedGeneration: node.Generation,
			})
			r.Recorder.Eventf(node, corev1.EventTypeNormal, "MonitorTaskCompleted",
				"Monitor task %s completed", key)
			logger.Info("monitor task completed", "task", key)

		case sidecar.Failed:
			errMsg := "task failed with unknown error"
			if result.Error != nil && *result.Error != "" {
				errMsg = *result.Error
			}
			r.failMonitorTask(node, key, mt, errMsg, ReasonTaskFailed)
			patched = true
			terminal = true
		}
	}

	if patched {
		return terminal, r.Status().Patch(ctx, node, patch)
	}
	return false, nil
}

// failMonitorTask marks a monitor task as failed, sets the Condition, and emits an Event.
func (r *SeiNodeReconciler) failMonitorTask(node *seiv1alpha1.SeiNode, key string, mt seiv1alpha1.MonitorTask, errMsg, reason string) {
	now := metav1.Now()
	mt.Status = seiv1alpha1.TaskFailed
	mt.CompletedAt = &now
	mt.Error = errMsg
	node.Status.MonitorTasks[key] = mt

	emitMonitorTaskTerminal(node.Namespace, node.Name, key, reason)
	emitMonitorTaskStatus(node.Namespace, node.Name, key, string(seiv1alpha1.TaskFailed))

	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               ConditionResultExportComplete,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            fmt.Sprintf("Monitor task %s failed: %s", key, errMsg),
		ObservedGeneration: node.Generation,
	})
	r.Recorder.Eventf(node, corev1.EventTypeWarning, "MonitorTaskFailed",
		"Monitor task %s failed: %s", key, errMsg)
}
