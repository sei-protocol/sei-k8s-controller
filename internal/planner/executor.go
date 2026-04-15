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

// ResultRequeueImmediate requests an immediate re-enqueue. Uses a minimal
var ResultRequeueImmediate = ctrl.Result{RequeueAfter: 1 * time.Millisecond}

// PlanExecutor drives a TaskPlan to completion for a given resource type.
// T is the owning Kubernetes object whose status carries the plan.
type PlanExecutor[T client.Object] interface {
	ExecutePlan(ctx context.Context, obj T, plan *seiv1alpha1.TaskPlan) (ctrl.Result, error)
}

// PhaseTransition records a phase change triggered by plan completion or failure.
type PhaseTransition struct {
	PrevPhase string
	NewPhase  string
}

// Executor[T] is the concrete implementation of PlanExecutor. It is stateless
// per reconcile — each call re-deserializes the current task from the plan's
// embedded params and calls Status/Execute.
//
// ConfigFor is a factory that builds the ExecutionConfig for the given
// resource. This is where the sidecar client, kube client, and other
// dependencies are resolved per-reconcile. The context is forwarded from
// the reconcile call in case the factory needs to make API calls.
//
// OnPlanComplete and OnPlanFailed are optional callbacks invoked after the
// status patch succeeds. They allow the caller to emit metrics or events
// without coupling the executor to resource-specific concerns.
type Executor[T client.Object] struct {
	Client         client.Client
	ConfigFor      func(ctx context.Context, obj T) task.ExecutionConfig
	OnPlanComplete func(ctx context.Context, obj T, transition *PhaseTransition)
	OnPlanFailed   func(ctx context.Context, obj T, transition *PhaseTransition)
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
	callbacks := planCallbacks{
		onComplete: func(ctx context.Context, transition *PhaseTransition) {
			if e.OnPlanComplete != nil {
				e.OnPlanComplete(ctx, obj, transition)
			}
		},
		onFailed: func(ctx context.Context, transition *PhaseTransition) {
			if e.OnPlanFailed != nil {
				e.OnPlanFailed(ctx, obj, transition)
			}
		},
	}
	return executePlan(ctx, e.Client, obj, plan, cfg, callbacks)
}

// planCallbacks are optional hooks invoked after plan-terminal status patches.
type planCallbacks struct {
	onComplete func(ctx context.Context, transition *PhaseTransition)
	onFailed   func(ctx context.Context, transition *PhaseTransition)
}

// executePlan is the core plan execution loop. It deserializes the current
// task, polls/submits it, and patches the owning object's status.
func executePlan(
	ctx context.Context,
	kc client.Client,
	obj client.Object,
	plan *seiv1alpha1.TaskPlan,
	cfg task.ExecutionConfig,
	cb planCallbacks,
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
		transition := applyTargetPhase(obj, plan)
		patch := client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})
		plan.Phase = seiv1alpha1.TaskPlanComplete
		applyPhaseToStatus(obj, transition)
		nilPlanIfConvergence(obj, plan, transition)
		if err := kc.Status().Patch(ctx, obj, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking plan complete: %w", err)
		}
		planActive.WithLabelValues(cn, obj.GetNamespace(), obj.GetName()).Set(0)
		if cb.onComplete != nil {
			cb.onComplete(ctx, transition)
		}
		return ResultRequeueImmediate, nil
	}

	var paramsRaw []byte
	if t.Params != nil {
		paramsRaw = t.Params.Raw
	}

	exec, err := task.Deserialize(t.Type, t.ID, paramsRaw, cfg)
	if err != nil {
		return failTask(ctx, kc, obj, cn, plan, t, err.Error(), cb)
	}

	if needsSubmission(t) {
		if err := exec.Execute(ctx); err != nil {
			var termErr *task.TerminalError
			if errors.As(err, &termErr) {
				return failTask(ctx, kc, obj, cn, plan, t, err.Error(), cb)
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
		return failTask(ctx, kc, obj, cn, plan, t, errMsg, cb)

	default:
		return ctrl.Result{RequeueAfter: TaskPollInterval}, nil
	}
}

// failTask marks an individual task and the overall plan as Failed.
func failTask(
	ctx context.Context,
	kc client.Client,
	obj client.Object,
	controller string,
	plan *seiv1alpha1.TaskPlan,
	t *seiv1alpha1.PlannedTask,
	errMsg string,
	cb planCallbacks,
) (ctrl.Result, error) {
	log.FromContext(ctx).Error(fmt.Errorf("task failed: %s", errMsg), "task plan failed", "task", t.Type)

	taskFailuresTotal.WithLabelValues(controller, obj.GetNamespace(), t.Type).Inc()

	transition := applyFailedPhase(obj, plan)
	patch := client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})
	t.Status = seiv1alpha1.TaskFailed
	t.Error = errMsg
	plan.Phase = seiv1alpha1.TaskPlanFailed
	applyPhaseToStatus(obj, transition)
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
	if cb.onFailed != nil {
		cb.onFailed(ctx, transition)
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

// applyTargetPhase builds a PhaseTransition from the plan's TargetPhase.
// Returns nil if no transition is needed.
func applyTargetPhase(obj client.Object, plan *seiv1alpha1.TaskPlan) *PhaseTransition {
	if plan.TargetPhase == "" {
		return nil
	}
	node, ok := obj.(*seiv1alpha1.SeiNode)
	if !ok {
		return nil
	}
	prev := string(node.Status.Phase)
	return &PhaseTransition{PrevPhase: prev, NewPhase: string(plan.TargetPhase)}
}

// applyFailedPhase builds a PhaseTransition from the plan's FailedPhase.
// Returns nil if no transition is needed.
func applyFailedPhase(obj client.Object, plan *seiv1alpha1.TaskPlan) *PhaseTransition {
	if plan.FailedPhase == "" {
		return nil
	}
	node, ok := obj.(*seiv1alpha1.SeiNode)
	if !ok {
		return nil
	}
	prev := string(node.Status.Phase)
	return &PhaseTransition{PrevPhase: prev, NewPhase: string(plan.FailedPhase)}
}

// applyPhaseToStatus sets the node's phase if a transition was determined.
func applyPhaseToStatus(obj client.Object, transition *PhaseTransition) {
	if transition == nil {
		return
	}
	if node, ok := obj.(*seiv1alpha1.SeiNode); ok {
		node.Status.Phase = seiv1alpha1.SeiNodePhase(transition.NewPhase)
	}
}

// nilPlanIfConvergence nils the plan on the object's status when the completed
// plan's target phase matches the current phase (convergence — the node stays
// in the same phase). This avoids dead plan data in etcd between reconciles.
func nilPlanIfConvergence(obj client.Object, _ *seiv1alpha1.TaskPlan, transition *PhaseTransition) {
	if transition == nil {
		return
	}
	if transition.PrevPhase == transition.NewPhase {
		if node, ok := obj.(*seiv1alpha1.SeiNode); ok {
			node.Status.Plan = nil
		}
	}
}
