package v1alpha1

// ArchiveSpec configures an archive node (no pruning, full history).
// Archive nodes always bootstrap via Tendermint state sync then block sync
// to retain all historical data from that point forward.
// If SnapshotGeneration is set, the node also acts as a snapshotter.
type ArchiveSpec struct {
	// SnapshotGeneration configures periodic snapshot creation and optional upload.
	// +optional
	SnapshotGeneration *SnapshotGenerationConfig `json:"snapshotGeneration,omitempty"`
}
