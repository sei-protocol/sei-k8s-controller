package node

import (
	"context"
	"fmt"
	"slices"
	"strings"

	seiconfig "github.com/sei-protocol/sei-config"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

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
// fully-composed `<node_id>@<host>:<port>` strings. Host is each peer's
// Spec.ExternalAddress when set, otherwise the headless Service DNS.
// node_id is fetched from each peer's sidecar via gRPC.
//
// Per-peer transient failures (sidecar restarting, gRPC timeout) do
// not fail the whole resolve. The peer's prior entry from
// Status.ResolvedPeers is preserved if available; otherwise the peer
// is skipped for this cycle with a structured log line. This keeps
// fleet-wide reconciles from wedging on a single peer's sidecar churn.
func (r *SeiNodeReconciler) resolveLabelPeers(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	src *seiv1alpha1.LabelPeerSource,
) ([]string, error) {
	logger := log.FromContext(ctx)
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

	prior := indexResolvedPeersByHost(node.Status.ResolvedPeers)
	var endpoints []string
	for i := range nodeList.Items {
		peer := &nodeList.Items[i]
		if peer.Name == node.Name && peer.Namespace == node.Namespace {
			continue
		}

		address := peerAddress(peer)
		sc, err := r.Planner.BuildSidecarClient(peer)
		if err == nil {
			var nodeID string
			nodeID, err = sc.GetNodeID(ctx)
			if err == nil {
				endpoints = append(endpoints, fmt.Sprintf("%s@%s", nodeID, address))
				continue
			}
		}
		// Per-peer failure: preserve the prior entry if we have one,
		// otherwise skip until the peer's sidecar is reachable.
		if existing, ok := prior[address]; ok {
			logger.Info("preserving prior peer entry; node_id fetch failed", "peer", peer.Name, "err", err)
			endpoints = append(endpoints, existing)
			continue
		}
		logger.Info("skipping peer until node_id is resolvable", "peer", peer.Name, "err", err)
	}
	return endpoints, nil
}

// indexResolvedPeersByHost maps the host:port portion of each composed
// peer entry back to the full string, so a transient sidecar failure
// can be papered over by reusing the prior entry.
func indexResolvedPeersByHost(peers []string) map[string]string {
	out := make(map[string]string, len(peers))
	for _, p := range peers {
		at := strings.Index(p, "@")
		if at <= 0 || at == len(p)-1 {
			continue
		}
		out[p[at+1:]] = p
	}
	return out
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
