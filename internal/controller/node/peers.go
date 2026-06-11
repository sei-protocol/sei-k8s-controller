package node

import (
	"context"
	"fmt"
	"slices"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/peering"
)

// reconcilePeers resolves spec.peers into status.resolvedPeers (the composed
// persistent_peers set) and status.resolvedRPCWitnesses (state-sync witnesses).
// The plan plumbs the resolved set into config via the config-apply override
// (init path) or the config-patch (running path).
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
	if !slices.Equal(node.Status.ResolvedRPCWitnesses, result.Witnesses) {
		node.Status.ResolvedRPCWitnesses = result.Witnesses
	}
	return nil
}
