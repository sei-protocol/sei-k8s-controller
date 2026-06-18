package node

import (
	"fmt"

	seiconfig "github.com/sei-protocol/sei-config"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// servesEVM reports whether this node's mode serves EVM HTTP/WS. Only fullNode
// and archive do; validator mode disables EVM and replayer is an ephemeral,
// RPC-less restore workload. Gates on the spec sub-spec, not
// noderesource.NodeMode, which collapses replayer -> ModeFull and would wrongly
// surface endpoints for an ephemeral replayer.
func servesEVM(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.FullNode != nil || node.Spec.Archive != nil
}

// composeNodeEndpoints derives the in-cluster URL bundle for this node from its
// headless Service DNS (<name>.<namespace>.svc) and the seiconfig port set.
// Returns nil for modes that serve no EVM, so omitempty leaves .status.endpoint
// absent.
func composeNodeEndpoints(node *seiv1alpha1.SeiNode) *seiv1alpha1.NodeEndpointStatus {
	if !servesEVM(node) {
		return nil
	}
	ns, name := node.Namespace, node.Name
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
