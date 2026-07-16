package node

import (
	"context"
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
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

// shouldHoldInitialStatefulSet reports whether initial StatefulSet creation must
// wait for the state-sync gate to open. The init plan's EnsureDataPVC task is the
// only creator of the data PVC the pod mounts by claimName, so an STS created
// while the gate suppresses that plan would strand a Pending pod until the gate
// opens. Only initial creation is held (Status.StatefulSet == nil): an existing
// STS is never touched, and StateSyncBlocksPlan is pre-Running-only, so a Running
// node's STS keeps syncing through transient syncer-source errors. One narrow
// stall is accepted: if the impostor branch just cleared Status.StatefulSet and
// the gate is blocked in the same window, STS re-creation waits for the gate's
// poll to re-resolve.
func shouldHoldInitialStatefulSet(node *seiv1alpha1.SeiNode) bool {
	return planner.StateSyncBlocksPlan(node) && node.Status.StatefulSet == nil
}
