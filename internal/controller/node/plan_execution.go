package node

import (
	"context"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// buildSidecarClient constructs a SidecarClient from the node's sidecar config.
func (r *SeiNodeReconciler) buildSidecarClient(node *seiv1alpha1.SeiNode) task.SidecarClient {
	if r.BuildSidecarClientFn != nil {
		return r.BuildSidecarClientFn(node)
	}
	c, err := sidecar.NewSidecarClientFromPodDNS(node.Name, node.Namespace, sidecarPort(node))
	if err != nil {
		log.Log.Info("failed to build sidecar client", "node", node.Name, "error", err)
		return nil
	}
	return c
}

// reconcileRuntimeTasks ensures all scheduled tasks are submitted exactly
// once. The sidecar owns execution cadence after that.
func (r *SeiNodeReconciler) reconcileRuntimeTasks(ctx context.Context, node *seiv1alpha1.SeiNode, sc task.SidecarClient) (ctrl.Result, error) {
	if builder := planner.SnapshotUploadScheduledTask(node); builder != nil {
		req := builder.ToTaskRequest()
		if err := r.ensureScheduledTask(ctx, node, sc, req); err != nil {
			log.FromContext(ctx).Info("scheduled task submission failed, will retry", "task", req.Type, "error", err)
		}
	}
	if builder := planner.ResultExportScheduledTask(node); builder != nil {
		req := builder.ToTaskRequest()
		if err := r.ensureScheduledTask(ctx, node, sc, req); err != nil {
			log.FromContext(ctx).Info("scheduled task submission failed, will retry", "task", req.Type, "error", err)
		}
	}
	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}

// ensureScheduledTask submits a recurring task exactly once.
func (r *SeiNodeReconciler) ensureScheduledTask(ctx context.Context, node *seiv1alpha1.SeiNode, sc task.SidecarClient, req sidecar.TaskRequest) error {
	if node.Status.ScheduledTasks != nil {
		if _, ok := node.Status.ScheduledTasks[req.Type]; ok {
			return nil
		}
	}

	id, err := sc.SubmitTask(ctx, req)
	if err != nil {
		return err
	}

	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	if node.Status.ScheduledTasks == nil {
		node.Status.ScheduledTasks = make(map[string]string)
	}
	node.Status.ScheduledTasks[req.Type] = id.String()
	return r.Status().Patch(ctx, node, patch)
}
