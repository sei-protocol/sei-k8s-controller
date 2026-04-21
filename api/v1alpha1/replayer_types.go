package v1alpha1

// ReplayerSpec configures an ephemeral replay workload that restores from a
// snapshot and replays blocks forward. Always requires both a snapshot source
// and peer sources for block sync after restore. Peers are configured on
// SeiNodeSpec.Peers (enforced by CEL validation on SeiNodeSpec).
type ReplayerSpec struct {
	// Snapshot identifies the snapshot to restore from before replay begins.
	Snapshot SnapshotSource `json:"snapshot"`

	// ResultExport configures block-execution result export. Select one or more
	// sub-structs (e.g., shadowResult) to enable an export mode. Useful for
	// shadow replayers that compare execution results against the canonical chain.
	// +optional
	ResultExport *ResultExportConfig `json:"resultExport,omitempty"`
}
