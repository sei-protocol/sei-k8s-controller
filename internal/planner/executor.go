package planner

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	TaskPollInterval = 5 * time.Second
	maxRetryBackoff  = 30 * time.Second
)

// ResultRequeueImmediate requests an immediate re-enqueue with a minimal
// delay to avoid busy-looping while still progressing the plan promptly.
var ResultRequeueImmediate = ctrl.Result{RequeueAfter: 1 * time.Millisecond}

// PlanExecutor drives a TaskPlan to completion for a given resource type.
// T is the owning Kubernetes object whose status carries the plan.
type PlanExecutor[T client.Object] interface {
	ExecutePlan(ctx context.Context, obj T, plan *seiv1alpha1.TaskPlan) (ctrl.Result, error)
}

// Executor[T] is the concrete implementation of PlanExecutor. It is stateless
// per reconcile — each call re-deserializes the current task from the plan's
// embedded params and calls Execute/Status.
//
// ExecutePlan performs all mutations in-memory on the owning object. The
// caller is responsible for persisting status changes via a single patch.
//
// ConfigFor is a factory that builds the ExecutionConfig for the given
// resource. This is where the sidecar client, kube client, and other
// dependencies are resolved per-reconcile.
type Executor[T client.Object] struct {
	ConfigFor func(ctx context.Context, obj T) task.ExecutionConfig
}

// needsSubmission reports whether the task should be submitted (or resubmitted)
// to the sidecar. This is true when the CRD task status is Pending, which
// covers both first-time submission and retries after failure.
func needsSubmission(t *seiv1alpha1.PlannedTask) bool {
	return t.Status == seiv1alpha1.TaskPending
}

// CurrentTask returns the first non-Complete task in the plan, or nil if all
// tasks are complete.
func CurrentTask(plan *seiv1alpha1.TaskPlan) *seiv1alpha1.PlannedTask {
	for i := range plan.Tasks {
		if plan.Tasks[i].Status != seiv1alpha1.TaskComplete {
			return &plan.Tasks[i]
		}
	}
	return nil
}

// ExecutePlan drives a TaskPlan forward, mutating obj's status in-memory.
// Synchronous tasks (those that complete within Execute) advance in a loop
// so that multiple fire-and-forget tasks complete in a single reconcile.
// The caller is responsible for persisting all status mutations.
func (e *Executor[T]) ExecutePlan(
	ctx context.Context,
	obj T,
	plan *seiv1alpha1.TaskPlan,
) (ctrl.Result, error) {
	if plan == nil {
		return ctrl.Result{}, fmt.Errorf("ExecutePlan called with nil plan for %s/%s",
			obj.GetNamespace(), obj.GetName())
	}

	cfg := e.ConfigFor(ctx, obj)
	return executePlan(ctx, obj, plan, cfg)
}

// executePlan is the core plan execution loop. It advances synchronous tasks
// in sequence, stopping at the first async (Running) task, error, or when
// the plan completes. All mutations are in-memory.
func executePlan(
	ctx context.Context,
	obj client.Object,
	plan *seiv1alpha1.TaskPlan,
	cfg task.ExecutionConfig,
) (ctrl.Result, error) {
	cn := controllerName(obj)

	switch plan.Phase {
	case seiv1alpha1.TaskPlanComplete, seiv1alpha1.TaskPlanFailed:
		return ctrl.Result{}, nil
	}

	planActive.WithLabelValues(cn, obj.GetNamespace(), obj.GetName()).Set(1)

	for {
		t := CurrentTask(plan)
		if t == nil {
			// All tasks complete — finalize the plan.
			prevPhase := currentPhase(obj)
			plan.Phase = seiv1alpha1.TaskPlanComplete
			setTargetPhase(obj, plan.TargetPhase)
			clearCompletedConvergencePlan(obj, prevPhase, plan.TargetPhase)
			planActive.WithLabelValues(cn, obj.GetNamespace(), obj.GetName()).Set(0)
			return ResultRequeueImmediate, nil
		}

		result := advanceTask(ctx, obj, cn, plan, t, cfg)

		// If the task is not yet complete, stop the loop — the result
		// carries the appropriate requeue interval.
		if t.Status != seiv1alpha1.TaskComplete {
			return result, nil
		}
	}
}

