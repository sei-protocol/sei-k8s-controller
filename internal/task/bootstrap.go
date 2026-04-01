package task

import sidecar "github.com/sei-protocol/seictl/sidecar/client"

// SnapshotRestoreParams are the serialized fields for snapshot-restore.
// S3 coordinates are derived by the sidecar from its environment.
// TargetHeight selects the highest available snapshot <= that height.
type SnapshotRestoreParams struct {
	TargetHeight int64 `json:"targetHeight,omitempty"`
}

func (p *SnapshotRestoreParams) taskType() string { return sidecar.TaskTypeSnapshotRestore }

func (p *SnapshotRestoreParams) toRequestParams() *map[string]any {
	if p.TargetHeight <= 0 {
		return nil
	}
	m := map[string]any{
		"targetHeight": p.TargetHeight,
	}
	return &m
}

// ConfigureStateSyncParams are the serialized fields for configure-state-sync.
type ConfigureStateSyncParams struct {
	UseLocalSnapshot bool   `json:"useLocalSnapshot,omitempty"`
	TrustPeriod      string `json:"trustPeriod,omitempty"`
	BackfillBlocks   int64  `json:"backfillBlocks,omitempty"`
}

func (p *ConfigureStateSyncParams) taskType() string { return sidecar.TaskTypeConfigureStateSync }

func (p *ConfigureStateSyncParams) toRequestParams() *map[string]any {
	m := map[string]any{}
	if p.UseLocalSnapshot {
		m["useLocalSnapshot"] = true
	}
	if p.TrustPeriod != "" {
		m["trustPeriod"] = p.TrustPeriod
	}
	if p.BackfillBlocks > 0 {
		m["backfillBlocks"] = p.BackfillBlocks
	}
	if len(m) == 0 {
		return nil
	}
	return &m
}

// AwaitConditionParams are the serialized fields for await-condition.
type AwaitConditionParams struct {
	Condition    string `json:"condition"`
	TargetHeight int64  `json:"targetHeight"`
	Action       string `json:"action,omitempty"`
}

func (p *AwaitConditionParams) taskType() string { return sidecar.TaskTypeAwaitCondition }

func (p *AwaitConditionParams) toRequestParams() *map[string]any {
	m := map[string]any{
		"condition":    p.Condition,
		"targetHeight": p.TargetHeight,
	}
	if p.Action != "" {
		m["action"] = p.Action
	}
	return &m
}
