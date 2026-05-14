package nodedeployment

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// composeEndpoints builds Endpoints from the resolved status fields.
// Returns nil when neither InternalService nor PerPodServices has been
// observed, so omitempty leaves .status.endpoints absent.
//
// Aggregate scalars (TendermintRpc, TendermintRest) come from
// InternalService; per-pod NodeEndpoint entries come from PerPodServices
// and preserve its order. EVM JSON-RPC and EVM WebSocket are surfaced
// per-pod only — the aggregate ClusterIP does not load-balance correctly
// for stateful EVM sequences (filters, mempool, finalized-tag,
// subscriptions). Consumers that need pod affinity pin to Nodes[N].
func composeEndpoints(group *seiv1alpha1.SeiNodeDeployment) *seiv1alpha1.Endpoints {
	internal := group.Status.InternalService
	perPod := group.Status.PerPodServices
	if internal == nil && len(perPod) == 0 {
		return nil
	}

	out := &seiv1alpha1.Endpoints{}

	if internal != nil {
		out.TendermintRpc = httpURL(internal.Name, internal.Namespace, internal.Ports.Rpc)
		out.TendermintRest = httpURL(internal.Name, internal.Namespace, internal.Ports.Rest)
	}

	for i := range perPod {
		p := &perPod[i]
		out.Nodes = append(out.Nodes, seiv1alpha1.NodeEndpoint{
			Name:       p.Name,
			EvmJsonRpc: httpURL(p.Name, p.Namespace, p.Ports.EvmHttp),
			EvmWs:      wsURL(p.Name, p.Namespace, p.Ports.EvmWs),
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
