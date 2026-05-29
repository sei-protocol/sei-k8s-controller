package node

import (
	"context"
	"fmt"
	"slices"

	seiconfig "github.com/sei-protocol/sei-config"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func (r *SeiNodeReconciler) reconcilePeers(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	var resolved []string
	for _, src := range node.Spec.Peers {
		if src.Label == nil {
			continue
		}
		endpoints, err := r.resolveLabelPeers(ctx, node, src.Label)
		if err != nil {
			return err
		}
		resolved = append(resolved, endpoints...)
	}

	slices.Sort(resolved)
	resolved = slices.Compact(resolved)

	if !slices.Equal(node.Status.ResolvedPeers, resolved) {
		node.Status.ResolvedPeers = resolved
	}
	return nil
}

// resolveLabelPeers lists SeiNodes matching the selector and returns
// fully-composed `<node_id>@<host>:<port>` strings. The host is each
// peer's Spec.ExternalAddress when set, otherwise the headless Service
// DNS. node_id is fetched from each peer's sidecar via gRPC — the same
// in-cluster path the genesis CollectAndSetPeers task uses.
func (r *SeiNodeReconciler) resolveLabelPeers(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	src *seiv1alpha1.LabelPeerSource,
) ([]string, error) {
	ns := node.Namespace
	if src.Namespace != "" {
		ns = src.Namespace
	}

	var nodeList seiv1alpha1.SeiNodeList
	if err := r.List(ctx, &nodeList,
		client.InNamespace(ns),
		client.MatchingLabels(src.Selector),
	); err != nil {
		return nil, fmt.Errorf("listing peers by label: %w", err)
	}

	var endpoints []string
	for i := range nodeList.Items {
		peer := &nodeList.Items[i]
		if peer.Name == node.Name && peer.Namespace == node.Namespace {
			continue
		}

		sc, err := r.Planner.BuildSidecarClient(peer)
		if err != nil {
			return nil, fmt.Errorf("building sidecar client for peer %s: %w", peer.Name, err)
		}
		nodeID, err := sc.GetNodeID(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetching node_id for peer %s: %w", peer.Name, err)
		}

		endpoints = append(endpoints, fmt.Sprintf("%s@%s", nodeID, peerAddress(peer)))
	}
	return endpoints, nil
}

// peerAddress is the dial address for a peer: Spec.ExternalAddress (already
// host:port from the SND publishable path) when set, otherwise the headless
// Service DNS at the standard P2P port.
func peerAddress(peer *seiv1alpha1.SeiNode) string {
	if peer.Spec.ExternalAddress != "" {
		return peer.Spec.ExternalAddress
	}
	return fmt.Sprintf("%s-0.%s.%s.svc.cluster.local:%d",
		peer.Name, peer.Name, peer.Namespace, seiconfig.PortP2P)
}
