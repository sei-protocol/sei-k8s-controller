package node

import (
	"context"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

// reconcileRuntimeTasks ensures all scheduled and monitor tasks are submitted
// exactly once, and polls monitor tasks for completion on each reconcile.
func (r *SeiNodeReconciler) reconcileRuntimeTasks(ctx context.Context, node *seiv1alpha1.SeiNode, sc task.SidecarClient) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Snapshot upload: always a scheduled (cron) task.
	if builder := planner.SnapshotUploadScheduledTask(node); builder != nil {
		req := builder.ToTaskRequest()
		if err := r.ensureScheduledTask(ctx, node, sc, req); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			logger.Info("scheduled task submission failed, will retry", "task", req.Type, "error", err)
		}
	}

	// Result export: monitor task that compares block results against
	// the canonical chain and completes on app-hash divergence.
	if err := r.ensureMonitorTasks(ctx, node, sc); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		logger.Info("monitor task submission failed, will retry", "error", err)
	}

	// Poll all active monitor tasks for completion.
	requeue, err := r.pollMonitorTasks(ctx, node, sc)
	if err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		logger.Info("monitor task poll failed, will retry", "error", err)
	}
	if requeue {
		return ctrl.Result{Requeue: true}, nil
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
