package node

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	taskSnapshotRestore    = sidecar.TaskTypeSnapshotRestore
	taskDiscoverPeers      = sidecar.TaskTypeDiscoverPeers
	taskConfigureGenesis   = sidecar.TaskTypeConfigureGenesis
	taskConfigureStateSync = sidecar.TaskTypeConfigureStateSync
	taskConfigApply        = sidecar.TaskTypeConfigApply
	taskConfigValidate     = sidecar.TaskTypeConfigValidate
	taskMarkReady          = sidecar.TaskTypeMarkReady
	taskSnapshotUpload     = sidecar.TaskTypeSnapshotUpload
	taskResultExport       = sidecar.TaskTypeResultExport

	bootstrapPollInterval = 5 * time.Second
)

// SidecarStatusClient abstracts the sidecar HTTP API for testability.
type SidecarStatusClient interface {
	SubmitTask(ctx context.Context, task sidecar.TaskRequest) (uuid.UUID, error)
	GetTask(ctx context.Context, id uuid.UUID) (*sidecar.TaskResult, error)
}

// insertBefore inserts task into prog immediately before target, unless task
// is already present. Returns the (possibly grown) slice.
func insertBefore(prog []string, target, task string) []string {
	if slices.Contains(prog, task) {
		return prog
	}
	for i, t := range prog {
		if t == target {
			return slices.Insert(prog, i, task)
		}
	}
	return prog
}

// buildSidecarClient constructs a SidecarClient from the node's sidecar config.
func (r *SeiNodeReconciler) buildSidecarClient(node *seiv1alpha1.SeiNode) SidecarStatusClient {
	if r.BuildSidecarClientFn != nil {
		return r.BuildSidecarClientFn(node)
	}
	c, err := sidecar.NewSidecarClientFromPodDNS(node.Name, node.Namespace, sidecarPort(node))
	if err != nil {
		return nil
	}
	return c
}

// currentTask returns the first non-Complete task in the plan, or nil if all
// tasks are complete.
func currentTask(plan *seiv1alpha1.TaskPlan) *seiv1alpha1.PlannedTask {
	for i := range plan.Tasks {
		if plan.Tasks[i].Status != seiv1alpha1.PlannedTaskComplete {
			return &plan.Tasks[i]
		}
	}
	return nil
}

// reconcileSidecarProgression drives the TaskPlan through to completion.
func (r *SeiNodeReconciler) reconcileSidecarProgression(ctx context.Context, node *seiv1alpha1.SeiNode, planner NodePlanner) (ctrl.Result, error) {
	sc := r.buildSidecarClient(node)

	// Build the plan on first encounter.
	if node.Status.InitPlan == nil {
		plan := planner.BuildPlan(node)
		patch := client.MergeFrom(node.DeepCopy())
		node.Status.InitPlan = plan
		if err := r.Status().Patch(ctx, node, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("initializing task plan: %w", err)
		}
	}

	plan := node.Status.InitPlan

	// Terminal states — nothing to do.
	switch plan.Phase {
	case seiv1alpha1.TaskPlanComplete:
		return r.reconcileRuntimeTasks(ctx, node, sc)
	case seiv1alpha1.TaskPlanFailed:
		return ctrl.Result{}, nil
	}

	task := currentTask(plan)
	if task == nil {
		return r.markPlanComplete(ctx, node)
	}

	switch task.Status {
	case seiv1alpha1.PlannedTaskPending:
		return r.submitTask(ctx, node, sc, planner, task)

	case seiv1alpha1.PlannedTaskSubmitted:
		return r.pollTask(ctx, node, sc, task)

	default:
		return ctrl.Result{RequeueAfter: bootstrapPollInterval}, nil
	}
}

// submitTask submits a task to the sidecar and records the UUID in the plan.
func (r *SeiNodeReconciler) submitTask(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	sc SidecarStatusClient,
	planner NodePlanner,
	task *seiv1alpha1.PlannedTask,
) (ctrl.Result, error) {
	builder := planner.BuildTask(node, task.Type)
	req := builder.ToTaskRequest()
	id, err := sc.SubmitTask(ctx, req)
	if err != nil {
		log.FromContext(ctx).Info("task submission failed, will retry", "task", task.Type, "error", err)
		return ctrl.Result{RequeueAfter: bootstrapPollInterval}, nil
	}

	patch := client.MergeFrom(node.DeepCopy())
	task.TaskID = id.String()
	task.Status = seiv1alpha1.PlannedTaskSubmitted
	if err := r.Status().Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("recording submitted task: %w", err)
	}
	return ctrl.Result{RequeueAfter: bootstrapPollInterval}, nil
}

