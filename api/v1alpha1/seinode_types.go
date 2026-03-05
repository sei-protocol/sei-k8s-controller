package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SeiNodeSpec defines the desired state of a standalone Sei node.
type SeiNodeSpec struct {
	// ChainID of the chain this node belongs to.
	// Deprecated: use Genesis.ChainID. Kept for backward compatibility.
	// +kubebuilder:validation:MinLength=1
	ChainID string `json:"chainId"`

	// Image is the seid container image.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image"`

	// Entrypoint overrides the image command for the running node process.
	// +optional
	Entrypoint *EntrypointConfig `json:"entrypoint,omitempty"`

	// Genesis configures the chain's genesis identity and where the
	// genesis configuration is sourced from.
	Genesis GenesisConfiguration `json:"genesis"`

	// Snapshot configures snapshot-based state restoration from S3.
	// When nil the node either mounts a genesis PVC or does a fresh seid init.
	// +optional
	Snapshot *SnapshotSource `json:"snapshot,omitempty"`

	// Peers configures how this node discovers and connects to peers.
	// When set, the sidecar resolves all configured sources during bootstrap.
	// +optional
	Peers *PeerConfig `json:"peers,omitempty"`

	// Storage controls PVC lifecycle.
	// +optional
	Storage SeiNodeStorageConfig `json:"storage,omitempty"`

	// Sidecar configures the sei-sidecar sidecar.
	// When set, seid-init and snapshot-restore init containers are replaced by the sidecar.
	// +optional
	Sidecar *SidecarConfig `json:"sidecar,omitempty"`

	// SnapshotGeneration configures the node to produce Tendermint state-sync
	// snapshots. When set, the sidecar patches app.toml during bootstrap to
	// enable archival pruning and periodic snapshot creation.
	// +optional
	SnapshotGeneration *SnapshotGenerationConfig `json:"snapshotGeneration,omitempty"`
}

// SnapshotGenerationConfig configures a node to produce Tendermint state-sync
// snapshots by setting archival pruning and a snapshot interval in app.toml.
type SnapshotGenerationConfig struct {
	// Interval is the block interval between snapshots (maps to snapshot-interval
	// in app.toml). For example, 2000 means a snapshot is taken every 2000 blocks.
	// +kubebuilder:validation:Minimum=1
	Interval int64 `json:"interval"`

	// KeepRecent is the number of recent snapshots to retain on disk.
	// Older snapshots are pruned automatically by Tendermint.
	// +optional
	// +kubebuilder:default=5
	KeepRecent int32 `json:"keepRecent,omitempty"`

	// Destination configures where generated snapshots are uploaded.
	// When set, the controller periodically submits upload tasks to the sidecar
	// which tar, compress, and push completed snapshots to the destination.
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
	// A trailing slash is added automatically if missing.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Region is the AWS region of the bucket.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`
}

