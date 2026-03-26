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
	ImmediateRequeue = time.Millisecond
)

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

// ExecutePlan drives a TaskPlan one step forward. The plan pointer must
// reference a field inside node.Status so that status patches capture
// mutations. Returns (result, error) suitable for the reconciler.
func (e *Executor) ExecutePlan(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	plan *seiv1alpha1.TaskPlan,
) (ctrl.Result, error) {
	if plan == nil {
		return ctrl.Result{}, fmt.Errorf("ExecutePlan called with nil plan for node %s/%s", node.Namespace, node.Name)
	}

	switch plan.Phase {
	case seiv1alpha1.TaskPlanComplete, seiv1alpha1.TaskPlanFailed:
		return ctrl.Result{}, nil
	}

	t := CurrentTask(plan)
	if t == nil {
		patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
		plan.Phase = seiv1alpha1.TaskPlanComplete
		if err := e.Client.Status().Patch(ctx, node, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking plan complete: %w", err)
		}
		return ctrl.Result{RequeueAfter: ImmediateRequeue}, nil
	}

	cfg := task.ExecutionConfig{
		BuildSidecarClient: func() (task.SidecarClient, error) {
			return e.BuildSidecarClient(node)
		},
		KubeClient: e.Client,
		Scheme:     e.Scheme,
		Node:       node,
		Platform:   e.Platform,
	}

	var paramsRaw []byte
	if t.Params != nil {
		paramsRaw = t.Params.Raw
	}

	exec, err := task.Deserialize(t.Type, t.ID, paramsRaw, cfg)
	if err != nil {
		return e.failTask(ctx, node, plan, t, err.Error())
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
		patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
		t.Status = seiv1alpha1.PlannedTaskComplete
		if err := e.Client.Status().Patch(ctx, node, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking task complete: %w", err)
		}
		return ctrl.Result{RequeueAfter: ImmediateRequeue}, nil

	case task.ExecutionFailed:
		errMsg := "unknown error"
		if exec.Err() != nil {
			errMsg = exec.Err().Error()
		}
		return e.failTask(ctx, node, plan, t, errMsg)

	default:
		return ctrl.Result{RequeueAfter: TaskPollInterval}, nil
	}
}

// failTask marks an individual task and the overall plan as Failed.
func (e *Executor) failTask(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	plan *seiv1alpha1.TaskPlan,
	t *seiv1alpha1.PlannedTask,
	errMsg string,
) (ctrl.Result, error) {
	log.FromContext(ctx).Error(fmt.Errorf("task failed: %s", errMsg), "task plan failed", "task", t.Type)

	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	t.Status = seiv1alpha1.PlannedTaskFailed
	t.Error = errMsg
	plan.Phase = seiv1alpha1.TaskPlanFailed
	if err := e.Client.Status().Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("marking task failed: %w", err)
	}
	return ctrl.Result{}, nil
}