// pollTask checks the result of a submitted task via GetTask.
func (r *SeiNodeReconciler) pollTask(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	sc SidecarStatusClient,
	task *seiv1alpha1.PlannedTask,
) (ctrl.Result, error) {
	taskID, err := uuid.Parse(task.TaskID)
	if err != nil {
		return r.failTask(ctx, node, task, fmt.Sprintf("invalid task UUID %q", task.TaskID))
	}

	result, err := sc.GetTask(ctx, taskID)
	if err != nil {
		log.FromContext(ctx).Info("failed to poll task, will retry", "task", task.Type, "error", err)
		return ctrl.Result{RequeueAfter: bootstrapPollInterval}, nil
	}

	switch result.Status {
	case sidecar.Running:
		log.FromContext(ctx).V(1).Info("task still running", "task", task.Type)
		return ctrl.Result{RequeueAfter: bootstrapPollInterval}, nil

	case sidecar.Failed:
		errMsg := "unknown error"
		if result.Error != nil && *result.Error != "" {
			errMsg = *result.Error
		}
		return r.failTask(ctx, node, task, errMsg)

	case sidecar.Completed:
		patch := client.MergeFrom(node.DeepCopy())
		task.Status = seiv1alpha1.PlannedTaskComplete
		if err := r.Status().Patch(ctx, node, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking task complete: %w", err)
		}
		return ctrl.Result{RequeueAfter: 1}, nil

	default:
		log.FromContext(ctx).Info("unexpected task status, will retry", "task", task.Type, "status", result.Status)
		return ctrl.Result{RequeueAfter: bootstrapPollInterval}, nil
	}
}

// failTask marks an individual task and the overall plan as Failed.
func (r *SeiNodeReconciler) failTask(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	task *seiv1alpha1.PlannedTask,
	errMsg string,
) (ctrl.Result, error) {
	log.FromContext(ctx).Error(fmt.Errorf("task failed: %s", errMsg), "task plan failed", "task", task.Type)

	patch := client.MergeFrom(node.DeepCopy())
	task.Status = seiv1alpha1.PlannedTaskFailed
	task.Error = errMsg
	node.Status.InitPlan.Phase = seiv1alpha1.TaskPlanFailed
	if err := r.Status().Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("marking task failed: %w", err)
	}
	return ctrl.Result{}, nil
}

// markPlanComplete transitions the plan to Complete.
func (r *SeiNodeReconciler) markPlanComplete(ctx context.Context, node *seiv1alpha1.SeiNode) (ctrl.Result, error) {
	patch := client.MergeFrom(node.DeepCopy())
	node.Status.InitPlan.Phase = seiv1alpha1.TaskPlanComplete
	if err := r.Status().Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("marking plan complete: %w", err)
	}
	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}

// reconcileRuntimeTasks ensures all scheduled tasks are submitted exactly
// once. The sidecar owns execution cadence after that.
func (r *SeiNodeReconciler) reconcileRuntimeTasks(ctx context.Context, node *seiv1alpha1.SeiNode, sc SidecarStatusClient) (ctrl.Result, error) {
	if builder := snapshotUploadScheduledTask(node); builder != nil {
		req := builder.ToTaskRequest()
		if err := r.ensureScheduledTask(ctx, node, sc, req); err != nil {
			log.FromContext(ctx).Info("scheduled task submission failed, will retry", "task", req.Type, "error", err)
		}
	}
	if builder := resultExportScheduledTask(node); builder != nil {
		req := builder.ToTaskRequest()
		if err := r.ensureScheduledTask(ctx, node, sc, req); err != nil {
			log.FromContext(ctx).Info("scheduled task submission failed, will retry", "task", req.Type, "error", err)
		}
	}
	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}

func (r *SeiNodeReconciler) ensureScheduledTask(ctx context.Context, node *seiv1alpha1.SeiNode, sc SidecarStatusClient, req sidecar.TaskRequest) error {
	if node.Status.ScheduledTasks != nil {
		if _, ok := node.Status.ScheduledTasks[req.Type]; ok {
			return nil
		}
	}

	id, err := sc.SubmitTask(ctx, req)
	if err != nil {
		return err
	}

	patch := client.MergeFrom(node.DeepCopy())
	if node.Status.ScheduledTasks == nil {
		node.Status.ScheduledTasks = make(map[string]string)
	}
	node.Status.ScheduledTasks[req.Type] = id.String()
	return r.Status().Patch(ctx, node, patch)
}
