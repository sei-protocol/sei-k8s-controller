package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

// PeerSource is a union type — exactly one field must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.ec2Tags) ? 1 : 0) + (has(self.static) ? 1 : 0) == 1",message="exactly one of ec2Tags or static must be set"
type PeerSource struct {
	// EC2Tags discovers peers by querying EC2 for running instances
	// matching the specified tags.
	// +optional
	EC2Tags *EC2TagsPeerSource `json:"ec2Tags,omitempty"`

	// Static provides a fixed list of peer addresses.
	// +optional
	Static *StaticPeerSource `json:"static,omitempty"`
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

// SnapshotGenerationConfig configures a node to produce Tendermint state-sync
// snapshots and optionally upload them to remote storage. The controller sets
// archival pruning and a system-default snapshot-interval in app.toml.
type SnapshotGenerationConfig struct {
	// KeepRecent is the number of recent snapshots to retain on disk.
	// Must be at least 2 so the upload algorithm can select the
	// second-to-latest completed snapshot.
	// +kubebuilder:validation:Minimum=2
	KeepRecent int32 `json:"keepRecent"`

	// Destination configures where generated snapshots are uploaded.
	// When set, the controller submits a scheduled upload task to the sidecar.
	// +optional
	Destination *SnapshotDestination `json:"destination,omitempty"`
}

// SnapshotDestination configures where generated snapshots are uploaded.
// Exactly one destination type must be set.
type SnapshotDestination struct {
	// S3 uploads snapshots to an S3 bucket.
	S3 *S3SnapshotDestination `json:"s3"`
}

// S3SnapshotDestination configures S3 as the upload target for snapshots.
type S3SnapshotDestination struct {
	// Bucket is the S3 bucket name.
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Prefix is an optional key prefix within the bucket (e.g. "state-sync/").
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Region is the AWS region of the bucket.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`
}

// ResultExportConfig enables export of block execution results to S3.
// The sidecar queries the local RPC endpoint for block results and uploads
// them in compressed NDJSON pages to the platform S3 bucket, keyed by the
// node's chain ID.
//
// When CanonicalRPC is set, the sidecar additionally compares local results
// against the canonical chain and the task completes when app-hash divergence
// is detected (monitor mode). Without CanonicalRPC, results are exported
// periodically on a cron schedule (scheduled mode).
type ResultExportConfig struct {
	// CanonicalRPC is the HTTP RPC endpoint of the canonical chain node
	// to compare block execution results against. When set, the sidecar
	// runs in comparison mode and the task completes when app-hash
	// divergence is detected.
	// +kubebuilder:validation:MinLength=1
	CanonicalRPC string `json:"canonicalRpc"`
}

// GenesisConfiguration defines where genesis data is sourced.
// At most one of PVC or S3 may be set. When neither is set and the chain ID
// is a well-known network (pacific-1, atlantic-2, arctic-1), the sidecar
// writes the embedded genesis from sei-config. Unknown chains fall back to
// the default genesis produced by seid init.
// +kubebuilder:validation:XValidation:rule="(has(self.pvc) ? 1 : 0) + (has(self.s3) ? 1 : 0) <= 1",message="at most one of pvc or s3 may be set"
type GenesisConfiguration struct {
	// PVC references a pre-provisioned PVC populated by SeiNodePool's genesis ceremony.
	// +optional
	PVC *GenesisPVCSource `json:"pvc,omitempty"`

	// S3 configures the sidecar to download genesis.json from an S3 bucket.
	// +optional
	S3 *GenesisS3Source `json:"s3,omitempty"`
}

// GenesisPVCSource references a data PVC that SeiNodePool's prep Job already populated.
type GenesisPVCSource struct {
	// DataPVC is the name of the pre-populated PVC in the same namespace.
	// +kubebuilder:validation:MinLength=1
	DataPVC string `json:"dataPVC"`
}

// GenesisS3Source configures download of genesis.json from an S3 bucket.
type GenesisS3Source struct {
	// URI is the S3 URI of the genesis.json file (s3://bucket/key format).
	// +kubebuilder:validation:MinLength=1
	URI string `json:"uri"`

	// Region is the AWS region for S3 access.
	// +optional
	Region string `json:"region,omitempty"`
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
