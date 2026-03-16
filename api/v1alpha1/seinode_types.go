package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SeiNodeSpec defines the desired state of a standalone Sei node.
// Exactly one mode sub-spec (fullNode, archive, replayer, validator) must be set;
// the populated field determines the node's operating mode.
// +kubebuilder:validation:XValidation:rule="(has(self.fullNode) ? 1 : 0) + (has(self.archive) ? 1 : 0) + (has(self.replayer) ? 1 : 0) + (has(self.validator) ? 1 : 0) == 1",message="exactly one of fullNode, archive, replayer, or validator must be set"
type SeiNodeSpec struct {
	// ChainID of the chain this node belongs to.
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

	// Storage controls PVC lifecycle.
	// +optional
	Storage SeiNodeStorageConfig `json:"storage,omitempty"`

	// Sidecar configures the sei-sidecar container.
	// +optional
	Sidecar *SidecarConfig `json:"sidecar,omitempty"`

	// --- Mode-specific sub-specs (exactly one must be set) ---

	// FullNode configures a chain-following full node (absorbs the "rpc" role).
	// +optional
	FullNode *FullNodeSpec `json:"fullNode,omitempty"`

	// Archive configures an archive node with full history and no pruning.
	// +optional
	Archive *ArchiveSpec `json:"archive,omitempty"`

	// Replayer configures an ephemeral replay workload that restores from a snapshot.
	// +optional
	Replayer *ReplayerSpec `json:"replayer,omitempty"`

	// Validator configures a consensus-participating validator node.
	// +optional
	Validator *ValidatorSpec `json:"validator,omitempty"`
}

// FullNodeSpec configures a chain-following full node. If SnapshotGeneration
// is set, the node also produces Tendermint state-sync snapshots.
type FullNodeSpec struct {
	// Sync configures how the node discovers peers and bootstraps chain state.
	// +optional
	Sync *SyncConfig `json:"sync,omitempty"`

	// SnapshotGeneration configures periodic snapshot creation and optional upload.
	// +optional
	SnapshotGeneration *SnapshotGenerationConfig `json:"snapshotGeneration,omitempty"`
}

// ArchiveSpec configures an archive node (no pruning, full history).
// If SnapshotGeneration is set, the node also acts as a snapshotter.
type ArchiveSpec struct {
	// Sync configures how the node discovers peers and bootstraps chain state.
	// +optional
	Sync *SyncConfig `json:"sync,omitempty"`

	// SnapshotGeneration configures periodic snapshot creation and optional upload.
	// +optional
	SnapshotGeneration *SnapshotGenerationConfig `json:"snapshotGeneration,omitempty"`
}

// ReplayerSpec configures an ephemeral snapshot-restore workload.
type ReplayerSpec struct {
	// Snapshot is the S3 snapshot to restore.
	Snapshot SnapshotSource `json:"snapshot"`

	// Peers configures how the replayer discovers peers for block sync
	// after restoring from the snapshot.
	Peers PeerConfig `json:"peers"`
}

// ValidatorSpec configures a consensus-participating validator node.
// Stub — will grow to include key management and oracle configuration.
type ValidatorSpec struct {
	// Sync configures how the node discovers peers and bootstraps chain state.
	// +optional
	Sync *SyncConfig `json:"sync,omitempty"`
}

// SyncConfig configures peer discovery and the chain-sync strategy.
// At most one of StateSync or BlockSync may be set. When neither is set,
// block sync from genesis is assumed.
// +kubebuilder:validation:XValidation:rule="!(has(self.stateSync) && has(self.blockSync))",message="stateSync and blockSync are mutually exclusive"
type SyncConfig struct {
	// Peers configures how this node discovers and connects to peers.
	// +optional
	Peers *PeerConfig `json:"peers,omitempty"`

	// StateSync enables Tendermint state sync from peers. The controller
	// manages trust period, trust height, and RPC server configuration.
	// +optional
	StateSync *StateSyncConfig `json:"stateSync,omitempty"`

	// BlockSync configures block-by-block sync from peers. When a Snapshot
	// is provided, the node restores from S3 first to accelerate catch-up.
	// +optional
	BlockSync *BlockSyncConfig `json:"blockSync,omitempty"`
}

