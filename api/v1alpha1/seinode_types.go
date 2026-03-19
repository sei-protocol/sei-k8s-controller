package v1alpha1

import (
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

	// Genesis configures where the genesis configuration is sourced from.
	// When omitted for a well-known chain (pacific-1, atlantic-2, arctic-1),
	// the sidecar writes the embedded genesis from sei-config.
	// +optional
	Genesis GenesisConfiguration `json:"genesis,omitempty"`

	// Storage controls PVC lifecycle.
	// +optional
	Storage SeiNodeStorageConfig `json:"storage,omitempty"`

	// Overrides is a flat map of dotted TOML key paths to string values.
	// Keys use the sei-config unified schema (e.g. "evm.http_port", "storage.pruning").
	// These are applied on top of mode defaults during config-apply.
	// +optional
	Overrides map[string]string `json:"overrides,omitempty"`

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

// SeiNodePhase represents the high-level lifecycle state of a SeiNode.
// +kubebuilder:validation:Enum=Pending;PreInitializing;Initializing;Running;Failed;Terminating
type SeiNodePhase string

const (
	PhasePending         SeiNodePhase = "Pending"
	PhasePreInitializing SeiNodePhase = "PreInitializing"
	PhaseInitializing    SeiNodePhase = "Initializing"
	PhaseRunning         SeiNodePhase = "Running"
	PhaseFailed          SeiNodePhase = "Failed"
	PhaseTerminating     SeiNodePhase = "Terminating"
)

// SeiNodeStatus defines the observed state of a SeiNode.
type SeiNodeStatus struct {
	// Phase is the high-level lifecycle state.
	Phase SeiNodePhase `json:"phase,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// PreInitPlan tracks the pre-initialization task sequence (bootstrap Job).
	// +optional
	PreInitPlan *TaskPlan `json:"preInitPlan,omitempty"`

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
