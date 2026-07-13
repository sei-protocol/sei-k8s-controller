package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SeiNodeTaskWorkflowKind discriminates the SeiNodeTaskWorkflow spec union.
// Exactly one matching payload sub-struct in SeiNodeTaskWorkflowSpec must be
// set. A workflow is a pure request object: the SeiNode controller adopts it
// and is its single executor (there is no workflow controller).
// +kubebuilder:validation:Enum=StateSync
type SeiNodeTaskWorkflowKind string

const (
	// SeiNodeTaskWorkflowKindStateSync re-bootstraps the target node through
	// CometBFT state sync: mark-not-ready -> stop-seid -> reset-data ->
	// config-patch -> configure-state-sync -> mark-ready. The recipe ends at
	// mark-ready, so Complete means every mutation was performed and the node
	// was released to re-bootstrap, NOT that it caught up; catch-up is
	// verified node-side (sdk WaitCaughtUp, RPC, alerts) by whoever triggered
	// the workflow. The clean consequence: Failed always means the node is
	// still held. It is the paved road for STO-624.
	SeiNodeTaskWorkflowKindStateSync SeiNodeTaskWorkflowKind = "StateSync"
)

// SeiNodeTaskWorkflowPhase is the high-level lifecycle state of a workflow.
// +kubebuilder:validation:Enum=Pending;Running;Complete;Failed
type SeiNodeTaskWorkflowPhase string

const (
	// SeiNodeTaskWorkflowPhasePending is the seed phase: created but not yet
	// adopted (idle-waiting for its target, or queued behind an active
	// workflow on the same node).
	SeiNodeTaskWorkflowPhasePending SeiNodeTaskWorkflowPhase = "Pending"
	// SeiNodeTaskWorkflowPhaseRunning means the target node has adopted the
	// workflow and is driving its compiled plan.
	SeiNodeTaskWorkflowPhaseRunning SeiNodeTaskWorkflowPhase = "Running"
	// SeiNodeTaskWorkflowPhaseComplete is the terminal success phase.
	SeiNodeTaskWorkflowPhaseComplete SeiNodeTaskWorkflowPhase = "Complete"
	// SeiNodeTaskWorkflowPhaseFailed is the terminal failure phase. The node
	// stays held (fail-closed); recovery is a re-run or manual escape.
	SeiNodeTaskWorkflowPhaseFailed SeiNodeTaskWorkflowPhase = "Failed"
)

// SeiNodeTaskWorkflow condition types. Ready/Failed are the documented
// latch-on-terminal-state pair (CLAUDE.md "Conditions"), load-bearing for
// `kubectl wait --for=condition=Ready=true` and its `Failed=true` dual, the
// same contract SeiNodeTask established.
const (
	// ConditionSeiNodeTaskWorkflowReady is True only when phase == Complete.
	ConditionSeiNodeTaskWorkflowReady = "Ready"
	// ConditionSeiNodeTaskWorkflowFailed is True only when phase == Failed.
	ConditionSeiNodeTaskWorkflowFailed = "Failed"
	// ConditionSeiNodeTaskWorkflowAdopted reflects whether a node has adopted
	// this workflow. True once a node stamps its adoptedWorkflow pointer at
	// it; the reason discriminates queued-vs-refused-vs-adopted.
	ConditionSeiNodeTaskWorkflowAdopted = "Adopted"
)

// Reasons for the Adopted condition. Treated as a stable enum (public API for
// runbooks/alerting per CLAUDE.md "Conditions").
const (
	// ReasonWorkflowAdopted: a node has adopted and is driving this workflow.
	ReasonWorkflowAdopted = "Adopted"
	// ReasonWorkflowQueued: the target node already has an active workflow;
	// this one waits its turn (first-wins by creationTimestamp, then name).
	ReasonWorkflowQueued = "QueuedBehindActive"
	// ReasonWorkflowTargetNotReady: the target node is not yet adoptable
	// (missing, not Running, or Paused).
	ReasonWorkflowTargetNotReady = "TargetNotReady"
	// ReasonWorkflowTargetRejected: the target node is structurally ineligible
	// (validator mode) and can never adopt this workflow.
	ReasonWorkflowTargetRejected = "TargetRejected"
	// ReasonWorkflowTargetPhaseTimeout: the target did not reach the required
	// phase within spec.target.requirePhaseTimeout; the workflow is failed.
	ReasonWorkflowTargetPhaseTimeout = "TargetPhaseTimeout"
	// ReasonWorkflowPlanBuildFailed: the recipe could not compile against the
	// target (e.g. fewer than two resolved state-sync witnesses).
	ReasonWorkflowPlanBuildFailed = "PlanBuildFailed"
)

