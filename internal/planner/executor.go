package planner

import (
	"context"
	"fmt"
	"time"

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

// ResultRequeueImmediate requests an immediate re-enqueue. Uses a minimal
var ResultRequeueImmediate = ctrl.Result{RequeueAfter: 1 * time.Millisecond}

// PlanExecutor drives a TaskPlan to completion for a given resource type.
// T is the owning Kubernetes object whose status carries the plan.
type PlanExecutor[T client.Object] interface {
	ExecutePlan(ctx context.Context, obj T, plan *seiv1alpha1.TaskPlan) (ctrl.Result, error)
}

// Executor[T] is the concrete implementation of PlanExecutor. It is stateless
// per reconcile — each call re-deserializes the current task from the plan's
// embedded params and calls Status/Execute.
//
// ConfigFor is a factory that builds the ExecutionConfig for the given
// resource. This is where the sidecar client, kube client, and other
// dependencies are resolved per-reconcile. The context is forwarded from
// the reconcile call in case the factory needs to make API calls.
type Executor[T client.Object] struct {
	Client    client.Client
	ConfigFor func(ctx context.Context, obj T) task.ExecutionConfig
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

// ExecutePlan drives a TaskPlan one step forward. The plan pointer must
// reference a field inside the object's status so that status patches
// capture mutations.
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
	return executePlan(ctx, e.Client, obj, plan, cfg)
}

// executePlan is the core plan execution loop. It deserializes the current
// task, polls/submits it, and patches the owning object's status.
func executePlan(
	ctx context.Context,
	kc client.Client,
	obj client.Object,
	plan *seiv1alpha1.TaskPlan,
	cfg task.ExecutionConfig,
) (ctrl.Result, error) {
	switch plan.Phase {
	case seiv1alpha1.TaskPlanComplete, seiv1alpha1.TaskPlanFailed:
		return ctrl.Result{}, nil
	}

	t := CurrentTask(plan)
	if t == nil {
		patch := client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})
		plan.Phase = seiv1alpha1.TaskPlanComplete
		if err := kc.Status().Patch(ctx, obj, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking plan complete: %w", err)
		}
		return ResultRequeueImmediate, nil
	}

	var paramsRaw []byte
	if t.Params != nil {
		paramsRaw = t.Params.Raw
	}

	exec, err := task.Deserialize(t.Type, t.ID, paramsRaw, cfg)
	if err != nil {
		return failTask(ctx, kc, obj, plan, t, err.Error())
	}

	status := exec.Status(ctx)

	if status == task.ExecutionRunning && t.Status == seiv1alpha1.TaskPending {
		if err := exec.Execute(ctx); err != nil {
			log.FromContext(ctx).Info("task submission failed, will retry",
				"task", t.Type, "error", err)
			return ctrl.Result{RequeueAfter: TaskPollInterval}, nil
		}
		status = exec.Status(ctx)
	}

	switch status {
	case task.ExecutionRunning:
		return ctrl.Result{RequeueAfter: TaskPollInterval}, nil

	case task.ExecutionComplete:
		patch := client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})
		t.Status = seiv1alpha1.TaskComplete
		if err := kc.Status().Patch(ctx, obj, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking task complete: %w", err)
		}
		return ResultRequeueImmediate, nil

	case task.ExecutionFailed:
		errMsg := "unknown error"
		if exec.Err() != nil {
			errMsg = exec.Err().Error()
		}
		if t.MaxRetries > 0 && t.RetryCount < t.MaxRetries {
			return retryTask(ctx, kc, obj, t, errMsg)
		}
		return failTask(ctx, kc, obj, plan, t, errMsg)

	default:
		return ctrl.Result{RequeueAfter: TaskPollInterval}, nil
	}
}

// retryTask resets a failed task for another attempt with a new deterministic
// ID, so the sidecar treats it as a fresh submission.
func retryTask(
	ctx context.Context,
	kc client.Client,
	obj client.Object,
	t *seiv1alpha1.PlannedTask,
	errMsg string,
) (ctrl.Result, error) {
	patch := client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})

	t.RetryCount++
	log.FromContext(ctx).Info("retrying failed task",
		"task", t.Type, "attempt", t.RetryCount, "maxRetries", t.MaxRetries, "lastError", errMsg)

	resourceName := obj.GetName()
	t.ID = task.DeterministicTaskID(resourceName, t.Type, t.RetryCount)
	t.Status = seiv1alpha1.TaskPending
	t.Error = ""

	if err := kc.Status().Patch(ctx, obj, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("resetting task for retry: %w", err)
	}

	backoff := retryBackoff(t.RetryCount)
	return ctrl.Result{RequeueAfter: backoff}, nil
}

// failTask marks an individual task and the overall plan as Failed.
func failTask(
	ctx context.Context,
	kc client.Client,
	obj client.Object,
	plan *seiv1alpha1.TaskPlan,
	t *seiv1alpha1.PlannedTask,
	errMsg string,
) (ctrl.Result, error) {
	log.FromContext(ctx).Error(fmt.Errorf("task failed: %s", errMsg), "task plan failed", "task", t.Type)

	patch := client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})
	t.Status = seiv1alpha1.TaskFailed
	t.Error = errMsg
	plan.Phase = seiv1alpha1.TaskPlanFailed
	if err := kc.Status().Patch(ctx, obj, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("marking task failed: %w", err)
	}
	return ctrl.Result{}, nil
}

// retryBackoff returns an exponential backoff duration capped at maxRetryBackoff.
func retryBackoff(attempt int) time.Duration {
	d := TaskPollInterval * time.Duration(1<<min(attempt, 5))
	if d > maxRetryBackoff {
		return maxRetryBackoff
	}
	return d
}
