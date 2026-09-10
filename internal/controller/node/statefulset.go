package node

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

// reconcileStatefulSet syncs the owned StatefulSet via the typed
// Get+Create/Update helper and records the resulting object on
// Status.StatefulSet so subsequent reconciles fetch the tracked
// identity directly.
//
// A nil StatefulSet with no error means the impostor branch fired:
// SyncStatefulSet detected a UID mismatch, issued Delete, cleared
// Status.StatefulSet, and deferred the Apply to the next reconcile.
// The next reconcile (triggered by the StatefulSet delete watch event)
// observes NotFound and Applies a fresh STS whose new UID gets stamped
// onto Status.StatefulSet below.
//
// A tracked UID that no longer matches the applied object means the
// StatefulSet the node was running on was deleted underneath a live
// SeiNode and has just been recreated. That is recorded as an Event on
// the node so the recreate reads as the controller converging on a
// SeiNode that still exists, not as the controller fighting an operator.
func (r *SeiNodeReconciler) reconcileStatefulSet(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	tracked := node.Status.StatefulSet
	sts, err := noderesource.SyncStatefulSet(ctx, r.Client, r.Scheme, node, r.Platform)
	if err != nil {
		return fmt.Errorf("syncing statefulset: %w", err)
	}
	if sts == nil {
		return nil
	}
	if tracked != nil && tracked.UID != sts.UID {
		r.Recorder.Eventf(node, corev1.EventTypeNormal, "StatefulSetRecreated",
			"Recreated StatefulSet %s (previous uid %s was deleted) because SeiNode %s still exists; delete the SeiNode to remove its workload",
			sts.Name, tracked.UID, node.Name)
	}
	if tracked == nil || tracked.UID != sts.UID || tracked.Name != sts.Name {
		node.Status.StatefulSet = &seiv1alpha1.StatefulSetRef{
			Name: sts.Name,
			UID:  sts.UID,
		}
	}
	return nil
}
