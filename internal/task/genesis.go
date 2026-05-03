package task

import (
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
)

// GenerateIdentityParams are the serialized fields for generate-identity.
type GenerateIdentityParams struct {
	ChainID string `json:"chainId"`
	Moniker string `json:"moniker"`
}

func (p *GenerateIdentityParams) taskType() string { return sidecar.TaskTypeGenerateIdentity }

func (p *GenerateIdentityParams) toRequestParams() *map[string]any {
	m := map[string]any{
		"chainId": p.ChainID,
		"moniker": p.Moniker,
	}
	return &m
}

// GenerateGentxParams are the serialized fields for generate-gentx.
type GenerateGentxParams struct {
	ChainID        string `json:"chainId"`
	StakingAmount  string `json:"stakingAmount"`
	AccountBalance string `json:"accountBalance"`
	GenesisParams  string `json:"genesisParams,omitempty"`
}

func (p *GenerateGentxParams) taskType() string { return sidecar.TaskTypeGenerateGentx }

func (p *GenerateGentxParams) toRequestParams() *map[string]any {
	m := map[string]any{
		"chainId":        p.ChainID,
		"stakingAmount":  p.StakingAmount,
		"accountBalance": p.AccountBalance,
	}
	if p.GenesisParams != "" {
		m["genesisParams"] = p.GenesisParams
	}
	return &m
}

// UploadGenesisArtifactsParams are the serialized fields for upload-genesis-artifacts.
// S3 coordinates are derived by the sidecar from its environment.
type UploadGenesisArtifactsParams struct {
	NodeName string `json:"nodeName"`
}

func (p *UploadGenesisArtifactsParams) taskType() string {
	return sidecar.TaskTypeUploadGenesisArtifacts
}

func (p *UploadGenesisArtifactsParams) toRequestParams() *map[string]any {
	m := map[string]any{
		"nodeName": p.NodeName,
	}
	return &m
}

// Re-exported so the planner doesn't need a second seictl import.
type (
	GenesisNodeParam    = sidecar.GenesisNodeParam
	GenesisAccountEntry = sidecar.GenesisAccountEntry
)

// AssembleAndUploadGenesisParams is the controller-side shell that
// satisfies taskParamser. Wire format and validation come from
// sidecar.AssembleAndUploadGenesisTask — single source of truth.
type AssembleAndUploadGenesisParams struct {
	AccountBalance string                `json:"accountBalance"`
	Namespace      string                `json:"namespace"`
	Nodes          []GenesisNodeParam    `json:"nodes"`
	Accounts       []GenesisAccountEntry `json:"accounts,omitempty"`
}

func (p *AssembleAndUploadGenesisParams) toClientTask() sidecar.AssembleAndUploadGenesisTask {
	return sidecar.AssembleAndUploadGenesisTask{
		AccountBalance: p.AccountBalance,
		Namespace:      p.Namespace,
		Nodes:          p.Nodes,
		Accounts:       p.Accounts,
	}
}

// Validate surfaces strict bech32 + shape checks at planner-time.
func (p *AssembleAndUploadGenesisParams) Validate() error {
	return p.toClientTask().Validate()
}

func (p *AssembleAndUploadGenesisParams) taskType() string {
	return sidecar.TaskTypeAssembleGenesis
}

func (p *AssembleAndUploadGenesisParams) toRequestParams() *map[string]any {
	return p.toClientTask().ToTaskRequest().Params
}

// SetGenesisPeersParams are the serialized fields for set-genesis-peers.
// S3 coordinates are derived by the sidecar from its environment.
type SetGenesisPeersParams struct{}

func (p *SetGenesisPeersParams) taskType() string { return sidecar.TaskTypeSetGenesisPeers }

func (p *SetGenesisPeersParams) toRequestParams() *map[string]any { return nil }
