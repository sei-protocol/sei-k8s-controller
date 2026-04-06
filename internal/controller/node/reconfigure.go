package node

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	ConditionPeerUpdateNeeded = "PeerUpdateNeeded"
	ReasonPeerSpecChanged     = "PeerSpecChanged"
	ReasonPeerUpdateFailed    = "PeerUpdateFailed"
)

// detectPeerDrift checks whether the node's peer spec has changed since
// the last fully reconciled generation. When drift is detected on a
// Running node with no active plan, the PeerUpdateNeeded condition is
// set, signaling reconcileRunning to build a plan via the node planner.
func (r *SeiNodeReconciler) detectPeerDrift(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	if node.Status.Phase != seiv1alpha1.PhaseRunning {
		return nil
	}
	if node.Status.Plan != nil {
		return nil
	}
	if node.Generation == node.Status.ObservedGeneration {
		return nil
	}

	// Generation changed — check if peers are part of the diff.
	// We consider any non-empty peers slice as potentially drifted
	// when the generation has advanced past what we last reconciled.
	// The planner will determine the exact tasks needed.
	if len(node.Spec.Peers) == 0 {
		return nil
	}

	if hasNodeCondition(node, ConditionPeerUpdateNeeded) {
		return nil // already flagged, waiting for plan
	}

	log.FromContext(ctx).Info("peer spec changed on running node",
		"generation", node.Generation,
		"observedGeneration", node.Status.ObservedGeneration)

	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               ConditionPeerUpdateNeeded,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonPeerSpecChanged,
		Message:            "Peer configuration has changed",
		ObservedGeneration: node.Generation,
	})
	return r.Status().Patch(ctx, node, patch)
}

// hasNodeCondition returns true if the named condition is True.
func hasNodeCondition(node *seiv1alpha1.SeiNode, condType string) bool {
	c := meta.FindStatusCondition(node.Status.Conditions, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}
