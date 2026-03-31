package node

import (
	"context"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
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

// reconcileRuntimeTasks ensures all monitor tasks are submitted exactly
// once, and polls them for completion on each reconcile.
func (r *SeiNodeReconciler) reconcileRuntimeTasks(ctx context.Context, node *seiv1alpha1.SeiNode, sc task.SidecarClient) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if err := r.ensureMonitorTasks(ctx, node, sc); err != nil {
		if apierrors.IsConflict(err) {
			return planner.ResultRequeueImmediate, nil
		}
		logger.Info("monitor task submission failed, will retry", "error", err)
	}

	requeue, err := r.pollMonitorTasks(ctx, node, sc)
	if err != nil {
		if apierrors.IsConflict(err) {
			return planner.ResultRequeueImmediate, nil
		}
		logger.Info("monitor task poll failed, will retry", "error", err)
	}
	if requeue {
		return planner.ResultRequeueImmediate, nil
	}

	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}
