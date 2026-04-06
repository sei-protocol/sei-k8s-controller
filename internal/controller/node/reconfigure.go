package node

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

const (
	ConditionConfigUpdateNeeded = "ConfigUpdateNeeded"
	ReasonConfigDriftDetected   = "ConfigDriftDetected"
	ReasonConfigUpdateComplete  = "ConfigUpdateComplete"
	ReasonConfigUpdateFailed    = "ConfigUpdateFailed"
)

// detectConfigDrift compares the current peer discovery params against the
// last-applied snapshot stored in status. When drift is detected on a
// Running node with no active plan, the ConfigUpdateNeeded condition is set,
// signaling reconcileRunning to build a reconfiguration plan.
func (r *SeiNodeReconciler) detectConfigDrift(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	if node.Status.Phase != seiv1alpha1.PhaseRunning {
		return nil
	}
	if node.Status.Plan != nil {
		return nil
	}

	currentPeerParams, err := MarshalPeerParams(node)
	if err != nil {
		return fmt.Errorf("marshaling current peer params: %w", err)
	}

	if peerParamsEqual(node.Status.LastAppliedPeerParams, currentPeerParams) {
		// No drift — clear the condition if it was previously set (e.g.,
		// after a manual spec revert).
		if hasNodeCondition(node, ConditionConfigUpdateNeeded) {
			patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
			meta.RemoveStatusCondition(&node.Status.Conditions, ConditionConfigUpdateNeeded)
			return r.Status().Patch(ctx, node, patch)
		}
		return nil
	}

	if hasNodeCondition(node, ConditionConfigUpdateNeeded) {
		return nil // already flagged, waiting for plan
	}

	log.FromContext(ctx).Info("config drift detected on running node")
	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               ConditionConfigUpdateNeeded,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonConfigDriftDetected,
		Message:            "Peer configuration has changed since last applied",
		ObservedGeneration: node.Generation,
	})
	return r.Status().Patch(ctx, node, patch)
}

// MarshalPeerParams builds and serializes the DiscoverPeersParams for the
// node's current spec. The serialized form is stored in status and compared
// on future reconciles to detect drift.
func MarshalPeerParams(node *seiv1alpha1.SeiNode) (*apiextensionsv1.JSON, error) {
	params := planner.BuildDiscoverPeersParams(node)
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return &apiextensionsv1.JSON{Raw: raw}, nil
}

// peerParamsEqual compares two serialized peer param snapshots.
// Both nil means equal (no peers configured).
func peerParamsEqual(a, b *apiextensionsv1.JSON) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return string(a.Raw) == string(b.Raw)
}

// hasNodeCondition returns true if the named condition is True.
func hasNodeCondition(node *seiv1alpha1.SeiNode, condType string) bool {
	c := meta.FindStatusCondition(node.Status.Conditions, condType)
	return c != nil && c.Status == metav1.ConditionTrue
}
