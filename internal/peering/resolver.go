// Package peering resolves a SeiNode's spec.peers into the fully-composed
// `<node_id>@<host>:<port>` persistent_peers set (and the in-cluster RPC
// witness endpoints for state sync). It collapses the two plan-path peer
// builders that previously duplicated this work: the planner's discoverPeersTask
// and the resolution the sidecar DiscoverPeers task performed.
//
// The imperative path — SeiNodeTask kind=DiscoverPeers (task.discoverPeersParams)
// — still routes source-building to the sidecar and is the deferred 3-PR tail;
// it is not collapsed here yet.
//
// Resolve handles all three source kinds in-controller:
//   - Label  — list matching SeiNodes, fetch each peer's node_id via its
//     sidecar, compose against the peer's external or in-cluster P2P address.
//   - Static — verbatim, already `<node_id>@<host>:<port>`.
//   - EC2Tags — DescribeInstances by tag filters, compose each instance's
//     P2P address from its node_id tag + IP/DNS.
//
// Resolution is level-triggered and idempotent: it preserves the prior
// resolved entry for a peer whose node_id momentarily can't be fetched, so a
// peer mid-restart does not churn the whole fleet's persistent_peers.
package peering

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

// errNoSidecarFactory marks a nil BuildSidecarClient: peer resolution treats it
// as a transient per-peer failure (preserve-prior or skip), never a panic, so a
// degraded or test environment without a sidecar factory still reconciles.
var errNoSidecarFactory = errors.New("sidecar client factory is nil")

// Resolver resolves spec.peers into persistent_peers + RPC witnesses.
// All dependencies are injected so the resolver is unit-testable without a
// live cluster or AWS account.
type Resolver struct {
	// Reader lists SeiNodes for Label sources.
	Reader client.Reader

	// BuildSidecarClient returns a sidecar client for a peer SeiNode, used
	// to fetch its Tendermint node_id. Nil is tolerated (treated as a
	// transient failure) so tests and degraded environments don't panic.
	BuildSidecarClient func(node *seiv1alpha1.SeiNode) (task.SidecarClient, error)

	// EC2 resolves EC2Tags sources. Nil means EC2 resolution is unavailable;
	// an EC2Tags source declared while EC2 is nil is a transient failure
	// (preserve prior / skip), matching the Label-path stability rule.
	EC2 EC2Resolver
}

// Result is the resolved peering state for a SeiNode.
type Result struct {
	// Peers are the fully-composed `<node_id>@<host>:<port>` persistent_peers,
	// sorted and de-duplicated.
	Peers []string
	// Witnesses are the in-cluster RPC endpoints of label-resolved peers,
	// sorted and de-duplicated, for state-sync light-client use.
	Witnesses []string
}

// Resolve resolves all of node.Spec.Peers. prior is the node's last resolved
// peer set (node.Status.ResolvedPeers); it is consulted to preserve a peer's
// composed entry when its node_id can't be fetched this reconcile. Resolve
// never returns an error for a per-peer transient failure — it preserves or
// skips — so it only errors on a hard failure (e.g. a List call).
func (r *Resolver) Resolve(ctx context.Context, node *seiv1alpha1.SeiNode, prior []string) (Result, error) {
	priorByHost := indexResolvedPeersByHost(prior)

	var peers, witnesses []string
	for i := range node.Spec.Peers {
		src := &node.Spec.Peers[i]
		switch {
		case src.Label != nil:
			endpoints, w, err := r.resolveLabel(ctx, node, src.Label, priorByHost)
			if err != nil {
				return Result{}, err
			}
			peers = append(peers, endpoints...)
			witnesses = append(witnesses, w...)
		case src.Static != nil:
			peers = append(peers, src.Static.Addresses...)
		case src.EC2Tags != nil:
			endpoints, preservePrior := r.resolveEC2(ctx, src.EC2Tags)
			if preservePrior {
				// A transient EC2 failure preserves the node's prior resolved
				// peer set wholesale (see resolveEC2) so this reconcile does
				// not churn persistent_peers. Witnesses are deterministic from
				// peer identity and re-derived every reconcile, so keep the set
				// gathered so far rather than wiping ResolvedRPCWitnesses.
				slices.Sort(witnesses)
				return Result{Peers: prior, Witnesses: slices.Compact(witnesses)}, nil
			}
			peers = append(peers, endpoints...)
		}
	}

	slices.Sort(peers)
	peers = slices.Compact(peers)
	slices.Sort(witnesses)
	witnesses = slices.Compact(witnesses)
	return Result{Peers: peers, Witnesses: witnesses}, nil
}

// resolveLabel lists SeiNodes matching the selector, composes each peer's
// `<node_id>@<host>:<port>` entry, and yields the in-cluster RPC witness for
// each matched peer. A per-peer node_id fetch failure preserves the prior
// composed entry (from priorByHost) or skips the peer until it is resolvable —
// transients never wedge the fleet. Witnesses are deterministic from peer
// identity (no node_id needed) so every matched peer yields one regardless of
// sidecar reachability.
func (r *Resolver) resolveLabel(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	src *seiv1alpha1.LabelPeerSource,
	priorByHost map[string]string,
) ([]string, []string, error) {
	logger := log.FromContext(ctx)
	ns := node.Namespace
	if src.Namespace != "" {
		ns = src.Namespace
	}

	var nodeList seiv1alpha1.SeiNodeList
	if err := r.Reader.List(ctx, &nodeList,
		client.InNamespace(ns),
		client.MatchingLabels(src.Selector),
	); err != nil {
		return nil, nil, fmt.Errorf("listing peers by label: %w", err)
	}

	var endpoints, witnesses []string
	for i := range nodeList.Items {
		peer := &nodeList.Items[i]
		if peer.Name == node.Name && peer.Namespace == node.Namespace {
			continue
		}

		witnesses = append(witnesses, peerRPCAddress(peer))

		address := peerAddress(peer)
		var sc task.SidecarClient
		err := errNoSidecarFactory
		if r.BuildSidecarClient != nil {
			sc, err = r.BuildSidecarClient(peer)
		}
		if err == nil {
			var nodeID string
			nodeID, err = sc.GetNodeID(ctx)
			if err == nil {
				endpoints = append(endpoints, fmt.Sprintf("%s@%s", nodeID, address))
				continue
			}
		}
		if existing, ok := priorByHost[address]; ok {
			logger.Info("preserving prior peer entry; node_id fetch failed", "peer", peer.Name, "err", err)
			endpoints = append(endpoints, existing)
			continue
		}
		logger.Info("skipping peer until node_id is resolvable", "peer", peer.Name, "err", err)
	}
	return endpoints, witnesses, nil
}

// indexResolvedPeersByHost maps `host:port` → `<node_id>@host:port` for O(1)
// lookup of the prior composed entry on transient failure.
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

// peerRPCAddress returns the in-cluster headless Service DNS for a peer's RPC
// port. Unlike peerAddress it never consults Spec.ExternalAddress: the external
// NLB exposes P2P only, so a state-sync light-client witness must target the
// cluster-internal RPC endpoint or seid exits on "no witnesses connected".
func peerRPCAddress(peer *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("%s-0.%s.%s.svc.cluster.local:%d",
		peer.Name, peer.Name, peer.Namespace, seiconfig.PortRPC)
}
