package task

import sidecar "github.com/sei-protocol/seictl/sidecar/client"

// AssembleForkGenesisParams is the controller-side shell that satisfies
// taskParamser. Wire format and validation come from
// sidecar.AssembleGenesisForkTask — single source of truth.
type AssembleForkGenesisParams struct {
	SourceChainID  string                `json:"sourceChainId"`
	ChainID        string                `json:"chainId"`
	AccountBalance string                `json:"accountBalance"`
	Namespace      string                `json:"namespace"`
	Nodes          []GenesisNodeParam    `json:"nodes"`
	Accounts       []GenesisAccountEntry `json:"accounts,omitempty"`
}

func (p *AssembleForkGenesisParams) toClientTask() sidecar.AssembleGenesisForkTask {
	return sidecar.AssembleGenesisForkTask{
		SourceChainID:  p.SourceChainID,
		ChainID:        p.ChainID,
		AccountBalance: p.AccountBalance,
		Namespace:      p.Namespace,
		Nodes:          p.Nodes,
		Accounts:       p.Accounts,
	}
}

func (p *AssembleForkGenesisParams) Validate() error {
	return p.toClientTask().Validate()
}

func (p *AssembleForkGenesisParams) taskType() string { return sidecar.TaskTypeAssembleGenesisFork }

func (p *AssembleForkGenesisParams) toRequestParams() *map[string]any {
	return p.toClientTask().ToTaskRequest().Params
}
