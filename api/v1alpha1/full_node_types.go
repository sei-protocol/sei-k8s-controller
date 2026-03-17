package v1alpha1

// FullNodeSpec configures a chain-following full node (RPC, sentry, etc.).
// The node bootstraps from a snapshot (S3 or state sync) or syncs from genesis.
type FullNodeSpec struct {
	// Peers configures how this node discovers and connects to peers.
	// +optional
	Peers []PeerSource `json:"peers,omitempty"`

	// Snapshot configures how the node obtains its initial chain state.
	// When absent the node block-syncs from genesis.
	// +optional
	Snapshot *SnapshotSource `json:"snapshot,omitempty"`

	// SnapshotGeneration configures periodic snapshot creation and optional upload.
	// When set, the controller disables pruning and enables snapshot intervals.
	// +optional
	SnapshotGeneration *SnapshotGenerationConfig `json:"snapshotGeneration,omitempty"`
}
