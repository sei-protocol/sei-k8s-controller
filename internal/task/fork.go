package task

// Controller-managed fork task types.
const (
	TaskTypeDeployForkJob   = "deploy-fork-job"
	TaskTypeAwaitForkExport = "await-fork-export"
	TaskTypeTeardownForkJob = "teardown-fork-job"
)

// AssembleForkGenesisParams are the serialized fields for the
// assemble-fork-genesis sidecar task.
type AssembleForkGenesisParams struct {
	SourceChainID  string                 `json:"sourceChainId"`
	SourceHeight   int64                  `json:"sourceHeight"`
	NewChainID     string                 `json:"newChainId"`
	AccountBalance string                 `json:"accountBalance"`
	StakingAmount  string                 `json:"stakingAmount"`
	Namespace      string                 `json:"namespace"`
	Nodes          []GenesisNodeParam     `json:"nodes"`
	Mutations      []GenesisMutationParam `json:"mutations,omitempty"`

	// Phase 1: pre-exported state S3 coordinates.
	StateExportBucket string `json:"stateExportBucket,omitempty"`
	StateExportKey    string `json:"stateExportKey,omitempty"`
	StateExportRegion string `json:"stateExportRegion,omitempty"`
}

func (p *AssembleForkGenesisParams) taskType() string { return "assemble-fork-genesis" }

func (p *AssembleForkGenesisParams) toRequestParams() *map[string]any {
	nodes := make([]any, len(p.Nodes))
	for i, n := range p.Nodes {
		nodes[i] = map[string]any{"name": n.Name}
	}
	m := map[string]any{
		"sourceChainId":  p.SourceChainID,
		"sourceHeight":   p.SourceHeight,
		"newChainId":     p.NewChainID,
		"accountBalance": p.AccountBalance,
		"stakingAmount":  p.StakingAmount,
		"namespace":      p.Namespace,
		"nodes":          nodes,
	}
	if len(p.Mutations) > 0 {
		mutations := make([]any, len(p.Mutations))
		for i, mut := range p.Mutations {
			mm := map[string]any{"type": mut.Type}
			if mut.Key != "" {
				mm["key"] = mut.Key
			}
			if mut.Value != "" {
				mm["value"] = mut.Value
			}
			if mut.Address != "" {
				mm["address"] = mut.Address
			}
			if mut.Balance != "" {
				mm["balance"] = mut.Balance
			}
			mutations[i] = mm
		}
		m["mutations"] = mutations
	}
	if p.StateExportBucket != "" {
		m["stateExportBucket"] = p.StateExportBucket
		m["stateExportKey"] = p.StateExportKey
		m["stateExportRegion"] = p.StateExportRegion
	}
	return &m
}

var _ taskParamser = (*AssembleForkGenesisParams)(nil)

// GenesisMutationParam is the serialized form of a genesis mutation.
type GenesisMutationParam struct {
	Type    string `json:"type"`
	Key     string `json:"key,omitempty"`
	Value   string `json:"value,omitempty"`
	Address string `json:"address,omitempty"`
	Balance string `json:"balance,omitempty"`
}

// DeployForkJobParams holds parameters for creating the fork export Job.
type DeployForkJobParams struct {
	GroupName     string `json:"groupName"`
	Namespace     string `json:"namespace"`
	JobName       string `json:"jobName"`
	ServiceName   string `json:"serviceName"`
	Image         string `json:"image"`
	SourceChainID string `json:"sourceChainId"`
	SourceHeight  int64  `json:"sourceHeight"`
	TargetHeight  int64  `json:"targetHeight"`
}

// AwaitForkExportParams holds parameters for waiting until the fork
// export Job completes.
type AwaitForkExportParams struct {
	JobName   string `json:"jobName"`
	Namespace string `json:"namespace"`
}

// TeardownForkJobParams holds parameters for cleaning up the fork export
// Job and its Service.
type TeardownForkJobParams struct {
	JobName     string `json:"jobName"`
	ServiceName string `json:"serviceName"`
	Namespace   string `json:"namespace"`
}