// GenesisConfiguration defines the chain identity and where genesis data is sourced.
// At most one of PVC or S3 may be set. When neither is set the node uses the
// default genesis produced by seid init.
// +kubebuilder:validation:XValidation:rule="(has(self.pvc) ? 1 : 0) + (has(self.s3) ? 1 : 0) <= 1",message="at most one of pvc or s3 may be set"
type GenesisConfiguration struct {
	// ChainID is the canonical chain identifier for this node.
	// +kubebuilder:validation:MinLength=1
	ChainID string `json:"chainId"`

	// Fresh indicates this is a brand new chain that starts from height 1.
	// When true, the node does not need state sync or a snapshot to bootstrap.
	// +optional
	Fresh bool `json:"fresh,omitempty"`

	// PVC references a pre-provisioned PVC populated by SeiNodePool's genesis ceremony.
	// The controller mounts this PVC directly; no sidecar task runs.
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

// SnapshotSource configures snapshot-based state restoration from S3.
type SnapshotSource struct {
	// Region is the AWS region for S3 access.
	// +optional
	// +kubebuilder:default="eu-central-1"
	Region string `json:"region,omitempty"`

	// Bucket is the S3 snapshot archive to restore from.
	Bucket BucketSnapshot `json:"bucket"`
}

// BucketSnapshot configures snapshot download from an S3 bucket.
type BucketSnapshot struct {
	// URI of the snapshot archive in s3://bucket/prefix format.
	// +kubebuilder:validation:MinLength=1
	URI string `json:"uri"`
}

// SeiNodeStorageConfig controls PVC lifecycle for SeiNode.
type SeiNodeStorageConfig struct {
	// RetainOnDelete prevents the data PVC from being deleted when the
	// SeiNode is deleted — useful for post-mortem inspection of chain state.
	// +optional
	// +kubebuilder:default=false
	RetainOnDelete bool `json:"retainOnDelete,omitempty"`
}

// SidecarConfig configures the sei-sidecar sidecar.
type SidecarConfig struct {
	// Image is the sei-sidecar container image.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Port is the HTTP port the sidecar listens on.
	// +optional
	// +kubebuilder:default=7777
	Port int32 `json:"port,omitempty"`

	// Resources defines CPU/memory requests and limits for the sidecar container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// PeerConfig configures how a node discovers and connects to peers.
type PeerConfig struct {
	// Sources is an ordered list of peer sources. The sidecar iterates
	// all sources, unions and deduplicates the results.
	// +kubebuilder:validation:MinItems=1
	Sources []PeerSource `json:"sources"`
}

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
	// All specified tags must match (AND logic).
	// +kubebuilder:validation:MinProperties=1
	Tags map[string]string `json:"tags"`
}

// StaticPeerSource provides a fixed list of peer addresses.
type StaticPeerSource struct {
	// Addresses is a list of peer addresses in "nodeId@host:port" format.
	// +kubebuilder:validation:MinItems=1
	Addresses []string `json:"addresses"`
}

// TaskPlanPhase represents the overall state of an initialization plan.
// +kubebuilder:validation:Enum=Active;Complete;Failed
type TaskPlanPhase string

const (
	TaskPlanActive   TaskPlanPhase = "Active"
	TaskPlanComplete TaskPlanPhase = "Complete"
	TaskPlanFailed   TaskPlanPhase = "Failed"
)

// PlannedTaskStatus represents the state of an individual task within a plan.
// +kubebuilder:validation:Enum=Pending;Submitted;Complete;Failed
type PlannedTaskStatus string

const (
	PlannedTaskPending   PlannedTaskStatus = "Pending"
	PlannedTaskSubmitted PlannedTaskStatus = "Submitted"
	PlannedTaskComplete  PlannedTaskStatus = "Complete"
	PlannedTaskFailed    PlannedTaskStatus = "Failed"
)

// PlannedTask is a single task within a TaskPlan.
type PlannedTask struct {
	// Type identifies the sidecar task (e.g. "snapshot-restore", "config-patch").
	Type string `json:"type"`

	// TaskID is the UUID assigned by the sidecar when the task was submitted.
	// +optional
	TaskID string `json:"taskID,omitempty"`

	// Status is the current state of this task.
	Status PlannedTaskStatus `json:"status"`

	// Error is the error message if the task failed.
	// +optional
	Error string `json:"error,omitempty"`
}

// TaskPlan tracks an ordered sequence of sidecar tasks that the controller
// executes to initialize a node. It serves as both the execution plan and
// the historical log of what happened.
type TaskPlan struct {
	// Phase is the overall state of the plan.
	Phase TaskPlanPhase `json:"phase"`

	// Tasks is the ordered list of tasks to execute.
	Tasks []PlannedTask `json:"tasks"`
}

// SeiNodeStatus defines the observed state of a SeiNode.
type SeiNodeStatus struct {
	// Phase is the high-level lifecycle state.
	// +kubebuilder:validation:Enum=Pending;Running;Failed;Terminating
	Phase string `json:"phase,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// InitPlan tracks the initialization task sequence for this node.
	// Built once from the node's bootstrap mode, then reconciled to completion.
	// +optional
	InitPlan *TaskPlan `json:"initPlan,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=snode
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SeiNode is the Schema for the seinodes API.
type SeiNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SeiNodeSpec   `json:"spec,omitempty"`
	Status SeiNodeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SeiNodeList contains a list of SeiNode.
type SeiNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SeiNode `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SeiNode{}, &SeiNodeList{})
}
