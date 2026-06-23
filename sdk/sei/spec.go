package sei

// DeletionPolicy values for NetworkSpec.DeletionPolicy. They mirror the
// SeiNetwork CRD enum (kept as plain strings so this core stays stdlib-only).
const (
	// DeletionDelete cascades a SeiNetwork delete to its child validators
	// (and their PVCs) — the right choice for an ephemeral chain.
	DeletionDelete = "Delete"
	// DeletionRetain orphans children on delete (the CRD default).
	DeletionRetain = "Retain"
)

// NetworkSpec is the typed input to CreateNetwork. ChainID is not a field: it
// defaults to Name (the genesis chain ID == the network name). Genesis and
// Config are the two override escape hatches so the typed surface need not chase
// every preset knob.
type NetworkSpec struct {
	Name       string // metadata.name; also spec.genesis.chainId and the peer-selector value
	Namespace  string // "" => client default (kubeconfig context / SA namespace)
	Image      string // -> spec.image
	Validators int    // genesis validator count (spec.replicas); >= 1

	Accounts []GenesisAccount  // non-validator genesis accounts to fund
	Genesis  map[string]string // -> spec.genesis.overrides (TOML-path keys)
	Config   map[string]string // -> spec.configOverrides (config.toml/app.toml)
	Labels   map[string]string // extra labels on the SeiNetwork object (e.g. a caller GC/run-id selector); the network object carries no labels otherwise

	// DeletionPolicy controls child-validator deletion; "" leaves the CRD Retain
	// default. Set DeletionDelete for ephemeral chains. -> spec.deletionPolicy.
	DeletionPolicy string
}

// GenesisAccount is a non-validator genesis account to fund.
type GenesisAccount struct {
	Address string
	Balance string
}

// NodeSpec is the typed input to CreateNode — one RPC node peered to a network.
// The caller loops CreateNode for N nodes; there is no Replicas field. Network
// drives the peer wiring (sei.io/seinetwork=<Network>), never a caller field.
type NodeSpec struct {
	Name             string            // metadata.name
	Network          string            // peer-wire target: sei.io/seinetwork=<Network>; also default ChainID
	Namespace        string            // "" => client default
	NetworkNamespace string            // "" => same as Namespace (co-located, the common case); set it when the SeiNetwork lives in a different namespace
	Image            string            // -> spec.image
	Config           map[string]string // -> spec.overrides (TOML-path keys)
	Labels           map[string]string // extra labels on the SeiNode object, merged UNDER the canonical sei.io/role + sei.io/seinetwork (which win on key conflict)
}
