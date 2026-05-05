package task

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	TaskTypeCollectAndSetPeers = "collect-and-set-peers"
	defaultP2PPort             = int32(26656)
)

type CollectAndSetPeersParams struct {
	GroupName string   `json:"groupName"`
	Namespace string   `json:"namespace"`
	NodeNames []string `json:"nodeNames"`
}

type collectAndSetPeersExecution struct {
	taskBase
	params CollectAndSetPeersParams
	cfg    ExecutionConfig
}

func deserializeCollectAndSetPeers(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p CollectAndSetPeersParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing collect-and-set-peers params: %w", err)
		}
	}
	return &collectAndSetPeersExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *collectAndSetPeersExecution) Execute(ctx context.Context) error {
	peers, err := e.collectPeers(ctx)
	if err != nil {
		return fmt.Errorf("collecting peers: %w", err) // transient — sidecar may not be ready
	}

	if err := e.setPeersOnNodes(ctx, peers); err != nil {
		return fmt.Errorf("setting peers: %w", err) // transient
	}

	e.complete()
	return nil
}

func (e *collectAndSetPeersExecution) collectPeers(ctx context.Context) ([]string, error) {
	var peers []string
	for _, name := range e.params.NodeNames {
		node := &seiv1alpha1.SeiNode{}
		if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
			return nil, fmt.Errorf("getting node %s: %w", name, err)
		}

		sc, err := sidecarClientForNode(node)
		if err != nil {
			return nil, fmt.Errorf("building sidecar client for %s: %w", name, err)
		}

		nodeID, err := sc.GetNodeID(ctx)
		if err != nil {
			return nil, fmt.Errorf("getting node ID for %s: %w", name, err)
		}

		dns := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", name, name, e.params.Namespace)
		peers = append(peers, fmt.Sprintf("%s@%s:%d", nodeID, dns, defaultP2PPort))
	}
	return peers, nil
}

func (e *collectAndSetPeersExecution) setPeersOnNodes(ctx context.Context, peers []string) error {
	for _, name := range e.params.NodeNames {
		node := &seiv1alpha1.SeiNode{}
		if err := e.cfg.KubeClient.Get(ctx, types.NamespacedName{Name: name, Namespace: e.params.Namespace}, node); err != nil {
			return fmt.Errorf("getting node %s: %w", name, err)
		}
		// Spec patch (not status); CLAUDE.md's optimistic-lock invariant
		// applies to .status writes only. Idempotent under conflict.
		patch := client.MergeFrom(node.DeepCopy())
		node.Spec.Peers = []seiv1alpha1.PeerSource{
			{Static: &seiv1alpha1.StaticPeerSource{Addresses: peers}},
		}
		if err := e.cfg.KubeClient.Patch(ctx, node, patch); err != nil {
			return fmt.Errorf("setting peers on %s: %w", name, err)
		}
	}
	return nil
}

func (e *collectAndSetPeersExecution) Status(_ context.Context) ExecutionStatus {
	return e.status
}
