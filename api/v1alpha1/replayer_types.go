package v1alpha1

// ReplayerSpec configures an ephemeral replay workload that restores from a
// snapshot and replays blocks forward. Always requires both a snapshot source
// and peer sources for block sync after restore.
type ReplayerSpec struct {
	// Snapshot identifies the snapshot to restore from before replay begins.
	Snapshot SnapshotSource `json:"snapshot"`

	// Peers configures how the replayer discovers peers for block sync
	// after restoring from the snapshot.
	// +kubebuilder:validation:MinItems=1
	Peers []PeerSource `json:"peers"`
}
