package seinetwork

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// composeEndpoints builds Endpoints from the resolved Services and, on an
// EvmOnly network, the children's own .status.endpoint. Returns nil when
// there is nothing to publish, so omitempty leaves .status.endpoints absent.
//
// Aggregate scalars (TendermintRpc, TendermintRest) come from InternalService,
// except on an EvmOnly network: its children run with rpc.laddr and the REST
// API disabled, so the Service ports exist but nothing answers behind them.
//
// Per-pod NodeEndpoint entries are surfaced only per-pod — the aggregate
// ClusterIP does not load-balance correctly for stateful EVM sequences
// (filters, mempool, finalized-tag, subscriptions); consumers that need pod
// affinity pin to Nodes[N]. On a Default-engine network they are one entry
// per PerPodServices Service, in inventory order, as they always were. On an
// EvmOnly network they mirror each child's .status.endpoint in child order and
// a child that publishes no EVM URL is omitted — the child controller is the
// one that knows whether its listener answers (see SeiNode's EvmServing
// condition), and a Service is not evidence that anything is bound behind it.
func composeEndpoints(network *seiv1alpha1.SeiNetwork, nodes []seiv1alpha1.SeiNode) *seiv1alpha1.Endpoints {
	out := &seiv1alpha1.Endpoints{}

	if network.Spec.EffectiveExecutionEngine().IsEvmOnly() {
		out.Nodes = servingChildEndpoints(nodes)
	} else {
		if internal := network.Status.InternalService; internal != nil {
			out.TendermintRpc = httpURL(internal.Name, internal.Namespace, internal.Ports.Rpc)
			out.TendermintRest = httpURL(internal.Name, internal.Namespace, internal.Ports.Rest)
		}
		for _, p := range network.Status.PerPodServices {
			out.Nodes = append(out.Nodes, seiv1alpha1.NodeEndpoint{
				Name:       p.Name,
				EvmJsonRpc: httpURL(p.Name, p.Namespace, p.Ports.EvmHttp),
				EvmWs:      wsURL(p.Name, p.Namespace, p.Ports.EvmWs),
			})
		}
	}

	if out.TendermintRpc == "" && len(out.Nodes) == 0 {
		return nil
	}
	return out
}

func servingChildEndpoints(nodes []seiv1alpha1.SeiNode) []seiv1alpha1.NodeEndpoint {
	var out []seiv1alpha1.NodeEndpoint
	for i := range nodes {
		ep := nodes[i].Status.Endpoint
		if ep == nil || (ep.EvmJsonRpc == "" && ep.EvmWs == "") {
			continue
		}
		out = append(out, seiv1alpha1.NodeEndpoint{
			Name:       nodes[i].Name,
			EvmJsonRpc: ep.EvmJsonRpc,
			EvmWs:      ep.EvmWs,
		})
	}
	return out
}

func httpURL(service, namespace string, port int32) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", service, namespace, port)
}

func wsURL(service, namespace string, port int32) string {
	return fmt.Sprintf("ws://%s.%s.svc:%d", service, namespace, port)
}
