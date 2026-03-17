package v1alpha1

// ArchiveSpec configures an archive node (no pruning, full history).
// Archive nodes always block sync from genesis to retain all historical data.
// If SnapshotGeneration is set, the node also acts as a snapshotter.
type ArchiveSpec struct {
	// Peers configures how the archive node discovers peers for block sync.
	// +optional
	Peers []PeerSource `json:"peers,omitempty"`

	// SnapshotGeneration configures periodic snapshot creation and optional upload.
	// +optional
	SnapshotGeneration *SnapshotGenerationConfig `json:"snapshotGeneration,omitempty"`
}
