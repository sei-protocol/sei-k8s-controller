package nodedeployment

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// composeEndpoints builds Endpoints from the resolved status fields.
// Returns nil when neither has been observed, so omitempty leaves
// .status.endpoints absent. See the Endpoints type for the ordering contract.
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

	for i := range perPod {
		p := &perPod[i]
		out.EvmJsonRpc = append(out.EvmJsonRpc, httpURL(p.Name, p.Namespace, p.Ports.EvmHttp))
		out.EvmWs = append(out.EvmWs, wsURL(p.Name, p.Namespace, p.Ports.EvmWs))
	}

	return out
}

func httpURL(service, namespace string, port int32) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", service, namespace, port)
}

func wsURL(service, namespace string, port int32) string {
	return fmt.Sprintf("ws://%s.%s.svc:%d", service, namespace, port)
}
