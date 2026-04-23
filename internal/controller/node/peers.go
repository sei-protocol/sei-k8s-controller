package node

import (
	"context"
	"fmt"
	"slices"

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

// resolveLabelPeers lists SeiNode resources matching the label selector
// and returns their stable headless Service DNS hostnames.
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
		dns := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local",
			peer.Name, peer.Name, peer.Namespace)
		endpoints = append(endpoints, dns)
	}
	return endpoints, nil
}
