package node

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"
	apimeta "k8s.io/apimachinery/pkg/api/meta"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// servesEVM reports whether this node's mode serves EVM HTTP/WS under the
// Default execution engine. Only fullNode and archive do; validator mode
// disables EVM and replayer is an ephemeral, RPC-less restore workload. Gates
// on the spec sub-spec, not noderesource.NodeMode, which collapses replayer ->
// ModeFull and would wrongly surface endpoints for an ephemeral replayer.
func servesEVM(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.FullNode != nil || node.Spec.Archive != nil
}

// composeNodeEndpoints derives the in-cluster URL bundle for this node from its
// headless Service DNS (<name>.<namespace>.svc) and the seiconfig port set.
//
// Under the Default engine the bundle is identity-derived for fullNode and
// archive and nil for every other mode. Under the EvmOnly engine the bundle is
// the EVM JSON-RPC URL alone (Tendermint RPC/REST are disabled, and the
// validator-mode WebSocket listener is off) and is published only while
// EvmServing is True — a consumer that reads a URL here can dial it. Returns
// nil otherwise, so omitempty leaves .status.endpoint absent.
func composeNodeEndpoints(node *seiv1alpha1.SeiNode) *seiv1alpha1.NodeEndpointStatus {
	ns, name := node.Namespace, node.Name
	if node.Spec.EffectiveExecutionEngine().IsEvmOnly() {
		if !apimeta.IsStatusConditionTrue(node.Status.Conditions, seiv1alpha1.ConditionEvmServing) {
			return nil
		}
		return &seiv1alpha1.NodeEndpointStatus{
			EvmJsonRpc: httpURL(name, ns, seiconfig.PortEVMHTTP),
		}
	}
	if !servesEVM(node) {
		return nil
	}
	return &seiv1alpha1.NodeEndpointStatus{
		EvmJsonRpc:     httpURL(name, ns, seiconfig.PortEVMHTTP),
		EvmWs:          wsURL(name, ns, seiconfig.PortEVMWS),
		TendermintRpc:  httpURL(name, ns, seiconfig.PortRPC),
		TendermintRest: httpURL(name, ns, seiconfig.PortREST),
	}
}

func httpURL(service, namespace string, port int32) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", service, namespace, port)
}

func wsURL(service, namespace string, port int32) string {
	return fmt.Sprintf("ws://%s.%s.svc:%d", service, namespace, port)
}
