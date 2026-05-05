package nodedeployment

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// composeEndpoints derives the per-protocol URL bundle from the already-
// resolved internal Service and per-pod Services on group.Status. It is the
// only consumer that turns (service-name, namespace, port) into a URL string;
// callers should not reassemble URLs themselves.
//
// Ordering invariants (mirror the API doc contract on Endpoints):
//   - TendermintRpc / TendermintRest / EvmJsonRpc: aggregate ClusterIP URL at
//     index 0; per-pod URLs follow in PerPodServices order (i.e. ordinal-
//     sorted) at indices 1..N. TendermintRpc/Rest have only the aggregate
//     entry until per-pod Services start exposing the Tendermint ports.
//   - EvmWs: per-pod only (indices 0..N-1, ordinal-sorted). The aggregate
//     ClusterIP path is intentionally omitted — kube-proxy's round-robin
//     LB breaks WebSocket subscription affinity.
//
// Returns nil when neither the internal Service nor any per-pod Service has
// been observed yet, so omitempty leaves .status.endpoints absent.
func composeEndpoints(group *seiv1alpha1.SeiNodeDeployment) *seiv1alpha1.Endpoints {
	internal := group.Status.InternalService
	perPod := group.Status.PerPodServices
	if internal == nil && len(perPod) == 0 {
		return nil
	}

	out := &seiv1alpha1.Endpoints{}

	if internal != nil {
		out.TendermintRpc = []string{httpURL(internal.Name, internal.Namespace, internal.Ports.Rpc)}
		out.TendermintRest = []string{httpURL(internal.Name, internal.Namespace, internal.Ports.Rest)}
		out.EvmJsonRpc = []string{httpURL(internal.Name, internal.Namespace, internal.Ports.EvmHttp)}
	}

	// Per-pod URLs follow in PerPodServices order, which the populator
	// already sorted by ordinal — keep that order as the public contract.
	for i := range perPod {
		p := &perPod[i]
		out.EvmJsonRpc = append(out.EvmJsonRpc, httpURL(p.Name, p.Namespace, p.Ports.EvmHttp))
		out.EvmWs = append(out.EvmWs, wsURL(p.Name, p.Namespace, p.Ports.EvmWs))
	}

	// TendermintRpc / TendermintRest do not gain per-pod entries today —
	// per-pod Services don't expose ports 26657 / 1317 (per #158 MVP cut).
	// When that changes, append per-pod entries here and update the
	// PerPodServicePorts API contract.

	return out
}

func httpURL(service, namespace string, port int32) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", service, namespace, port)
}

func wsURL(service, namespace string, port int32) string {
	return fmt.Sprintf("ws://%s.%s.svc:%d", service, namespace, port)
}
