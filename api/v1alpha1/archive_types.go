package v1alpha1

// ArchiveSpec configures an archive node (no pruning, full history).
// Archive nodes bootstrap via Tendermint state sync then block sync to
// retain all historical data from that point forward.
// If SnapshotGeneration is set, the node also acts as a snapshotter.
type ArchiveSpec struct {
	// Peers configures how the archive node discovers peers for block sync.
	// +optional
	Peers []PeerSource `json:"peers,omitempty"`

	// StateSync enables Tendermint state sync for initial bootstrap.
	// When set, the sidecar configures state sync so the node fetches
	// a snapshot from peers before switching to block sync.
	// +optional
	StateSync *StateSyncSource `json:"stateSync,omitempty"`

	// SnapshotGeneration configures periodic snapshot creation and optional upload.
	// +optional
	SnapshotGeneration *SnapshotGenerationConfig `json:"snapshotGeneration,omitempty"`
}
