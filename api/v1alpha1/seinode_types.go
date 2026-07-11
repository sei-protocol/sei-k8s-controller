package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// SeiNodeSpec defines the desired state of a standalone Sei node.
// Exactly one mode sub-spec (fullNode, archive, replayer, validator) must be set;
// the populated field determines the node's operating mode.
// +kubebuilder:validation:XValidation:rule="(has(self.fullNode) ? 1 : 0) + (has(self.archive) ? 1 : 0) + (has(self.replayer) ? 1 : 0) + (has(self.validator) ? 1 : 0) == 1",message="exactly one of fullNode, archive, replayer, or validator must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.replayer) || (has(self.peers) && size(self.peers) > 0)",message="peers is required when replayer mode is set"
type SeiNodeSpec struct {
	// ChainID of the chain this node belongs to.
	// Constrained to DNS-1123 label characters because the controller composes
	// it into P2P endpoint hostnames (e.g. `<node>-p2p.<chainID>.<domain>`) when
	// the parent SeiNetwork opts into TCP networking; the address is a one-way door
	// once peers cache it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ChainID string `json:"chainId"`

	// Image is the seid container image.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image"`

	// Peers configures how this node discovers and connects to network peers.
	// Applies to all node modes. Required for replayer nodes.
	// +optional
	Peers []PeerSource `json:"peers,omitempty"`

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

	// ExternalAddress is the routable P2P host:port written into seid's
	// `p2p.external_address`. SeiNetwork-managed nodes get this stamped by the
	// SeiNetwork reconciler when TCP networking is enabled. Standalone SeiNodes
	// can set it directly.
	// +optional
	ExternalAddress string `json:"externalAddress,omitempty"`

	// Paused freezes reconciliation. While true, the controller does not
	// advance the lifecycle, start plans, or mutate derived resources
	// except the owned StatefulSet — which scales to Replicas=0 so pods
	// terminate. In-flight tasks on the cluster run to completion but
	// their results are not polled until the field is cleared.
	// Has no effect on nodes in PhaseFailed; delete and recreate to
	// recover from a failed node.
	// +optional
	Paused bool `json:"paused,omitempty"`
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
// nil: they bootstrap via block sync from peers (or an imported volume), never
// by restoring from a snapshot, so ArchiveSpec carries no snapshot source — its
// SnapshotGeneration knob only produces snapshots for other nodes to restore from.
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

// TaskPlanPhase represents the overall state of a TaskPlan.
// +kubebuilder:validation:Enum=Active;Complete;Failed
type TaskPlanPhase string

const (
	TaskPlanActive   TaskPlanPhase = "Active"
	TaskPlanComplete TaskPlanPhase = "Complete"
	TaskPlanFailed   TaskPlanPhase = "Failed"
)

// TaskStatus is the lifecycle state of a single PlannedTask.
// +kubebuilder:validation:Enum=Pending;Running;Complete;Failed
type TaskStatus string

const (
	TaskPending  TaskStatus = "Pending"
	TaskRunning  TaskStatus = "Running"
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

// AdoptedWorkflowRef is a SeiNode's durable pointer to the
// SeiNodeTaskWorkflow it is executing. Same-namespace by construction.
type AdoptedWorkflowRef struct {
	// Name of the adopted SeiNodeTaskWorkflow in this node's namespace.
	Name string `json:"name"`

	// UID pins the specific object. On re-adoption after a restart the
	// controller matches by UID so a deleted-and-recreated workflow of the
	// same name is not mistaken for the one originally adopted.
	UID types.UID `json:"uid"`

	// AdoptedAt is when the node stamped this pointer.
	AdoptedAt metav1.Time `json:"adoptedAt"`
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

	// ConditionSigningKeyReady indicates whether a referenced validator
	// signing-key Secret passes all validation requirements. Only set on
	// SeiNodes with spec.validator.signingKey.
	ConditionSigningKeyReady = "SigningKeyReady"

	// ConditionNodeKeyReady indicates whether a referenced validator
	// node-key Secret passes all validation requirements. Only set on
	// SeiNodes with spec.validator.nodeKey.
	ConditionNodeKeyReady = "NodeKeyReady"

	// ConditionOperatorKeyringReady indicates whether a referenced
	// operator-keyring Secret pair (keyring data + passphrase) passes
	// pre-flight validation. Only set on SeiNodes with
	// spec.validator.operatorKeyring.
	ConditionOperatorKeyringReady = "OperatorKeyringReady"

	// ConditionSeiNodePaused mirrors spec.paused: True when paused.
	ConditionSeiNodePaused = "Paused"

	// ConditionStateSyncReady gates the ConfigureStateSync-bearing plan, which is
	// built for any snapshot bootstrap (stateSync or s3 — both apply via CometBFT
	// state-sync and need rpc-server witnesses). Always-present once reconciled.
	// True means canonical syncers are configured and the plan may proceed; False
	// fails closed (no such plan built, and peers are never used as witnesses). It
	// is a configured-count gate: witness reliability comes from curating the
	// canonical-syncer set, and the sidecar establishes the trust point from them
	// as it does today.
	ConditionStateSyncReady = "StateSyncReady"

	// ConditionWorkflowInProgress reports whether the node is currently driving
	// an adopted SeiNodeTaskWorkflow. InProgress-style: True is the exception,
	// False the steady state, always-present once the node is Running (seeded
	// False/NoWorkflow on the first Running reconcile — the phase where a
	// workflow can be adopted; a pre-Running node has none, so absence there is
	// unambiguous). It is the alert-inhibition key for the degraded-present family
	// a held node produces (height-lag, RPC availability, exporter staleness and
	// restart count) — the Paused-condition precedent. Written only by the
	// SeiNode controller (single writer).
	ConditionWorkflowInProgress = "WorkflowInProgress"
)

// Reasons for the WorkflowInProgress condition. Stable enum (public API for
// alerting/runbooks per CLAUDE.md "Conditions").
const (
	// ReasonNoWorkflow: no workflow is adopted (steady state). Seeded value.
	ReasonNoWorkflow = "NoWorkflow"
	// ReasonWorkflowRunning: an adopted workflow's plan is executing; seid may
	// be intentionally held. This is a healthy mid-resync hold.
	ReasonWorkflowRunning = "WorkflowRunning"
	// ReasonWorkflowFailedHeld: an adopted workflow failed (or its deletion is
	// blocked pending data-safety verification) and the node is parked held.
	// Distinct from WorkflowRunning so paging can tell a stuck/failed hold from
	// a healthy in-progress resync.
	ReasonWorkflowFailedHeld = "WorkflowFailedHeld"
)

// Reasons for the StateSyncReady condition.
const (
	// ReasonStateSyncReady: the node bootstraps from a snapshot (stateSync or s3)
	// and >=2 canonical syncers are configured for the chain; the
	// ConfigureStateSync-bearing plan may proceed.
	ReasonStateSyncReady = "Ready"
	// ReasonStateSyncNoSyncersConfigured: the node bootstraps from a snapshot but
	// the canonical-syncer source yields <2 entries for the chain (fail closed).
	ReasonStateSyncNoSyncersConfigured = "NoSyncersConfigured"
	// ReasonStateSyncNotApplicable: the node does not bootstrap from a snapshot
	// (e.g. a genesis node), so it carries no ConfigureStateSync task to gate.
	ReasonStateSyncNotApplicable = "NotApplicable"
	// ReasonStateSyncSyncerSourceError: reading or parsing the canonical-syncer
	// source file failed for a reason other than absence (transient). Fails
	// closed and requeues; the rest of the reconcile (StatefulSet, Failed/Paused
	// handling, status flush) still runs.
	ReasonStateSyncSyncerSourceError = "SyncerSourceError"
)

// Reasons for the ImportPVCReady condition.
const (
	ReasonPVCValidated = "PVCValidated" // import succeeded
	ReasonPVCNotReady  = "PVCNotReady"  // transient: retry
	ReasonPVCInvalid   = "PVCInvalid"   // terminal: fail the plan
)

// Reasons for the SigningKeyReady condition.
const (
	ReasonSigningKeyValidated = "SigningKeyValidated" // validation succeeded
	ReasonSigningKeyNotReady  = "SigningKeyNotReady"  // transient: retry
	ReasonSigningKeyInvalid   = "SigningKeyInvalid"   // terminal: fail the plan
)

// Reasons for the NodeKeyReady condition.
const (
	ReasonNodeKeyValidated = "NodeKeyValidated" // validation succeeded
	ReasonNodeKeyNotReady  = "NodeKeyNotReady"  // transient: retry
	ReasonNodeKeyInvalid   = "NodeKeyInvalid"   // terminal: fail the plan
)

// Reasons for the OperatorKeyringReady condition.
const (
	ReasonOperatorKeyringValidated = "OperatorKeyringValidated" // validation succeeded
	ReasonOperatorKeyringNotReady  = "OperatorKeyringNotReady"  // transient: retry
	ReasonOperatorKeyringInvalid   = "OperatorKeyringInvalid"   // terminal: fail the plan
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

	// CurrentSidecarImage is the sidecar container image observed running
	// on the owned StatefulSet. Stamped jointly with CurrentImage on
	// rollout completion. Empty means "not yet observed" and is treated
	// as no-drift so a controller upgrade doesn't fleet-roll every node
	// on first reconcile.
	// +optional
	CurrentSidecarImage string `json:"currentSidecarImage,omitempty"`

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

	// AdoptedWorkflow points at the single SeiNodeTaskWorkflow this node is
	// currently driving, or nil when none. It is the authoritative,
	// UID-guarded adoption record: committed node-first so a controller
	// restart re-adopts deterministically, and the in-process interlock that
	// makes one-active-workflow-per-node race-free under per-key
	// serialization. Nil is the steady state.
	// +optional
	AdoptedWorkflow *AdoptedWorkflowRef `json:"adoptedWorkflow,omitempty"`

	// ResolvedPeers carries `<node_id>@<host>:<port>` entries resolved
	// from label-based peer sources, ready for CometBFT's persistent_peers.
	// +optional
	ResolvedPeers []string `json:"resolvedPeers,omitempty"`

	// ResolvedRPCWitnesses is DEPRECATED and no longer written. State-sync
	// witnesses now come from the controller-level canonical-syncer ConfigMap
	// (see ResolvedStateSyncers), not label-derived fleet peers. The field is
	// retained present-but-unwritten this release (CRD field removal is a
	// one-way door); remove it at the version bump.
	//
	// Deprecated: use ResolvedStateSyncers.
	// +optional
	ResolvedRPCWitnesses []string `json:"resolvedRPCWitnesses,omitempty"`

	// ResolvedStateSyncers carries the canonical state-sync RPC endpoints
	// (`host:port`) read from the canonical-syncer ConfigMap for this node's
	// chain, fed verbatim into ConfigureStateSyncTask.RpcServers. Written by the
	// StateSyncReady gate only when state-sync is enabled and >=2 syncers are
	// configured; otherwise left empty (fail closed).
	// +optional
	ResolvedStateSyncers []string `json:"resolvedStateSyncers,omitempty"`

	// StatefulSet references the StatefulSet the controller created for
	// this SeiNode. UID is the identity check: an STS with the expected
	// name but a different UID is not the one this controller created
	// (e.g., manual recreation out-of-band) and triggers replacement.
	// +optional
	StatefulSet *StatefulSetRef `json:"statefulSet,omitempty"`

	// Endpoint is the in-cluster discoverable address(es) for this node, derived
	// from its headless Service and mode. It is a DISCOVERABILITY signal, not a
	// serve-readiness guarantee: the URL is published once the node is
	// PhaseRunning and (for EVM) the mode serves EVM, but the seid listener may
	// take additional time to bind — consumers MUST probe before driving load.
	// omitempty leaves .status.endpoint absent for nodes that surface nothing.
	// +optional
	Endpoint *NodeEndpointStatus `json:"endpoint,omitempty"`
}

// NodeEndpointStatus carries the in-cluster URLs this SeiNode serves, derived
// from its headless Service and operating mode. EVM URLs are populated only
// when the node's mode serves EVM HTTP/WS (fullNode, archive); validator and
// replayer modes leave them empty (validator mode disables EVM). All URLs
// resolve to the node's headless Service at <name>.<namespace>.svc. Field names
// match SeiNetwork's NodeEndpoint leaf (evmJsonRpc, evmWs) so consumers parse
// one shape across both CRDs.
type NodeEndpointStatus struct {
	// EvmJsonRpc is the EVM JSON-RPC HTTP URL (http://). Empty unless the
	// node's mode serves EVM (fullNode, archive).
	// +optional
	EvmJsonRpc string `json:"evmJsonRpc,omitempty"`

	// EvmWs is the EVM WebSocket URL (ws://). Empty unless the node's mode
	// serves EVM (fullNode, archive).
	// +optional
	EvmWs string `json:"evmWs,omitempty"`

	// TendermintRpc is the Tendermint / CometBFT RPC URL (http://). Populated
	// only for fullNode/archive (gated by servesEVM); not surfaced for
	// validator/replayer — validators do bind RPC on 0.0.0.0 but we don't
	// advertise it.
	// +optional
	TendermintRpc string `json:"tendermintRpc,omitempty"`

	// TendermintRest is the Cosmos REST (LCD) URL (http://). Served only by
	// fullNode/archive; validators disable the REST API.
	// +optional
	TendermintRest string `json:"tendermintRest,omitempty"`
}

// StatefulSetRef identifies a StatefulSet owned and managed by a
// SeiNode. Stored on Status so the controller can fetch and mutate the
// owned object directly rather than blindly server-side-applying.
type StatefulSetRef struct {
	// Name of the StatefulSet (always equals the SeiNode name).
	Name string `json:"name"`

	// UID of the StatefulSet. Used to detect out-of-band recreation:
	// if a new StatefulSet appears with the same name but a different
	// UID, the controller knows it is not the one it created.
	UID types.UID `json:"uid"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=snode
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="StatefulSet",type=string,JSONPath=`.status.statefulSet.name`,priority=1
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
