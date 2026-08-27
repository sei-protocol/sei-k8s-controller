package v1alpha1

// FullNodeSpec configures a chain-following full node (RPC, sentry, etc.).
// The node bootstraps from a snapshot (S3 or state sync) or syncs from genesis.
// +kubebuilder:validation:XValidation:rule="!has(self.freeze) || !has(self.snapshotGeneration)",message="freeze and snapshotGeneration are mutually exclusive: a frozen node produces no new blocks to snapshot"
// +kubebuilder:validation:XValidation:rule="has(self.freeze) == has(oldSelf.freeze)",message="freeze is create-only: it cannot be added to or removed from an existing node; replace the node instead"
// +kubebuilder:validation:XValidation:rule="!has(self.freeze) || !has(self.snapshot) || !has(self.snapshot.s3) || self.snapshot.s3.targetHeight < self.freeze.height",message="snapshot.s3.targetHeight must be below freeze.height: seid refuses to start once a store has reached the freeze height"
// +kubebuilder:validation:XValidation:rule="!has(self.freeze) || !has(self.snapshot) || !has(self.snapshot.stateSync)",message="freeze and snapshot.stateSync are mutually exclusive: seid disables state sync under freeze and falls back to block sync from genesis"
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
	// this field, and /lag_status reports a permanent failure on a node frozen
	// below the chain tip.
	// +optional
	Freeze *FreezeSpec `json:"freeze,omitempty"`
}
