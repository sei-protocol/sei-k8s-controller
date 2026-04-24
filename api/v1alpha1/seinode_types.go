package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SeiNodeSpec defines the desired state of a standalone Sei node.
// Exactly one mode sub-spec (fullNode, archive, replayer, validator) must be set;
// the populated field determines the node's operating mode.
// +kubebuilder:validation:XValidation:rule="(has(self.fullNode) ? 1 : 0) + (has(self.archive) ? 1 : 0) + (has(self.replayer) ? 1 : 0) + (has(self.validator) ? 1 : 0) == 1",message="exactly one of fullNode, archive, replayer, or validator must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.replayer) || (has(self.peers) && size(self.peers) > 0)",message="peers is required when replayer mode is set"
type SeiNodeSpec struct {
	// ChainID of the chain this node belongs to.
	// +kubebuilder:validation:MinLength=1
	ChainID string `json:"chainId"`

	// Image is the seid container image.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image"`

	// Peers configures how this node discovers and connects to network peers.
	// Applies to all node modes. Required for replayer nodes.
	// +optional
	Peers []PeerSource `json:"peers,omitempty"`

	// Entrypoint overrides the image command for the running node process.
	// +optional
	Entrypoint *EntrypointConfig `json:"entrypoint,omitempty"`

	// Overrides is a flat map of dotted TOML key paths to string values.
	// Keys use the sei-config unified schema (e.g. "evm.http_port", "storage.pruning").
	// These are applied on top of mode defaults during config-apply.
	// +optional
	Overrides map[string]string `json:"overrides,omitempty"`

	// Sidecar configures the sei-sidecar container.
	// +optional
	Sidecar *SidecarConfig `json:"sidecar,omitempty"`

	// PodLabels are additional labels merged into the StatefulSet pod template.
	// The controller always sets sei.io/node; these are additive and applied
	// first so that system labels take precedence.
	// Must be set before the StatefulSet is first created; changes after
	// creation require StatefulSet recreation due to selector immutability.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// DataVolume configures the data PersistentVolumeClaim for this node.
	// When omitted, the controller creates a PVC using the node's mode-default
	// storage class and size (see noderesource.DefaultStorageForMode).
	// +optional
	DataVolume *DataVolumeSpec `json:"dataVolume,omitempty"`

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

// DataVolumeSpec configures how the data PVC is sourced.
//
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.import) || has(self.import))",message="import cannot be unset once configured"
type DataVolumeSpec struct {
	// Import references a pre-existing PersistentVolumeClaim in the same
	// namespace as the SeiNode, instead of creating a new one. The
	// controller validates the referenced PVC but never mutates it. Storage
	// class is the importer's responsibility — the controller does not
	// validate it.
	//
	// When Import is set, the controller never deletes the referenced PVC
	// on SeiNode deletion — storage lifecycle is the operator's responsibility.
	// +optional
	Import *DataVolumeImport `json:"import,omitempty"`
}

// DataVolumeImport names a pre-existing PVC to adopt as this node's data volume.
type DataVolumeImport struct {
	// PVCName is the name of a PersistentVolumeClaim in the SeiNode's
	// namespace. The PVC must be Bound, ReadWriteOnce, and sized at or above
	// the node mode's default storage size. Immutable after creation.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="pvcName is immutable"
	PVCName string `json:"pvcName"`
}

