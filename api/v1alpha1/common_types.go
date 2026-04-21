package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

// PeerSource is a union type — exactly one field must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.ec2Tags) ? 1 : 0) + (has(self.static) ? 1 : 0) + (has(self.label) ? 1 : 0) == 1",message="exactly one of ec2Tags, static, or label must be set"
type PeerSource struct {
	// EC2Tags discovers peers by querying EC2 for running instances
	// matching the specified tags.
	// +optional
	EC2Tags *EC2TagsPeerSource `json:"ec2Tags,omitempty"`

	// Static provides a fixed list of peer addresses.
	// +optional
	Static *StaticPeerSource `json:"static,omitempty"`

	// Label discovers peers by selecting SeiNode resources via
	// Kubernetes labels. The controller resolves matching nodes to
	// stable headless Service DNS names and tracks them in
	// status.resolvedPeers. The sidecar queries each for its
	// Tendermint node ID at task execution time.
	// +optional
	Label *LabelPeerSource `json:"label,omitempty"`
}

// LabelPeerSource discovers peers by selecting SeiNode resources via
// Kubernetes labels. The controller resolves matching nodes to their
// headless Service DNS names ({name}-0.{name}.{namespace}.svc.cluster.local)
// and writes them to status.resolvedPeers on every reconcile.
type LabelPeerSource struct {
	// Selector is a set of key-value label pairs. SeiNode resources
	// matching ALL labels are included as peers.
	// +kubebuilder:validation:MinProperties=1
	Selector map[string]string `json:"selector"`

	// Namespace restricts discovery to a specific namespace.
	// When empty, defaults to the namespace of the discovering node.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// EC2TagsPeerSource discovers peers via EC2 tag filters in a specific region.
type EC2TagsPeerSource struct {
	// Region is the AWS region to query for EC2 instances.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`

	// Tags are the EC2 instance tags to filter on.
	// +kubebuilder:validation:MinProperties=1
	Tags map[string]string `json:"tags"`
}

// StaticPeerSource provides a fixed list of peer addresses.
type StaticPeerSource struct {
	// Addresses is a list of peer addresses in "nodeId@host:port" format.
	// +kubebuilder:validation:MinItems=1
	Addresses []string `json:"addresses"`
}

// SnapshotSource identifies where to obtain a chain snapshot and how to
// configure the node after restore. Exactly one source variant must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.s3) ? 1 : 0) + (has(self.stateSync) ? 1 : 0) == 1",message="exactly one of s3 or stateSync must be set"
type SnapshotSource struct {
	// S3 downloads a snapshot archive from an S3 bucket.
	// +optional
	S3 *S3SnapshotSource `json:"s3,omitempty"`

	// StateSync fetches snapshot chunks from peers via Tendermint state sync.
	// +optional
	StateSync *StateSyncSource `json:"stateSync,omitempty"`

	// TrustPeriod is the window during which the snapshot's block validators
	// are considered trustworthy. Must be long enough to cover the age of
	// the snapshot (e.g. "9999h0m0s" for old S3 snapshots).
	// +optional
	TrustPeriod string `json:"trustPeriod,omitempty"`

	// BackfillBlocks is the number of historical blocks to fetch from peers
	// after snapshot restore.
	// +optional
	BackfillBlocks int64 `json:"backfillBlocks,omitempty"`

	// BootstrapImage is a seid container image used for the bootstrap Job.
	// When set, the controller runs a one-shot Job with this image and the
	// seictl sidecar to prepare the node's data PVC before the main StatefulSet
	// starts. The Job restores a snapshot, applies config, and syncs to the
	// target height.
	// +optional
	BootstrapImage string `json:"bootstrapImage,omitempty"`
}

// S3SnapshotSource configures snapshot download from an S3 bucket.
// The controller infers the bucket from the chain ID using the well-known
// convention {chainId}-snapshots/state-sync/ and selects the latest snapshot
// via latest.txt.
type S3SnapshotSource struct {
	// TargetHeight is the block height the node should sync to after restoring.
	// +kubebuilder:validation:Minimum=1
	TargetHeight int64 `json:"targetHeight"`
}

// StateSyncSource enables Tendermint state sync, which fetches snapshot
// chunks from peers on the p2p network. The controller manages trust
// height and RPC server configuration automatically.
type StateSyncSource struct{}

// SnapshotGenerationConfig configures snapshot generation. One or more snapshot
// modes may be enabled by setting the corresponding sub-struct. A mode sub-struct
// being absent means that snapshot type is not produced. An empty
// SnapshotGenerationConfig (no sub-struct set) is rejected by the planner as a
// likely user typo.
type SnapshotGenerationConfig struct {
	// Tendermint configures Tendermint state-sync snapshot generation.
	// +optional
	Tendermint *TendermintSnapshotGenerationConfig `json:"tendermint,omitempty"`
}

// TendermintSnapshotGenerationConfig configures a node to produce Tendermint
// state-sync snapshots. The controller sets archival pruning and a
// system-default snapshot-interval in app.toml. Snapshots are written to the
// node's data volume.
type TendermintSnapshotGenerationConfig struct {
	// KeepRecent is the number of recent snapshots to retain on disk.
	// When Publish is set, must be at least 2 so the upload algorithm can
	// select the second-to-latest completed snapshot. Otherwise must be at
	// least 1.
	// +kubebuilder:validation:Minimum=1
	KeepRecent int32 `json:"keepRecent"`

	// Publish, when set, causes the sidecar to upload completed snapshots
	// to {SEI_SNAPSHOT_BUCKET}/{chainID}/. Absence means snapshots are kept
	// on disk only and are not uploaded.
	// +optional
	Publish *TendermintSnapshotPublishConfig `json:"publish,omitempty"`
}

// TendermintSnapshotPublishConfig configures how completed Tendermint
// snapshots are uploaded. Currently an empty struct — its presence on
// TendermintSnapshotGenerationConfig enables upload to the platform
// snapshot bucket. Fields may be added here in the future (e.g., bucket
// override, prefix) without a breaking change.
type TendermintSnapshotPublishConfig struct{}

// ResultExportConfig configures the node to export block-execution results.
// One or more use-case sub-structs may be enabled. An empty ResultExportConfig
// (no sub-struct set) is rejected by the planner as a likely user typo.
type ResultExportConfig struct {
	// ShadowResult configures the node to generate and export shadow-result
	// pages, comparing local block execution results against a canonical
	// chain via app-hash divergence detection.
	// +optional
	ShadowResult *ShadowResultConfig `json:"shadowResult,omitempty"`
}

// ShadowResultConfig configures shadow-result generation. The sidecar queries
// the local RPC endpoint for block_results, uploads them in compressed NDJSON
// pages to the platform result-export bucket, and compares each page's app-hash
// against the canonical chain. The export task completes when divergence is
// detected.
type ShadowResultConfig struct {
	// CanonicalRPC is the HTTP RPC endpoint of the canonical chain node
	// to compare block-execution results against.
	// +kubebuilder:validation:MinLength=1
	CanonicalRPC string `json:"canonicalRpc"`
}

// EntrypointConfig defines the command and arguments for the node process.
type EntrypointConfig struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Command []string `json:"command"`

	// +optional
	// +kubebuilder:validation:MaxItems=64
	Args []string `json:"args,omitempty"`
}

// SidecarConfig configures the sei-sidecar container.
type SidecarConfig struct {
	// Image overrides the sidecar container image.
	// +optional
	Image string `json:"image,omitempty"`

	// Port is the HTTP port the sidecar listens on.
	// +optional
	// +kubebuilder:default=7777
	Port int32 `json:"port,omitempty"`

	// Resources defines CPU/memory requests and limits for the sidecar container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}
