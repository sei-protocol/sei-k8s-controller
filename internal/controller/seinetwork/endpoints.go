package seinetwork

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// composeEndpoints builds Endpoints from the resolved InternalService and the
// children's own .status.endpoint. Returns nil when there is nothing to
// publish, so omitempty leaves .status.endpoints absent.
//
// Aggregate scalars (TendermintRpc, TendermintRest) come from InternalService,
// except on an EvmOnly network: its children run with rpc.laddr and the REST
// API disabled, so the Service ports exist but nothing answers behind them.
//
// Per-pod NodeEndpoint entries mirror each child's .status.endpoint in child
// order; a child that publishes none is omitted. The child controller is the
// one that knows which listeners its mode and engine actually open (and, for
// EvmOnly, whether the listener answers — see SeiNode's EvmServing
// condition), so every URL here is one the child stands behind. A per-pod
// Service is inventory, not evidence that anything is bound behind it, and is
// deliberately not used to synthesize URLs.
func composeEndpoints(network *seiv1alpha1.SeiNetwork, nodes []seiv1alpha1.SeiNode) *seiv1alpha1.Endpoints {
	out := &seiv1alpha1.Endpoints{}

	if internal := network.Status.InternalService; internal != nil && !network.Spec.EffectiveExecutionEngine().IsEvmOnly() {
		out.TendermintRpc = httpURL(internal.Name, internal.Namespace, internal.Ports.Rpc)
		out.TendermintRest = httpURL(internal.Name, internal.Namespace, internal.Ports.Rest)
	}
	out.Nodes = childEndpoints(nodes)

	if out.TendermintRpc == "" && len(out.Nodes) == 0 {
		return nil
	}
	return out
}

func childEndpoints(nodes []seiv1alpha1.SeiNode) []seiv1alpha1.NodeEndpoint {
	var out []seiv1alpha1.NodeEndpoint
	for i := range nodes {
		ep := nodes[i].Status.Endpoint
		if ep == nil || *ep == (seiv1alpha1.NodeEndpointStatus{}) {
			continue
		}
		out = append(out, seiv1alpha1.NodeEndpoint{
			Name:           nodes[i].Name,
			EvmJsonRpc:     ep.EvmJsonRpc,
			EvmWs:          ep.EvmWs,
			TendermintRpc:  ep.TendermintRpc,
			TendermintRest: ep.TendermintRest,
		})
	}
	return out
}

func httpURL(service, namespace string, port int32) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", service, namespace, port)
}