// SnapshotSource returns the SnapshotSource from whichever mode sub-spec is
// populated, or nil if no snapshot is configured. Archive nodes always return
// nil because they use state sync (configured internally by the planner).
func (s *SeiNodeSpec) SnapshotSource() *SnapshotSource {
	switch {
	case s.FullNode != nil:
		return s.FullNode.Snapshot
	case s.Validator != nil:
		return s.Validator.Snapshot
	case s.Replayer != nil:
		return &s.Replayer.Snapshot
	default:
		return nil
	}
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// TaskPlanPhase represents the overall state of an initialization plan.
// +kubebuilder:validation:Enum=Active;Complete;Failed
type TaskPlanPhase string

const (
	TaskPlanActive   TaskPlanPhase = "Active"
	TaskPlanComplete TaskPlanPhase = "Complete"
	TaskPlanFailed   TaskPlanPhase = "Failed"
)

// TaskStatus represents the lifecycle state of a task (plan, monitor, etc.).
// +kubebuilder:validation:Enum=Pending;Complete;Failed
type TaskStatus string

const (
	TaskPending  TaskStatus = "Pending"
	TaskComplete TaskStatus = "Complete"
	TaskFailed   TaskStatus = "Failed"
)

// PlannedTask is a single task within a TaskPlan. Each task carries its full
// payload so the plan executor can deserialize and run it without needing
// access to the planner or sidecar client factory.
type PlannedTask struct {
	// Type identifies the task (e.g. "snapshot-restore", "config-patch").
	Type string `json:"type"`

	// ID is a deterministic UUID v5 derived from planID/taskType/planIndex.
	// Used as the key for sidecar task submission and status polling.
	ID string `json:"id"`

	// Status is the current state of this task.
	Status TaskStatus `json:"status"`

	// Params is the opaque JSON payload for this task. Deserialized at
	// execution time by task.Deserialize into the concrete task type.
	// +optional
	Params *apiextensionsv1.JSON `json:"params,omitempty"`

	// SubmittedAt is when the task was first submitted to the sidecar.
	// Nil means the task has not been submitted yet.
	// +optional
	SubmittedAt *metav1.Time `json:"submittedAt,omitempty"`

	// Error is the error message if the task failed.
	// +optional
	Error string `json:"error,omitempty"`

	// MaxRetries is the maximum number of times this task can be retried after
	// failure. When 0 (default), failures are terminal. Used for tasks like
	// configure-genesis that may need to wait for upstream data to appear.
	// +optional
	MaxRetries int `json:"maxRetries,omitempty"`

	// RetryCount is the current retry attempt number.
	// +optional
	RetryCount int `json:"retryCount,omitempty"`
}

// FailedTaskInfo records details about a task failure for observability.
type FailedTaskInfo struct {
	// Type is the task type that failed.
	Type string `json:"type"`
	// ID is the task ID that failed.
	ID string `json:"id"`
	// Error is the error message from the failed execution.
	Error string `json:"error"`
	// RetryCount is the number of retries that were attempted.
	RetryCount int `json:"retryCount"`
	// MaxRetries is the configured retry limit.
	MaxRetries int `json:"maxRetries"`
}

// TaskPlan tracks an ordered sequence of tasks that the controller
// executes to drive a node toward a target state.
type TaskPlan struct {
	// ID is a unique identifier for this plan instance.
	// +optional
	ID string `json:"id,omitempty"`

	// Phase is the overall state of the plan.
	Phase TaskPlanPhase `json:"phase"`

	// Tasks is the ordered list of tasks to execute.
	Tasks []PlannedTask `json:"tasks"`

	// TargetPhase is the SeiNodePhase the executor sets on the owning
	// resource when the plan completes successfully. When empty, the
	// executor does not perform a phase transition.
	// +optional
	TargetPhase SeiNodePhase `json:"targetPhase,omitempty"`

	// FailedPhase is the SeiNodePhase the executor sets on the owning
	// resource when the plan fails terminally. When empty, the executor
	// does not perform a phase transition on failure.
	// +optional
	FailedPhase SeiNodePhase `json:"failedPhase,omitempty"`

	// FailedTaskIndex is the index of the task that caused the plan to fail.
	// +optional
	FailedTaskIndex *int `json:"failedTaskIndex,omitempty"`

	// FailedTaskDetail records diagnostics about the task that caused the plan to fail.
	// +optional
	FailedTaskDetail *FailedTaskInfo `json:"failedTaskDetail,omitempty"`
}

// SeiNodePhase represents the high-level lifecycle state of a SeiNode.
// +kubebuilder:validation:Enum=Pending;Initializing;Running;Failed;Terminating
type SeiNodePhase string

const (
	PhasePending      SeiNodePhase = "Pending"
	PhaseInitializing SeiNodePhase = "Initializing"
	PhaseRunning      SeiNodePhase = "Running"
	PhaseFailed       SeiNodePhase = "Failed"
	PhaseTerminating  SeiNodePhase = "Terminating"
)

// SeiNode condition types.
const (
	// ConditionNodeUpdateInProgress indicates an image update is being rolled out.
	ConditionNodeUpdateInProgress = "NodeUpdateInProgress"

	// ConditionSidecarReady reflects the last observed sidecar Healthz state.
	ConditionSidecarReady = "SidecarReady"

	// ConditionImportPVCReady indicates whether an imported data PVC passes all
	// validation requirements. Only set on SeiNodes with spec.dataVolume.import.
	ConditionImportPVCReady = "ImportPVCReady"
)

// Reasons for the ImportPVCReady condition.
const (
	ReasonPVCValidated = "PVCValidated" // import succeeded
	ReasonPVCNotReady  = "PVCNotReady"  // transient: retry
	ReasonPVCInvalid   = "PVCInvalid"   // terminal: fail the plan
)

// SeiNodeStatus defines the observed state of a SeiNode.
type SeiNodeStatus struct {
	// Phase is the high-level lifecycle state.
	Phase SeiNodePhase `json:"phase,omitempty"`

	// CurrentImage is the seid container image observed running on the
	// owned StatefulSet. Updated by the SeiNode controller when the
	// StatefulSet rollout completes (currentRevision == updateRevision).
	// Parent controllers compare this against spec.image to determine
	// whether a spec change has been fully actuated.
	// +optional
	CurrentImage string `json:"currentImage,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// PhaseTransitionTime is when the node last changed phases.
	// Used to compute phase duration metrics.
	// +optional
	PhaseTransitionTime *metav1.Time `json:"phaseTransitionTime,omitempty"`

	// Plan tracks the active task sequence for this node. A planner generates
	// the plan based on the node's current state and conditions.
	// +optional
	Plan *TaskPlan `json:"plan,omitempty"`

	// ResolvedPeers is the current set of peer DNS hostnames discovered
	// from label-based peer sources. Reconciled continuously so that
	// future peer-update plans can detect drift.
	// +optional
	ResolvedPeers []string `json:"resolvedPeers,omitempty"`

	// ExternalAddress is the routable P2P address (host:port) for this node,
	// populated by the SeiNodeDeployment controller from the LoadBalancer
	// ingress. Used by the planner to set p2p.external_address in CometBFT
	// config so the node advertises a reachable address for gossip discovery.
	// +optional
	ExternalAddress string `json:"externalAddress,omitempty"`
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
