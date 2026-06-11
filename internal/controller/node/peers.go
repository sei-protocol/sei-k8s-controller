package node

import (
	"context"
	"fmt"
	"slices"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/peering"
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
	return nil
}