// WorkflowForceDeleteAnnotation, when present on a workflow with any non-empty
// value, instructs the finalizer to release the hold and clear the adoption
// pointer WITHOUT the data-state safety verification. It is the documented
// operator escape hatch: it may release seid onto a partially reset data
// directory, so it is for deliberate manual recovery only.
//
// Escape order under a paused target: the SeiNode reconcile freezes on
// spec.paused BEFORE it runs the workflow finalizer, so a force-delete of a
// workflow whose target is paused does not take effect until the node is
// unpaused. Unpause the target first (clear spec.paused), then the force delete
// releases on the next reconcile. This is the accepted freeze semantics — pause
// suspends ALL node action, including hold release.
const WorkflowForceDeleteAnnotation = "sei.io/force-delete-workflow"

// SeiNodeTaskWorkflowFinalizer gates deletion of an adopted workflow so the
// hold is never lifted onto partial state without a safety check (or the
// force annotation). Added by the SeiNode controller at adoption.
const SeiNodeTaskWorkflowFinalizer = "sei.io/seinodetaskworkflow-finalizer"

// ---------------------------------------------------------------------------
// Spec
// ---------------------------------------------------------------------------

// SeiNodeTaskWorkflowSpec is the desired state of a SeiNodeTaskWorkflow: a
// discriminated union selecting one reviewed recipe against one target node.
//
// The spec is a one-way door: field names are locked at v1alpha1 and the
// recipe is immutable after creation. Its typed parameters are snapshotted
// into the compiled plan at adoption and never re-read, so a workflow behaves
// as a pure request.
//
// +kubebuilder:validation:XValidation:rule="(has(self.stateSync) ? 1 : 0) == 1",message="exactly one recipe (stateSync) must be set"
// +kubebuilder:validation:XValidation:rule="self.kind != 'StateSync' || has(self.stateSync)",message="spec.stateSync is required when kind=StateSync"
// +kubebuilder:validation:XValidation:rule="self.kind == oldSelf.kind",message="spec.kind is immutable"
// +kubebuilder:validation:XValidation:rule="self.target == oldSelf.target",message="spec.target is immutable"
// +kubebuilder:validation:XValidation:rule="self.stateSync == oldSelf.stateSync",message="spec.stateSync is immutable"
type SeiNodeTaskWorkflowSpec struct {
	// Kind selects the recipe. Immutable after creation.
	Kind SeiNodeTaskWorkflowKind `json:"kind"`

	// Target identifies the single SeiNode this workflow operates on, with the
	// same requirePhase gating SeiNodeTask uses. Immutable after creation.
	Target SeiNodeTaskTarget `json:"target"`

	// StateSync is the payload for kind=StateSync. Immutable after creation.
	// +optional
	StateSync *StateSyncWorkflow `json:"stateSync,omitempty"`
}

// StateSyncWorkflow parameterizes the StateSync recipe.
type StateSyncWorkflow struct {
	// Migration, when set, runs a named seid config migration inside this
	// destructive re-bootstrap: the planner materializes its typed parameters
	// into the config-patch step's TOML tree, applied after reset-data and
	// before configure-state-sync. Nil (the common case) is a plain
	// re-bootstrap that leaves config unchanged and omits the config-patch step
	// entirely. A ConfigMigration always means "a migration executed inside a
	// wipe" — a wipe-less migration belongs in a different workflow kind.
	// +optional
	Migration *ConfigMigration `json:"migration,omitempty"`

	// RpcServers are the CometBFT rpc-servers used as light-client witnesses
	// (trust-point acquisition and verification) for the resync. Witnesses are
	// NOT snapshot providers: snapshot chunks are delivered over p2p by
	// snapshot-serving peers; rpcServers verify the trust point only, so at
	// least two distinct endpoints are required for the light-client
	// cross-check. When empty, the node's resolved state-syncers
	// (node.status.resolvedStateSyncers) are used and the two-witness
	// fail-closed floor still holds. Endpoints are bare host:port (no scheme,
	// no IPv6 literal), matching the CometBFT rpc_servers key and the
	// SnapshotSource.RpcServers shape.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:items:Pattern=`^[^\s:/,]+:[0-9]{1,5}$`
	RpcServers []string `json:"rpcServers,omitempty"`
}

// ConfigMigrationKind discriminates the ConfigMigration union. Exactly one
// matching payload sub-struct must be set. Each variant is a real, reviewed
// seid migration (cf. sei-chain docs/migration/*), grounded against its own
// chain-side doc; a half-specified variant is worse than an absent one.
// +kubebuilder:validation:Enum=GigaStore
type ConfigMigrationKind string

