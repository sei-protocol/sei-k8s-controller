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

// AssembleAndUploadGenesisParams are the serialized fields for the
// group-level assemble-and-upload-genesis sidecar task.
// S3 coordinates are derived by the sidecar from its environment.
type AssembleAndUploadGenesisParams struct {
	AccountBalance string             `json:"accountBalance"`
	Namespace      string             `json:"namespace"`
	Nodes          []GenesisNodeParam `json:"nodes"`
}

// GenesisNodeParam identifies a node participating in the genesis ceremony.
type GenesisNodeParam struct {
	Name string `json:"name"`
}

func (p *AssembleAndUploadGenesisParams) taskType() string {
	return sidecar.TaskTypeAssembleGenesis
}

func (p *AssembleAndUploadGenesisParams) toRequestParams() *map[string]any {
	nodes := make([]map[string]any, len(p.Nodes))
	for i, n := range p.Nodes {
		nodes[i] = map[string]any{"name": n.Name}
	}
	m := map[string]any{
		"accountBalance": p.AccountBalance,
		"namespace":      p.Namespace,
		"nodes":          nodes,
	}
	return &m
}

// SetGenesisPeersParams are the serialized fields for set-genesis-peers.
// S3 coordinates are derived by the sidecar from its environment.
type SetGenesisPeersParams struct{}

func (p *SetGenesisPeersParams) taskType() string { return sidecar.TaskTypeSetGenesisPeers }

func (p *SetGenesisPeersParams) toRequestParams() *map[string]any { return nil }
