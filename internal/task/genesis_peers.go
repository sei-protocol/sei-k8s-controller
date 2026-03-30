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

// CollectAndSetPeersParams holds parameters for collecting node IDs from
// each node's sidecar and setting static peers on all validator nodes.
type CollectAndSetPeersParams struct {
	GroupName string   `json:"groupName"`
	Namespace string   `json:"namespace"`
	NodeNames []string `json:"nodeNames"`
	S3Bucket  string   `json:"s3Bucket"`
	S3Prefix  string   `json:"s3Prefix"`
	S3Region  string   `json:"s3Region"`
}

type collectAndSetPeersExecution struct {
	id     string
	params CollectAndSetPeersParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeCollectAndSetPeers(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p CollectAndSetPeersParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing collect-and-set-peers params: %w", err)
		}
	}
	return &collectAndSetPeersExecution{id: id, params: p, cfg: cfg, status: ExecutionRunning}, nil
}

func (e *collectAndSetPeersExecution) Execute(ctx context.Context) error {
	peers, err := e.collectPeers(ctx)
	if err != nil {
		return e.fail(fmt.Errorf("collecting peers: %w", err))
	}

	if err := e.setPeersOnNodes(ctx, peers); err != nil {
		return e.fail(fmt.Errorf("setting peers: %w", err))
	}

	group, err := ResourceAs[*seiv1alpha1.SeiNodeGroup](e.cfg)
	if err != nil {
		return e.fail(err)
	}

	genesisURI := fmt.Sprintf("s3://%s/%sgenesis.json", e.params.S3Bucket, e.params.S3Prefix)
	patch := client.MergeFrom(group.DeepCopy())
	group.Status.GenesisS3URI = genesisURI
	if err := e.cfg.KubeClient.Status().Patch(ctx, group, patch); err != nil {
		return e.fail(fmt.Errorf("recording genesis S3 URI: %w", err))
	}

	e.status = ExecutionComplete
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

		dns := fmt.Sprintf("%s.%s.svc.cluster.local", name, e.params.Namespace)
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
		if node.Spec.Validator == nil {
			continue
		}
		patch := client.MergeFrom(node.DeepCopy())
		node.Spec.Validator.Peers = []seiv1alpha1.PeerSource{
			{Static: &seiv1alpha1.StaticPeerSource{Addresses: peers}},
		}
		if err := e.cfg.KubeClient.Patch(ctx, node, patch); err != nil {
			return fmt.Errorf("setting peers on %s: %w", name, err)
		}
	}
	return nil
}

func (e *collectAndSetPeersExecution) fail(err error) error {
	e.status = ExecutionFailed
	e.err = err
	return err
}

func (e *collectAndSetPeersExecution) Status(_ context.Context) ExecutionStatus {
	return e.status
}
func (e *collectAndSetPeersExecution) Err() error { return e.err }
