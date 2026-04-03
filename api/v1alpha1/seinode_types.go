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

// TaskPlan tracks an ordered sequence of sidecar tasks that the controller
// executes to initialize a node.
type TaskPlan struct {
	// ID is a unique identifier for this plan instance.
	// +optional
	ID string `json:"id,omitempty"`

	// Phase is the overall state of the plan.
	Phase TaskPlanPhase `json:"phase"`

	// Tasks is the ordered list of tasks to execute.
	Tasks []PlannedTask `json:"tasks"`

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

// MonitorTask tracks a long-running sidecar task that the controller
// actively polls for completion. Unlike fire-and-forget tasks,
// completing a monitor task triggers a controller response (Event + Condition).
// The map key in MonitorTasks serves as the task type identifier.
type MonitorTask struct {
	// ID is the sidecar-assigned task UUID.
	ID string `json:"id"`

	// Status tracks lifecycle: Pending → Complete or Failed.
	Status TaskStatus `json:"status"`

	// SubmittedAt is the time the task was submitted to the sidecar.
	SubmittedAt metav1.Time `json:"submittedAt"`

	// CompletedAt is the time the task reached a terminal state.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Error is set when the task fails.
	// +optional
	Error string `json:"error,omitempty"`
}

// SeiNodeStatus defines the observed state of a SeiNode.
type SeiNodeStatus struct {
	// Phase is the high-level lifecycle state.
	Phase SeiNodePhase `json:"phase,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Plan tracks the active task sequence for this node. A planner generates
	// the plan based on the node's current state and conditions.
	// +optional
	Plan *TaskPlan `json:"plan,omitempty"`

	// MonitorTasks tracks long-running sidecar tasks the controller polls
	// for completion. Keyed by task type for idempotent submission.
	// +optional
	MonitorTasks map[string]MonitorTask `json:"monitorTasks,omitempty"`

	// ResolvedPeers is the current set of peer DNS hostnames discovered
	// from label-based peer sources. Reconciled continuously so that
	// future peer-update plans can detect drift.
	// +optional
	ResolvedPeers []string `json:"resolvedPeers,omitempty"`

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
