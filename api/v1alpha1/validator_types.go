package v1alpha1

// ValidatorSpec configures a consensus-participating validator node.
// Validators bootstrap the same way as full nodes but participate in consensus.
type ValidatorSpec struct {
	// Peers configures how this node discovers and connects to peers.
	// +optional
	Peers []PeerSource `json:"peers,omitempty"`

	// Snapshot configures how the node obtains its initial chain state.
	// When absent the node block-syncs from genesis.
	// +optional
	Snapshot *SnapshotSource `json:"snapshot,omitempty"`
}
