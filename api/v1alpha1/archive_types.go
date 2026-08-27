package v1alpha1

// ArchiveSpec configures an archive node (no pruning, full history).
// Archive nodes bootstrap via block sync from peers to retain all
// historical data. If SnapshotGeneration is set, the node also produces
// Tendermint state-sync snapshots for other nodes to bootstrap from.
// +kubebuilder:validation:XValidation:rule="!has(self.freeze) || !has(self.snapshotGeneration)",message="freeze and snapshotGeneration are mutually exclusive: a frozen node produces no new blocks to snapshot"
// +kubebuilder:validation:XValidation:rule="has(self.freeze) == has(oldSelf.freeze)",message="freeze is create-only: it cannot be added to or removed from an existing node; replace the node instead"
type ArchiveSpec struct {
	// SnapshotGeneration configures periodic snapshot creation and optional upload.
	// +optional
	SnapshotGeneration *SnapshotGenerationConfig `json:"snapshotGeneration,omitempty"`

	// Freeze holds the node at a block height. The node serves query RPC at
	// that height and never advances. An archive node retains every block, so
	// a frozen archive node serves any height below the freeze height.
	// +optional
	Freeze *FreezeSpec `json:"freeze,omitempty"`
}
