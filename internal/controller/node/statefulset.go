package node

import (
	"context"
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

// reconcileStatefulSet syncs the owned StatefulSet via the typed
// Get+Create/Update helper and records the resulting object on
// Status.StatefulSet so subsequent reconciles fetch the tracked
// identity directly.
//
// A nil StatefulSet with no error means the impostor branch fired:
// SyncStatefulSet detected a UID mismatch, issued Delete, and deferred
// the Apply to the next reconcile. Leave Status.StatefulSet untouched —
// the stale UID still points at a now-deleted object, and the next
// reconcile (triggered by the StatefulSet delete watch event) observes
// NotFound and Applies a fresh STS whose new UID gets stamped onto
// Status.StatefulSet.
func (r *SeiNodeReconciler) reconcileStatefulSet(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	sts, err := noderesource.SyncStatefulSet(ctx, r.Client, r.Scheme, node, r.Platform)
	if err != nil {
		return fmt.Errorf("syncing statefulset: %w", err)
	}
	if sts == nil {
		return nil
	}
	if node.Status.StatefulSet == nil ||
		node.Status.StatefulSet.UID != sts.UID ||
		node.Status.StatefulSet.Name != sts.Name {
		node.Status.StatefulSet = &seiv1alpha1.StatefulSetRef{
			Name: sts.Name,
			UID:  sts.UID,
		}
	}
	return nil
}
