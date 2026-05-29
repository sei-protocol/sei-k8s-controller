package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	seiconfig "github.com/sei-protocol/sei-config"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// errNoSidecarFactory matches the planner's documented "nil factory" contract
// (see planner.NodeResolver). resolveLabelPeers treats it as a transient
// per-peer failure rather than panicking.
var errNoSidecarFactory = errors.New("sidecar client factory is nil")

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

// resolveLabelPeers returns fully-composed `<node_id>@<host>:<port>`
// strings for SeiNodes matching the selector. Per-peer sidecar failures
// preserve the prior entry from Status.ResolvedPeers (so transients
// don't wedge fleet-wide reconciles) or skip with a log line.
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
		var (
			sc  task.SidecarClient
			err error
		)
		if r.Planner.BuildSidecarClient == nil {
			err = errNoSidecarFactory
		} else {
			sc, err = r.Planner.BuildSidecarClient(peer)
		}
		if err == nil {
			var nodeID string
			nodeID, err = sc.GetNodeID(ctx)
			if err == nil {
				endpoints = append(endpoints, fmt.Sprintf("%s@%s", nodeID, address))
				continue
			}
		}
		if existing, ok := prior[address]; ok {
			logger.Info("preserving prior peer entry; node_id fetch failed", "peer", peer.Name, "err", err)
			endpoints = append(endpoints, existing)
			continue
		}
		logger.Info("skipping peer until node_id is resolvable", "peer", peer.Name, "err", err)
	}
	return endpoints, nil
}

// indexResolvedPeersByHost maps `host:port` → `<node_id>@host:port` for
// O(1) lookup of the prior composed entry on transient failure.
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

// peerAddress returns Spec.ExternalAddress (already host:port) when set,
// otherwise the headless Service DNS at the standard P2P port.
func peerAddress(peer *seiv1alpha1.SeiNode) string {
	if peer.Spec.ExternalAddress != "" {
		return peer.Spec.ExternalAddress
	}
	return fmt.Sprintf("%s-0.%s.%s.svc.cluster.local:%d",
		peer.Name, peer.Name, peer.Namespace, seiconfig.PortP2P)
}
