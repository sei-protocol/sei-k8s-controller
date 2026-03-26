package task

import (
	"context"
	"fmt"

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

// AwaitGenesisAssemblyParams are the serialized fields for await-genesis-assembly.
// This is a controller-managed task that polls S3, not a sidecar task.
type AwaitGenesisAssemblyParams struct {
	S3Bucket string `json:"s3Bucket"`
	S3Prefix string `json:"s3Prefix"`
	S3Region string `json:"s3Region"`
}

// awaitGenesisAssemblyExecution polls S3 for the assembled genesis.json.
// Execute is a no-op — Status IS the execution for this task type.
//
// STUB: The S3 polling logic is not yet implemented; Status always returns
// ExecutionRunning. The genesis assembly PR will inject an ObjectStoreClient
// via ExecutionConfig and implement the HeadObject check here.
type awaitGenesisAssemblyExecution struct {
	id     string
	params AwaitGenesisAssemblyParams
	status ExecutionStatus
	err    error
}

func (e *awaitGenesisAssemblyExecution) Execute(_ context.Context) error {
	return nil
}

func (e *awaitGenesisAssemblyExecution) Status(ctx context.Context) ExecutionStatus {
	if e.status == ExecutionComplete || e.status == ExecutionFailed {
		return e.status
	}

	// TODO(genesis-pr): implement S3 HeadObject check for
	// s3://{bucket}/{prefix}genesis.json. For now, this always returns
	// Running so the task blocks until the group controller assembles
	// and uploads genesis.json (which will be implemented in the genesis
	// task handlers PR).
	key := fmt.Sprintf("%sgenesis.json", e.params.S3Prefix)
	_ = key
	return ExecutionRunning
}

func (e *awaitGenesisAssemblyExecution) Err() error { return e.err }