const (
	// ConfigMigrationGigaStore is the giga SS-store migration
	// (sei-chain docs/migration/giga_store_migration.md): it splits hot EVM
	// state into a dedicated state-store DB, which requires a full state-sync
	// re-bootstrap into the new layout. RPC nodes only.
	ConfigMigrationGigaStore ConfigMigrationKind = "GigaStore"
)

// ConfigMigration is a typed, discriminated seid config migration executed
// inside the StateSync re-bootstrap. It enshrines the migration (a recurring
// operation), not each config value: the fixed flags a migration sets are
// pinned in controller code against the chain-side migration doc. Only Backend
// (for GigaStore) is an operator input; the flags the migration flips are
// materialized into status.plan (spec = intent, status.plan = the applied
// TOML keys).
//
// The CEL rules below fire only when Migration is non-nil, so a nil migration
// (plain re-bootstrap) bypasses them. Per-field immutability is intentionally
// omitted: the parent's `self.stateSync == oldSelf.stateSync` already freezes
// this whole subtree.
//
// +kubebuilder:validation:XValidation:rule="(has(self.gigaStore) ? 1 : 0) == 1",message="exactly one migration payload (gigaStore) must be set"
// +kubebuilder:validation:XValidation:rule="self.kind != 'GigaStore' || has(self.gigaStore)",message="spec.stateSync.migration.gigaStore is required when kind=GigaStore"
type ConfigMigration struct {
	// Kind selects the migration variant. The matching payload must be set.
	Kind ConfigMigrationKind `json:"kind"`

	// GigaStore is the payload for kind=GigaStore.
	// +optional
	GigaStore *GigaStoreMigration `json:"gigaStore,omitempty"`
}

// GigaStoreMigration parameterizes the giga SS-store migration. Backend is the
// only operator input; the migration itself sets the fixed enabling flags
// (pinned to giga_store_migration.md): app.toml [state-store] ss-enable=true,
// evm-ss-split=true and [state-commit] sc-enable=true. Those flags are not
// operator-visible knobs — they are observable after the fact in status.plan.
type GigaStoreMigration struct {
	// Backend is the DBBackend for the Cosmos SS MVCC DB and every EVM SS
	// sub-DB, mapped to app.toml [state-store] ss-backend. rocksdb additionally
	// requires a seid image built with -tags rocksdbBackend.
	// +kubebuilder:default=pebbledb
	// +kubebuilder:validation:Enum=pebbledb;rocksdb
	Backend string `json:"backend,omitempty"`
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// SeiNodeTaskWorkflowStatus is the observed state, written solely by the
// SeiNode controller (single writer). Plan is authoritative for how far the
// recipe has progressed; the node's status.adoptedWorkflow pointer is
// authoritative for which node owns it — orthogonal facts, so nothing mirrors
// and nothing diverges.
type SeiNodeTaskWorkflowStatus struct {
	// ObservedGeneration is the spec generation the controller last acted on.
	// Stamped explicitly by the node controller (the plan executor does not
	// touch it).
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the high-level lifecycle state.
	// +optional
	Phase SeiNodeTaskWorkflowPhase `json:"phase,omitempty"`

	// TargetFirstObservedAt is when the node first observed this workflow while
	// the target was not yet in the required phase. It anchors the
	// spec.target.requirePhaseTimeout budget; cleared once the target is met.
	// +optional
	TargetFirstObservedAt *metav1.Time `json:"targetFirstObservedAt,omitempty"`

	// Plan is the compiled recipe — the same TaskPlan the node/network
	// controllers persist, driven by the generic plan executor. TargetPhase
	// and FailedPhase are always empty: a workflow never drives a node phase.
	// +optional
	Plan *TaskPlan `json:"plan,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ---------------------------------------------------------------------------
// Root object
// ---------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sntw
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.nodeRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SeiNodeTaskWorkflow composes existing node tasks into a named, multi-step
// recipe against a single SeiNode. It is a request object with no controller
// of its own; the SeiNode controller adopts and executes it.
type SeiNodeTaskWorkflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SeiNodeTaskWorkflowSpec   `json:"spec,omitempty"`
	Status SeiNodeTaskWorkflowStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SeiNodeTaskWorkflowList contains a list of SeiNodeTaskWorkflow.
type SeiNodeTaskWorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SeiNodeTaskWorkflow `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SeiNodeTaskWorkflow{}, &SeiNodeTaskWorkflowList{})
}
