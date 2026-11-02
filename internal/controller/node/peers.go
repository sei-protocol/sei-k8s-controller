package node

import (
	"context"
	"fmt"
	"slices"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/peering"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

// reconcilePeers resolves spec.peers into status.resolvedPeers (the composed
// persistent_peers set). The plan plumbs the resolved set into config via the
// config-apply override (init path) or the config-patch (running path).
//
// State-sync witnesses are no longer derived here from label-matched peers;
// they come from the controller-level canonical-syncer ConfigMap via the
// StateSyncReady gate (see statesync.go).
func (r *SeiNodeReconciler) reconcilePeers(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	resolver := peering.Resolver{
		Reader:             r.Client,
		BuildSidecarClient: r.Planner.BuildSidecarClient,
		EC2:                r.EC2Peers,
	}
	result, err := resolver.Resolve(ctx, node, node.Status.ResolvedPeers)
	if err != nil {
		return fmt.Errorf("resolving peers: %w", err)
	}

	if !slices.Equal(node.Status.ResolvedPeers, result.Peers) {
		node.Status.ResolvedPeers = result.Peers
	}
	setPeersReady(node, time.Now())
	return nil
}

// setPeersReady sets the always-present PeersReady condition from the node's
// resolved peers and the planner's peer gate.
func setPeersReady(node *seiv1alpha1.SeiNode, now time.Time) {
	status, reason, message := metav1.ConditionFalse, seiv1alpha1.ReasonPeersUnresolved,
		"no peer resolved; seid runs without persistent peers until the next deployment"
	switch {
	case len(node.Spec.Peers) == 0:
		reason, message = seiv1alpha1.ReasonPeersNotApplicable, "spec.peers is empty"
	case len(node.Status.ResolvedPeers) > 0:
		status, reason = metav1.ConditionTrue, seiv1alpha1.ReasonPeersResolved
		message = fmt.Sprintf("%d persistent peers resolved", len(node.Status.ResolvedPeers))
	case planner.PeersBlockPlan(node, now):
		reason = seiv1alpha1.ReasonPeersAwaiting
		message = fmt.Sprintf("no label peer resolved yet; init plan held for up to %s after creation",
			planner.PeerResolutionWait)
	}
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionPeersReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}
