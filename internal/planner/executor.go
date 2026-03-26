package planner

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	TaskPollInterval = 5 * time.Second
	maxRetryBackoff  = 30 * time.Second
)

// ResultRequeueImmediate is the idiomatic way to request an immediate
// re-enqueue in controller-runtime without an artificial timer delay.
var ResultRequeueImmediate = ctrl.Result{Requeue: true}

// Executor drives TaskPlans to completion using the task.TaskExecution
// interface. It is stateless per reconcile — each call re-deserializes
// the current task from the plan's embedded params and calls Status/Execute.
//
// BuildSidecarClient is a factory that produces a SidecarClient for the
// given node. Only called for sidecar-driven tasks (not controller-side).
type Executor struct {
	Client             client.Client
	Scheme             *runtime.Scheme
	Platform           platform.Config
	ObjectStore        platform.ObjectStore
	BuildSidecarClient func(node *seiv1alpha1.SeiNode) (task.SidecarClient, error)
}

// CurrentTask returns the first non-Complete task in the plan, or nil if all
// tasks are complete.
func CurrentTask(plan *seiv1alpha1.TaskPlan) *seiv1alpha1.PlannedTask {
	for i := range plan.Tasks {
		if plan.Tasks[i].Status != seiv1alpha1.PlannedTaskComplete {
			return &plan.Tasks[i]
		}
	}
	return nil
}

// ExecutePlan drives a TaskPlan one step forward for a SeiNode. This is the
// node-specific convenience wrapper around executePlan.
func (e *Executor) ExecutePlan(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	plan *seiv1alpha1.TaskPlan,
) (ctrl.Result, error) {
	if plan == nil {
		return ctrl.Result{}, fmt.Errorf("ExecutePlan called with nil plan for node %s/%s", node.Namespace, node.Name)
	}

	cfg := task.ExecutionConfig{
		BuildSidecarClient: func() (task.SidecarClient, error) {
			return e.BuildSidecarClient(node)
		},
		KubeClient:  e.Client,
		Scheme:      e.Scheme,
		Node:        node,
		Platform:    e.Platform,
		ObjectStore: e.ObjectStore,
	}

	return e.executePlan(ctx, node, plan, cfg)
}

// ExecuteGroupPlan drives a TaskPlan one step forward for a SeiNodeGroup.
// The assemblerNode is used to build the sidecar client for sidecar tasks.
func (e *Executor) ExecuteGroupPlan(
	ctx context.Context,
	group *seiv1alpha1.SeiNodeGroup,
	plan *seiv1alpha1.TaskPlan,
	assemblerNode *seiv1alpha1.SeiNode,
) (ctrl.Result, error) {
	if plan == nil {
		return ctrl.Result{}, fmt.Errorf("ExecuteGroupPlan called with nil plan for group %s/%s", group.Namespace, group.Name)
	}

	cfg := task.ExecutionConfig{
		BuildSidecarClient: func() (task.SidecarClient, error) {
			return e.BuildSidecarClient(assemblerNode)
		},
		KubeClient:  e.Client,
		Scheme:      e.Scheme,
		Node:        assemblerNode,
		Platform:    e.Platform,
		ObjectStore: e.ObjectStore,
	}

	return e.executePlan(ctx, group, plan, cfg)
}

// executePlan is the core plan execution loop, generic over the status-bearing
// resource type. It deserializes the current task, polls/submits it, and
// patches the owning object's status.
func (e *Executor) executePlan(
	ctx context.Context,
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
		if err := e.Client.Status().Patch(ctx, obj, patch); err != nil {
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
		return e.failTask(ctx, obj, plan, t, err.Error())
	}

	status := exec.Status(ctx)

	if status == task.ExecutionRunning && t.Status == seiv1alpha1.PlannedTaskPending {
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
		t.Status = seiv1alpha1.PlannedTaskComplete
		if err := e.Client.Status().Patch(ctx, obj, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking task complete: %w", err)
		}
		return ResultRequeueImmediate, nil

	case task.ExecutionFailed:
		errMsg := "unknown error"
		if exec.Err() != nil {
			errMsg = exec.Err().Error()
		}
		if t.MaxRetries > 0 && t.RetryCount < t.MaxRetries {
			return e.retryTask(ctx, obj, plan, t, cfg, errMsg)
		}
		return e.failTask(ctx, obj, plan, t, errMsg)

	default:
		return ctrl.Result{RequeueAfter: TaskPollInterval}, nil
	}
}

// retryTask resets a failed task for another attempt with a new deterministic
// ID, so the sidecar treats it as a fresh submission.
func (e *Executor) retryTask(
	ctx context.Context,
	obj client.Object,
	_ *seiv1alpha1.TaskPlan,
	t *seiv1alpha1.PlannedTask,
	cfg task.ExecutionConfig,
	errMsg string,
) (ctrl.Result, error) {
	patch := client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})

	t.RetryCount++
	log.FromContext(ctx).Info("retrying failed task",
		"task", t.Type, "attempt", t.RetryCount, "maxRetries", t.MaxRetries, "lastError", errMsg)

	nodeName := ""
	if cfg.Node != nil {
		nodeName = cfg.Node.Name
	}
	t.ID = task.DeterministicTaskID(nodeName, t.Type, t.RetryCount)
	t.Status = seiv1alpha1.PlannedTaskPending
	t.Error = ""

	if err := e.Client.Status().Patch(ctx, obj, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("resetting task for retry: %w", err)
	}

	backoff := retryBackoff(t.RetryCount)
	return ctrl.Result{RequeueAfter: backoff}, nil
}

// failTask marks an individual task and the overall plan as Failed.
func (e *Executor) failTask(
	ctx context.Context,
	obj client.Object,
	plan *seiv1alpha1.TaskPlan,
	t *seiv1alpha1.PlannedTask,
	errMsg string,
) (ctrl.Result, error) {
	log.FromContext(ctx).Error(fmt.Errorf("task failed: %s", errMsg), "task plan failed", "task", t.Type)

	patch := client.MergeFromWithOptions(obj.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})
	t.Status = seiv1alpha1.PlannedTaskFailed
	t.Error = errMsg
	plan.Phase = seiv1alpha1.TaskPlanFailed
	if err := e.Client.Status().Patch(ctx, obj, patch); err != nil {
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
