package task

// AssembleForkGenesisParams are the serialized fields for the
// assemble-fork-genesis sidecar task. The sidecar downloads exported
// state from the platform genesis bucket at {sourceChainId}/exported-state.json,
// strips old validators, rewrites chain identity, and runs collect-gentxs.
type AssembleForkGenesisParams struct {
	SourceChainID  string             `json:"sourceChainId"`
	ChainID        string             `json:"chainId"`
	AccountBalance string             `json:"accountBalance"`
	Namespace      string             `json:"namespace"`
	Nodes          []GenesisNodeParam `json:"nodes"`
}

func (p *AssembleForkGenesisParams) taskType() string { return "assemble-fork-genesis" }

func (p *AssembleForkGenesisParams) toRequestParams() *map[string]any {
	nodes := make([]any, len(p.Nodes))
	for i, n := range p.Nodes {
		nodes[i] = map[string]any{"name": n.Name}
	}
	m := map[string]any{
		"sourceChainId":  p.SourceChainID,
		"chainId":        p.ChainID,
		"accountBalance": p.AccountBalance,
		"namespace":      p.Namespace,
		"nodes":          nodes,
	}
	return &m
}

var _ taskParamser = (*AssembleForkGenesisParams)(nil)