// advanceTask drives a single task one step forward: submit if pending,
// then check status. All mutations are in-memory.
func advanceTask(
	ctx context.Context,
	obj client.Object,
	cn string,
	plan *seiv1alpha1.TaskPlan,
	t *seiv1alpha1.PlannedTask,
	cfg task.ExecutionConfig,
) ctrl.Result {
	var paramsRaw []byte
	if t.Params != nil {
		paramsRaw = t.Params.Raw
	}

	exec, err := task.Deserialize(t.Type, t.ID, paramsRaw, cfg)
	if err != nil {
		failTask(ctx, obj, cn, plan, t, err.Error())
		return ctrl.Result{}
	}

	if needsSubmission(t) {
		if err := exec.Execute(ctx); err != nil {
			var termErr *task.TerminalError
			if errors.As(err, &termErr) {
				failTask(ctx, obj, cn, plan, t, err.Error())
				return ctrl.Result{}
			}
			log.FromContext(ctx).Info("task execution failed, will retry",
				"task", t.Type, "error", err)
			return ctrl.Result{RequeueAfter: TaskPollInterval}
		}
		if t.SubmittedAt == nil {
			now := metav1.Now()
			t.SubmittedAt = &now
		}
		log.FromContext(ctx).Info("task submitted", "task", t.Type, "id", t.ID)
	}

	status := exec.Status(ctx)

	switch status {
	case task.ExecutionRunning:
		return ctrl.Result{RequeueAfter: TaskPollInterval}

	case task.ExecutionComplete:
		t.Status = seiv1alpha1.TaskComplete
		return ResultRequeueImmediate

	case task.ExecutionFailed:
		errMsg := "unknown error"
		if exec.Err() != nil {
			errMsg = exec.Err().Error()
		}
		if t.MaxRetries > 0 && t.RetryCount < t.MaxRetries {
			taskRetriesTotal.WithLabelValues(cn, obj.GetNamespace(), t.Type).Inc()
			t.RetryCount++
			t.Status = seiv1alpha1.TaskPending
			t.Error = ""
			t.SubmittedAt = nil
			log.FromContext(ctx).Info("retrying failed task",
				"task", t.Type, "retry", t.RetryCount, "maxRetries", t.MaxRetries, "lastError", errMsg)
			return ctrl.Result{RequeueAfter: retryBackoff(t.RetryCount)}
		}
		failTask(ctx, obj, cn, plan, t, errMsg)
		return ctrl.Result{}

	default:
		return ctrl.Result{RequeueAfter: TaskPollInterval}
	}
}

// failTask marks an individual task and the overall plan as Failed.
// Sets FailedPhase on the node. The planner handles condition updates
// when it observes the failed plan on the next reconcile.
func failTask(
	ctx context.Context,
	obj client.Object,
	controller string,
	plan *seiv1alpha1.TaskPlan,
	t *seiv1alpha1.PlannedTask,
	errMsg string,
) {
	log.FromContext(ctx).Error(fmt.Errorf("task failed: %s", errMsg), "task plan failed", "task", t.Type)

	taskFailuresTotal.WithLabelValues(controller, obj.GetNamespace(), t.Type).Inc()

	t.Status = seiv1alpha1.TaskFailed
	t.Error = errMsg
	plan.Phase = seiv1alpha1.TaskPlanFailed
	setTargetPhase(obj, plan.FailedPhase)
	for i := range plan.Tasks {
		if plan.Tasks[i].ID == t.ID {
			plan.FailedTaskIndex = &i
			break
		}
	}
	plan.FailedTaskDetail = &seiv1alpha1.FailedTaskInfo{
		Type:       t.Type,
		ID:         t.ID,
		Error:      errMsg,
		RetryCount: t.RetryCount,
		MaxRetries: t.MaxRetries,
	}
	planActive.WithLabelValues(controller, obj.GetNamespace(), obj.GetName()).Set(0)
}

// retryBackoff returns an exponential backoff duration capped at maxRetryBackoff.
func retryBackoff(attempt int) time.Duration {
	d := TaskPollInterval * time.Duration(1<<min(attempt, 5))
	if d > maxRetryBackoff {
		return maxRetryBackoff
	}
	return d
}

// setTargetPhase sets the node's phase if the plan specifies one.
func setTargetPhase(obj client.Object, phase seiv1alpha1.SeiNodePhase) {
	if phase == "" {
		return
	}
	if node, ok := obj.(*seiv1alpha1.SeiNode); ok {
		node.Status.Phase = phase
	}
}

// currentPhase returns the SeiNode phase, or empty for non-SeiNode objects.
func currentPhase(obj client.Object) seiv1alpha1.SeiNodePhase {
	if node, ok := obj.(*seiv1alpha1.SeiNode); ok {
		return node.Status.Phase
	}
	return ""
}

// clearCompletedConvergencePlan nils the plan on the object's status when the
// plan's target phase matches the phase the node was already in before the
// plan completed (convergence — the node stays in the same phase). Init plans
// that transition to a new phase keep their completed plan visible in status.
func clearCompletedConvergencePlan(obj client.Object, prevPhase, targetPhase seiv1alpha1.SeiNodePhase) {
	if targetPhase == "" {
		return
	}
	if prevPhase != targetPhase {
		return
	}
	if node, ok := obj.(*seiv1alpha1.SeiNode); ok {
		node.Status.Plan = nil
	}
}
