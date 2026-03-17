package v1alpha1

// ReplayerSpec configures an ephemeral replay workload that restores from a
// snapshot and replays blocks forward. Always requires both a snapshot source
// and peer sources for block sync after restore.
type ReplayerSpec struct {
	// Peers configures how the replayer discovers peers for block sync
	// after restoring from the snapshot.
	// +kubebuilder:validation:MinItems=1
	Peers []PeerSource `json:"peers"`

	// Snapshot identifies the snapshot to restore from before replay begins.
	Snapshot SnapshotSource `json:"snapshot"`

	// ResultExport configures periodic export of block execution results to S3.
	// The sidecar queries the local RPC for block_results and uploads compressed
	// NDJSON pages on a schedule. Useful for shadow replayers that need their
	// execution results compared against the canonical chain.
	// +optional
	ResultExport *ResultExportConfig `json:"resultExport,omitempty"`
}
