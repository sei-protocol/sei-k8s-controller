package v1alpha1

// ArchiveSpec configures an archive node (no pruning, full history).
// Archive nodes bootstrap via block sync from peers to retain all
// historical data. If SnapshotGeneration is set, the node also produces
// Tendermint state-sync snapshots for other nodes to bootstrap from.
type ArchiveSpec struct {
	// SnapshotGeneration configures periodic snapshot creation and optional upload.
	// +optional
	SnapshotGeneration *SnapshotGenerationConfig `json:"snapshotGeneration,omitempty"`
}
