package planner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
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

	// ConditionPlanFailed is set on the node when a plan fails terminally.
	ConditionPlanFailed = "PlanFailed"
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

// needsSubmission reports whether the task should be submitted (or resubmitted)
// to the sidecar. This is true when the CRD task status is Pending, which
// covers both first-time submission and retries after failure. We submit
// before polling Status because the sidecar may still hold a stale Failed
// result from a previous run — the sidecar's cloud-API Submit transparently
// re-executes failed tasks when resubmitted with the same stable ID.
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
	cn := controllerName(obj)

	switch plan.Phase {
	case seiv1alpha1.TaskPlanComplete, seiv1alpha1.TaskPlanFailed:
		return ctrl.Result{}, nil
	}

	// Mark the plan as active for this resource.
	planActive.WithLabelValues(cn, obj.GetNamespace(), obj.GetName()).Set(1)

	t := CurrentTask(plan)
	if t == nil {
		prevPhase := currentPhase(obj)
		patch := client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})
		plan.Phase = seiv1alpha1.TaskPlanComplete
		setTargetPhase(obj, plan.TargetPhase)
		nilPlanIfConvergence(obj, prevPhase, plan.TargetPhase)
		if err := kc.Status().Patch(ctx, obj, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking plan complete: %w", err)
		}
		planActive.WithLabelValues(cn, obj.GetNamespace(), obj.GetName()).Set(0)
		return ResultRequeueImmediate, nil
	}

	var paramsRaw []byte
	if t.Params != nil {
		paramsRaw = t.Params.Raw
	}

	exec, err := task.Deserialize(t.Type, t.ID, paramsRaw, cfg)
	if err != nil {
		return failTask(ctx, kc, obj, cn, plan, t, err.Error())
	}

	if needsSubmission(t) {
		if err := exec.Execute(ctx); err != nil {
			var termErr *task.TerminalError
			if errors.As(err, &termErr) {
				return failTask(ctx, kc, obj, cn, plan, t, err.Error())
			}
			log.FromContext(ctx).Info("task execution failed, will retry",
				"task", t.Type, "error", err)
			return ctrl.Result{RequeueAfter: TaskPollInterval}, nil
		}
		if t.SubmittedAt == nil {
			now := metav1.Now()
			t.SubmittedAt = &now
		}
		log.FromContext(ctx).Info("task submitted to sidecar", "task", t.Type, "id", t.ID)
	}

	status := exec.Status(ctx)

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
			taskRetriesTotal.WithLabelValues(cn, obj.GetNamespace(), t.Type).Inc()
			patch := client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})
			t.RetryCount++
			t.Status = seiv1alpha1.TaskPending
			t.Error = ""
			t.SubmittedAt = nil
			log.FromContext(ctx).Info("retrying failed task",
				"task", t.Type, "retry", t.RetryCount, "maxRetries", t.MaxRetries, "lastError", errMsg)
			if err := kc.Status().Patch(ctx, obj, patch); err != nil {
				return ctrl.Result{}, fmt.Errorf("resetting task for retry: %w", err)
			}
			return ctrl.Result{RequeueAfter: retryBackoff(t.RetryCount)}, nil
		}
		return failTask(ctx, kc, obj, cn, plan, t, errMsg)

	default:
		return ctrl.Result{RequeueAfter: TaskPollInterval}, nil
	}
}

// failTask marks an individual task and the overall plan as Failed.
// It sets the FailedPhase on the node and a PlanFailed condition.
func failTask(
	ctx context.Context,
	kc client.Client,
	obj client.Object,
	controller string,
	plan *seiv1alpha1.TaskPlan,
	t *seiv1alpha1.PlannedTask,
	errMsg string,
) (ctrl.Result, error) {
	log.FromContext(ctx).Error(fmt.Errorf("task failed: %s", errMsg), "task plan failed", "task", t.Type)

	taskFailuresTotal.WithLabelValues(controller, obj.GetNamespace(), t.Type).Inc()

	patch := client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})
	t.Status = seiv1alpha1.TaskFailed
	t.Error = errMsg
	plan.Phase = seiv1alpha1.TaskPlanFailed
	setTargetPhase(obj, plan.FailedPhase)
	setPlanFailedCondition(obj, plan, t, errMsg)
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
	if err := kc.Status().Patch(ctx, obj, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("marking task failed: %w", err)
	}
	planActive.WithLabelValues(controller, obj.GetNamespace(), obj.GetName()).Set(0)
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

// setTargetPhase sets the node's phase if the plan specifies one.
func setTargetPhase(obj client.Object, phase seiv1alpha1.SeiNodePhase) {
	if phase == "" {
		return
	}
	if node, ok := obj.(*seiv1alpha1.SeiNode); ok {
		node.Status.Phase = phase
	}
}

// setPlanFailedCondition sets a PlanFailed condition on the node with
// details about which task failed and why.
func setPlanFailedCondition(obj client.Object, plan *seiv1alpha1.TaskPlan, t *seiv1alpha1.PlannedTask, errMsg string) {
	node, ok := obj.(*seiv1alpha1.SeiNode)
	if !ok {
		return
	}
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               ConditionPlanFailed,
		Status:             metav1.ConditionTrue,
		Reason:             "TaskFailed",
		Message:            fmt.Sprintf("plan %s failed at task %s: %s", plan.ID, t.Type, errMsg),
		ObservedGeneration: node.Generation,
	})
}

// currentPhase returns the SeiNode phase, or empty for non-SeiNode objects.
func currentPhase(obj client.Object) seiv1alpha1.SeiNodePhase {
	if node, ok := obj.(*seiv1alpha1.SeiNode); ok {
		return node.Status.Phase
	}
	return ""
}

// nilPlanIfConvergence nils the plan on the object's status when the plan's
// target phase matches the phase the node was already in before the plan
// completed (convergence — the node stays in the same phase). Init plans
// that transition to a new phase keep their completed plan visible in status.
func nilPlanIfConvergence(obj client.Object, prevPhase, targetPhase seiv1alpha1.SeiNodePhase) {
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
