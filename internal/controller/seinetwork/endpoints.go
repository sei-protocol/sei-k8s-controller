package seinetwork

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// composeEndpoints builds Endpoints from the resolved InternalService and the
// children's own .status.endpoint. Returns nil when there is neither an
// InternalService nor any child publishing an EVM URL, so omitempty leaves
// .status.endpoints absent.
//
// Aggregate scalars (TendermintRpc, TendermintRest) come from InternalService,
// except on an EvmOnly network: its children run with rpc.laddr and the REST
// API disabled, so the Service ports exist but nothing answers behind them.
// Per-pod NodeEndpoint entries mirror each child's .status.endpoint in child
// order and are omitted for a child that publishes no EVM URL — the child
// controller is the one that knows whether its listener answers (see
// SeiNode's EvmServing condition), and PerPodServices is a Service inventory,
// not evidence that anything is bound behind it. EVM JSON-RPC and EVM
// WebSocket are surfaced per-pod only — the aggregate ClusterIP does not
// load-balance correctly for stateful EVM sequences (filters, mempool,
// finalized-tag, subscriptions). Consumers that need pod affinity pin to
// Nodes[N].
func composeEndpoints(network *seiv1alpha1.SeiNetwork, nodes []seiv1alpha1.SeiNode) *seiv1alpha1.Endpoints {
	out := &seiv1alpha1.Endpoints{}

	if internal := network.Status.InternalService; internal != nil && !network.Spec.EffectiveExecutionEngine().IsEvmOnly() {
		out.TendermintRpc = httpURL(internal.Name, internal.Namespace, internal.Ports.Rpc)
		out.TendermintRest = httpURL(internal.Name, internal.Namespace, internal.Ports.Rest)
	}

	for i := range nodes {
		node := &nodes[i]
		ep := node.Status.Endpoint
		if ep == nil || (ep.EvmJsonRpc == "" && ep.EvmWs == "") {
			continue
		}
		out.Nodes = append(out.Nodes, seiv1alpha1.NodeEndpoint{
			Name:       node.Name,
			EvmJsonRpc: ep.EvmJsonRpc,
			EvmWs:      ep.EvmWs,
		})
	}

	if out.TendermintRpc == "" && len(out.Nodes) == 0 {
		return nil
	}
	return out
}

func httpURL(service, namespace string, port int32) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", service, namespace, port)
}
