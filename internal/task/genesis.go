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
type UploadGenesisArtifactsParams struct {
	S3Bucket string `json:"s3Bucket"`
	S3Prefix string `json:"s3Prefix"`
	S3Region string `json:"s3Region"`
	NodeName string `json:"nodeName"`
}

func (p *UploadGenesisArtifactsParams) taskType() string {
	return sidecar.TaskTypeUploadGenesisArtifacts
}

func (p *UploadGenesisArtifactsParams) toRequestParams() *map[string]any {
	m := map[string]any{
		"s3Bucket": p.S3Bucket,
		"s3Prefix": p.S3Prefix,
		"s3Region": p.S3Region,
		"nodeName": p.NodeName,
	}
	return &m
}

// AssembleAndUploadGenesisParams are the serialized fields for the
// group-level assemble-and-upload-genesis sidecar task.
type AssembleAndUploadGenesisParams struct {
	S3Bucket       string             `json:"s3Bucket"`
	S3Prefix       string             `json:"s3Prefix"`
	S3Region       string             `json:"s3Region"`
	ChainID        string             `json:"chainId"`
	AccountBalance string             `json:"accountBalance"`
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
		"s3Bucket": p.S3Bucket,
		"s3Prefix": p.S3Prefix,
		"s3Region": p.S3Region,
		"chainId":  p.ChainID,
		"nodes":    nodes,
	}
	return &m
}
