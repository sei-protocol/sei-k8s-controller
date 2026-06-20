package sei

// Endpoints mirrors SeiNetwork .status.endpoints (seinetwork_types.go:260-276).
// Aggregate stateless protocols (TM RPC/REST) are kube-proxy-safe and sit at
// top level; stateful EVM is per-pod only (Nodes), per the controller's
// discoverability-not-load-balancing contract.
type Endpoints struct {
	TendermintRPC  string                // aggregate; "" until Ready
	TendermintREST string                // aggregate; "" until Ready
	Nodes          []NetworkNodeEndpoint // per-pod EVM JSON-RPC / WS
}

// NetworkNodeEndpoint mirrors SeiNetwork's EVM-only per-pod leaf (the
// NodeEndpoint struct, seinetwork_types.go:281-293). Aggregate networks surface
// stateless TM only at top level, so the per-pod bundle is EVM-only.
type NetworkNodeEndpoint struct {
	Name       string
	EvmJsonRPC string
	EvmWS      string
}

// FleetEndpoints mirrors each follower SeiNode .status.endpoint. One scalar
// bundle per node; the consumer assembles the list (the WS-A0 shape — per-node
// scalar, no fleet aggregate). Unlike the network leaf it carries TM RPC/REST
// per node, because a standalone follower fleet has no aggregate to surface
// them at.
type FleetEndpoints struct {
	Nodes []FleetNodeEndpoint
}

// FleetNodeEndpoint is the 4-field SeiNode leaf (NodeEndpointStatus,
// seinode_types.go:430-452). Name is stamped from the parent object, not the
// status struct. The TM fields are load-bearing: provisionnode.publishEndpoints
// feeds node-0's TendermintRpc/Rest into <ROLE>_TM_RPC / <ROLE>_REST, so an
// EVM-only shape would silently drop them.
type FleetNodeEndpoint struct {
	Name           string
	EvmJsonRPC     string
	EvmWS          string
	TendermintRPC  string
	TendermintREST string
}

// EVMRPCList returns the per-node EVM JSON-RPC URLs in fleet order — the typed
// form of WS-B's <ROLE>_EVM_RPC_LIST CSV, offered so a harness needn't re-join
// (LLD §3.4). Nodes with an empty EvmJsonRPC are skipped.
func (fe FleetEndpoints) EVMRPCList() []string {
	out := make([]string, 0, len(fe.Nodes))
	for _, n := range fe.Nodes {
		if n.EvmJsonRPC != "" {
			out = append(out, n.EvmJsonRPC)
		}
	}
	return out
}
