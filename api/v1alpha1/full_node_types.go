package v1alpha1

// FullNodeSpec configures a chain-following full node (RPC, sentry, etc.).
// The node bootstraps from a snapshot (S3 or state sync) or syncs from genesis.
// +kubebuilder:validation:XValidation:rule="!has(self.freeze) || !has(self.snapshotGeneration)",message="freeze and snapshotGeneration are mutually exclusive: a frozen node produces no new blocks to snapshot"
type FullNodeSpec struct {
	// Snapshot configures how the node obtains its initial chain state.
	// When absent the node block-syncs from genesis.
	// +optional
	Snapshot *SnapshotSource `json:"snapshot,omitempty"`

	// SnapshotGeneration configures periodic snapshot creation and optional upload.
	// When set, the controller disables pruning and enables snapshot intervals.
	// +optional
	SnapshotGeneration *SnapshotGenerationConfig `json:"snapshotGeneration,omitempty"`

	// Freeze holds the node at a block height. The node serves query RPC at
	// that height and never advances. Set the height here rather than through
	// spec.overrides: the controller derives the node's readiness probe from
	// this field, because a frozen node's lag grows without bound.
	// +optional
	Freeze *FreezeSpec `json:"freeze,omitempty"`
}