// StateSyncConfig enables Tendermint state sync. Presence of this struct
// signals the controller to configure state sync from peers.
type StateSyncConfig struct{}

// BlockSyncConfig configures block sync, optionally accelerated by an
// S3 snapshot restore.
type BlockSyncConfig struct {
	// Snapshot configures S3 snapshot restore before block sync begins.
	// +optional
	Snapshot *SnapshotRestoreConfig `json:"snapshot,omitempty"`
}

// SnapshotRestoreConfig configures bootstrap from a pre-built S3 snapshot.
type SnapshotRestoreConfig struct {
	// Region is the AWS region for S3 access.
	// +optional
	// +kubebuilder:default="eu-central-1"
	Region string `json:"region,omitempty"`

	// Bucket is the S3 snapshot archive to restore from.
	Bucket BucketSnapshot `json:"bucket"`

	// TrustPeriod is the window during which the snapshot's block validators
	// are considered trustworthy. Must be long enough to cover the age of
	// the snapshot (e.g. "9999h0m0s" for old S3 snapshots).
	// +optional
	TrustPeriod string `json:"trustPeriod,omitempty"`

	// BackfillBlocks is the number of historical blocks to fetch from peers
	// after snapshot restore.
	// +optional
	BackfillBlocks int64 `json:"backfillBlocks,omitempty"`
}

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

// GenesisConfiguration defines the chain identity and where genesis data is sourced.
// At most one of PVC or S3 may be set. When neither is set the node uses the
// default genesis produced by seid init.
// +kubebuilder:validation:XValidation:rule="(has(self.pvc) ? 1 : 0) + (has(self.s3) ? 1 : 0) <= 1",message="at most one of pvc or s3 may be set"
type GenesisConfiguration struct {
	// ChainID is the canonical chain identifier for this node.
	// +kubebuilder:validation:MinLength=1
	ChainID string `json:"chainId"`

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
	// SeiNode is deleted.
	// +optional
	// +kubebuilder:default=false
	RetainOnDelete bool `json:"retainOnDelete,omitempty"`
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
// executes to initialize a node.
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
	// +optional
	InitPlan *TaskPlan `json:"initPlan,omitempty"`

	// ScheduledTasks maps task type to the sidecar-assigned UUID of its
	// scheduled task.
	// +optional
	ScheduledTasks map[string]string `json:"scheduledTasks,omitempty"`

	// ConfigStatus reports the observed configuration state from the sidecar.
	// +optional
	ConfigStatus *ConfigStatus `json:"configStatus,omitempty"`
}

// ConfigStatus reports the observed config state of a managed node.
type ConfigStatus struct {
	// Version is the on-disk config schema version.
	// +optional
	Version int `json:"version,omitempty"`

	// Mode is the config mode that was applied.
	// +optional
	Mode string `json:"mode,omitempty"`

	// Diagnostics contains validation findings from the last config-validate task.
	// +optional
	Diagnostics []ConfigDiagnostic `json:"diagnostics,omitempty"`

	// LastValidatedAt is the timestamp of the last successful config-validate task.
	// +optional
	LastValidatedAt *metav1.Time `json:"lastValidatedAt,omitempty"`

	// LastAppliedAt is the timestamp of the last successful config-apply task.
	// +optional
	LastAppliedAt *metav1.Time `json:"lastAppliedAt,omitempty"`

	// DriftDetected is true when the on-disk config diverges from the CRD-desired state.
	// +optional
	DriftDetected bool `json:"driftDetected,omitempty"`
}

// ConfigDiagnostic is a single finding from config validation.
type ConfigDiagnostic struct {
	// Severity is the diagnostic level: ERROR, WARNING, or INFO.
	Severity string `json:"severity"`

	// Field is the config key path that the diagnostic applies to.
	Field string `json:"field"`

	// Message describes the finding.
	Message string `json:"message"`
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
