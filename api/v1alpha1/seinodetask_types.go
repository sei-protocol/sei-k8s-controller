package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SeiNodeTaskKind discriminates the SeiNodeTask spec union. Exactly one of
// the matching payload sub-structs in SeiNodeTaskSpec must be set.
// +kubebuilder:validation:Enum=GovSoftwareUpgrade;GovVote;AwaitCondition;UpdateNodeImage;AwaitNodesAtHeight
type SeiNodeTaskKind string

const (
	// SeiNodeTaskKindGovSoftwareUpgrade backs the sidecar `gov-software-upgrade`
	// task. Submits MsgSubmitProposal for an upgrade plan from the configured
	// operator key. See the REHYDRATION WARNING on the sidecar handler before
	// composing with retry loops.
	SeiNodeTaskKindGovSoftwareUpgrade SeiNodeTaskKind = "GovSoftwareUpgrade"

	// SeiNodeTaskKindGovVote backs the sidecar `gov-vote` task. Submits
	// MsgVote on an existing proposal. Chain-idempotent (last-write-wins on
	// proposalId/voter).
	SeiNodeTaskKindGovVote SeiNodeTaskKind = "GovVote"

	// SeiNodeTaskKindAwaitCondition backs the sidecar `await-condition` task.
	// Polls a local node until a typed condition (e.g. height) is satisfied,
	// optionally executing a post-condition action.
	SeiNodeTaskKindAwaitCondition SeiNodeTaskKind = "AwaitCondition"

	// SeiNodeTaskKindUpdateNodeImage backs the controller-side
	// `update-node-image` task. Patches the target SeiNode's spec.image and
	// completes when status.currentImage observes the new image — no
	// readiness check, by design (see LLD).
	SeiNodeTaskKindUpdateNodeImage SeiNodeTaskKind = "UpdateNodeImage"

	// SeiNodeTaskKindAwaitNodesAtHeight backs the controller-side
	// `await-nodes-at-height` task. For SeiNodeTask, fan-out is single-node
	// (target.nodeRef); the task submits await-condition(height=H) to the
	// target's sidecar and completes when the local height crosses H.
	SeiNodeTaskKindAwaitNodesAtHeight SeiNodeTaskKind = "AwaitNodesAtHeight"
)

// SeiNodeTaskPhase is the high-level lifecycle state of a SeiNodeTask.
// +kubebuilder:validation:Enum=Pending;Running;Complete;Failed
type SeiNodeTaskPhase string

const (
	SeiNodeTaskPhasePending  SeiNodeTaskPhase = "Pending"
	SeiNodeTaskPhaseRunning  SeiNodeTaskPhase = "Running"
	SeiNodeTaskPhaseComplete SeiNodeTaskPhase = "Complete"
	SeiNodeTaskPhaseFailed   SeiNodeTaskPhase = "Failed"
)

// SeiNodeTask condition types.
const (
	// ConditionSeiNodeTaskReady reflects whether the task has reached a
	// terminal successful state. True only when status.phase == Complete.
	// Load-bearing for `kubectl wait --for=condition=Ready=true` in the
	// seitask-runner.
	ConditionSeiNodeTaskReady = "Ready"

	// ConditionSeiNodeTaskFailed reflects whether the task has reached a
	// terminal failure state. True only when status.phase == Failed.
	// Load-bearing for `kubectl wait --for=condition=Failed=true` in
	// verifier-style scenarios.
	ConditionSeiNodeTaskFailed = "Failed"

	// ConditionSeiNodeTaskTargetReady reflects whether the target SeiNode
	// satisfies spec.target.requirePhase. Reason indicates why (Resolving,
	// PhaseMet, PhaseNotMet, ResolveTimeout).
	ConditionSeiNodeTaskTargetReady = "TargetReady"
)

// ---------------------------------------------------------------------------
// Spec
// ---------------------------------------------------------------------------

// SeiNodeTaskSpec defines the desired state of a SeiNodeTask.
//
// Exactly one payload sub-spec matching spec.kind must be set; the CEL rule
// below mirrors SeiNodeSpec.fullNode|archive|replayer|validator.
//
// Field names locked at v1alpha1 — see docs/design/seinode-task-lld.md
// (PR sei-protocol/sei-k8s-controller#277).
//
// +kubebuilder:validation:XValidation:rule="(has(self.govSoftwareUpgrade) ? 1 : 0) + (has(self.govVote) ? 1 : 0) + (has(self.awaitCondition) ? 1 : 0) + (has(self.updateNodeImage) ? 1 : 0) + (has(self.awaitNodesAtHeight) ? 1 : 0) == 1",message="exactly one of govSoftwareUpgrade, govVote, awaitCondition, updateNodeImage, or awaitNodesAtHeight must be set"
// +kubebuilder:validation:XValidation:rule="self.kind != 'GovSoftwareUpgrade' || has(self.govSoftwareUpgrade)",message="spec.govSoftwareUpgrade is required when kind=GovSoftwareUpgrade"
// +kubebuilder:validation:XValidation:rule="self.kind != 'GovVote' || has(self.govVote)",message="spec.govVote is required when kind=GovVote"
// +kubebuilder:validation:XValidation:rule="self.kind != 'AwaitCondition' || has(self.awaitCondition)",message="spec.awaitCondition is required when kind=AwaitCondition"
// +kubebuilder:validation:XValidation:rule="self.kind != 'UpdateNodeImage' || has(self.updateNodeImage)",message="spec.updateNodeImage is required when kind=UpdateNodeImage"
// +kubebuilder:validation:XValidation:rule="self.kind != 'AwaitNodesAtHeight' || has(self.awaitNodesAtHeight)",message="spec.awaitNodesAtHeight is required when kind=AwaitNodesAtHeight"
// +kubebuilder:validation:XValidation:rule="self.kind == oldSelf.kind",message="spec.kind is immutable"
type SeiNodeTaskSpec struct {
	// Kind selects the task implementation. Immutable after creation.
	// The matching payload sub-spec (govSoftwareUpgrade, govVote, etc.)
	// must be set; all others must be unset.
	Kind SeiNodeTaskKind `json:"kind"`

	// Target identifies the single SeiNode this task operates on. Fan-out
	// targeting (label selectors) is intentionally out of scope at the CRD
	// layer — express fan-out at the seitask-runner / Chaos Workflow layer.
	Target SeiNodeTaskTarget `json:"target"`

	// TimeoutSeconds bounds the total time the reconciler will spend driving
	// this task to a terminal state, measured from first reconcile after
	// status.task is populated. When 0 (default), no upper bound is applied
	// and the task runs until it completes, fails terminally, or is deleted.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// --- Per-kind payload sub-specs (exactly one must be set) ---

	// GovSoftwareUpgrade is the payload for kind=GovSoftwareUpgrade.
	// +optional
	GovSoftwareUpgrade *GovSoftwareUpgradePayload `json:"govSoftwareUpgrade,omitempty"`

	// GovVote is the payload for kind=GovVote.
	// +optional
	GovVote *GovVotePayload `json:"govVote,omitempty"`

	// AwaitCondition is the payload for kind=AwaitCondition.
	// +optional
	AwaitCondition *AwaitConditionPayload `json:"awaitCondition,omitempty"`

	// UpdateNodeImage is the payload for kind=UpdateNodeImage.
	// +optional
	UpdateNodeImage *UpdateNodeImagePayload `json:"updateNodeImage,omitempty"`

	// AwaitNodesAtHeight is the payload for kind=AwaitNodesAtHeight.
	// +optional
	AwaitNodesAtHeight *AwaitNodesAtHeightPayload `json:"awaitNodesAtHeight,omitempty"`
}

// SeiNodeTaskTarget identifies the single SeiNode this task operates on.
// Selector-based fan-out is intentionally out of scope for MVP — express
// multi-node operations at the seitask-runner / Chaos Workflow layer.
type SeiNodeTaskTarget struct {
	// NodeRef is a same-namespace reference to a SeiNode.
	NodeRef SeiNodeTaskNodeRef `json:"nodeRef"`

	// RequirePhase is the SeiNode phase that must be observed on the target
	// before the task is dispatched. When the target is not in this phase
	// within RequirePhaseTimeout, the task is failed terminally.
	// +optional
	// +kubebuilder:default=Running
	RequirePhase SeiNodePhase `json:"requirePhase,omitempty"`

	// RequirePhaseTimeout bounds how long the reconciler waits for the
	// target to reach RequirePhase before failing the task. Default 5m.
	// +optional
	// +kubebuilder:default="5m"
	RequirePhaseTimeout *metav1.Duration `json:"requirePhaseTimeout,omitempty"`
}

// SeiNodeTaskNodeRef references a SeiNode in the same namespace as the
// owning SeiNodeTask. Cross-namespace targeting is out of scope.
type SeiNodeTaskNodeRef struct {
	// Name of the SeiNode in the same namespace as the SeiNodeTask.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
}

// ---------------------------------------------------------------------------
// Per-kind payload sub-specs
// ---------------------------------------------------------------------------

// GovSoftwareUpgradePayload mirrors
// seictl/sidecar/tasks/gov_software_upgrade.go::GovSoftwareUpgradeRequest.
// The sidecar handler enforces additional invariants (denom whitelist,
// keyring presence) at submit time — see that file for the canonical
// validation order.
type GovSoftwareUpgradePayload struct {
	// ChainID is the chain ID the proposal targets. Cross-checked against
	// the local node's reported chain ID by the sidecar.
	// +kubebuilder:validation:MinLength=1
	ChainID string `json:"chainId"`

	// KeyName names the keyring entry that signs the proposal.
	// +kubebuilder:validation:MinLength=1
	KeyName string `json:"keyName"`

	// Title is the on-chain proposal title.
	// +kubebuilder:validation:MinLength=1
	Title string `json:"title"`

	// Description is the on-chain proposal description.
	// +kubebuilder:validation:MinLength=1
	Description string `json:"description"`

	// UpgradeName is the upgrade plan name. Must match an upgrade handler
	// registered in the target binary.
	// +kubebuilder:validation:MinLength=1
	UpgradeName string `json:"upgradeName"`

	// UpgradeHeight is the block height at which the upgrade is scheduled.
	// +kubebuilder:validation:Minimum=1
	UpgradeHeight int64 `json:"upgradeHeight"`

	// UpgradeInfo is the optional Plan.Info string (e.g. binary checksums).
	// +optional
	UpgradeInfo string `json:"upgradeInfo,omitempty"`

	// InitialDeposit is the proposal deposit in coin notation (e.g.
	// "10000000usei"). Sei gov rejects non-usei denominations; the sidecar
	// pre-validates.
	// +kubebuilder:validation:MinLength=1
	InitialDeposit string `json:"initialDeposit"`

	// Memo is the optional tx memo. The sidecar appends a `taskID=<id>` tag
	// for on-chain audit; do not pre-tag.
	// +optional
	Memo string `json:"memo,omitempty"`

	// Fees is the tx fee in coin notation (e.g. "2000usei"). usei-only.
	// +kubebuilder:validation:MinLength=1
	Fees string `json:"fees"`

	// Gas is the tx gas limit.
	// +kubebuilder:validation:Minimum=1
	Gas uint64 `json:"gas"`
}

// GovVotePayload mirrors seictl/sidecar/tasks/gov_vote.go::GovVoteRequest.
type GovVotePayload struct {
	// ChainID is the chain ID the vote targets.
	// +kubebuilder:validation:MinLength=1
	ChainID string `json:"chainId"`

	// KeyName names the keyring entry that signs the vote.
	// +kubebuilder:validation:MinLength=1
	KeyName string `json:"keyName"`

	// ProposalID is the on-chain proposal ID being voted on.
	// +kubebuilder:validation:Minimum=1
	ProposalID uint64 `json:"proposalId"`

	// Option is the vote choice. Mirrors gov v1beta1 VoteOption parse rules.
	// +kubebuilder:validation:Enum=yes;no;abstain;no_with_veto;no-with-veto
	Option string `json:"option"`

	// Memo is the optional tx memo. The sidecar appends a `taskID=<id>` tag;
	// do not pre-tag.
	// +optional
	Memo string `json:"memo,omitempty"`

	// Fees is the tx fee in coin notation (e.g. "2000usei"). usei-only.
	// +kubebuilder:validation:MinLength=1
	Fees string `json:"fees"`

	// Gas is the tx gas limit.
	// +kubebuilder:validation:Minimum=1
	Gas uint64 `json:"gas"`
}

// AwaitConditionPayload is the await-condition payload. Today only the
// height condition is supported by the sidecar; the nested condition
// union shape exists so new condition kinds (proposalStatus, nodeRunning,
// panicAtHeight) can be added without breaking the CRD.
//
// +kubebuilder:validation:XValidation:rule="(has(self.height) ? 1 : 0) == 1",message="exactly one condition must be set (currently: height)"
type AwaitConditionPayload struct {
	// Height waits until the local node reports a latest height >=
	// targetHeight. Maps to sidecar await-condition with condition=height.
	// +optional
	Height *AwaitHeightCondition `json:"height,omitempty"`

	// Action is an optional post-condition action the sidecar performs
	// after the condition is met. Currently the only recognized value is
	// "SIGTERM_SEID", which sends SIGTERM to the local seid process.
	// Leave empty for a pure wait.
	// +optional
	// +kubebuilder:validation:Enum=SIGTERM_SEID
	Action string `json:"action,omitempty"`
}

// AwaitHeightCondition configures a height wait. Sidecar polls the local
// Tendermint /status endpoint until latestHeight >= targetHeight.
type AwaitHeightCondition struct {
	// TargetHeight is the block height the local node must reach.
	// +kubebuilder:validation:Minimum=1
	TargetHeight int64 `json:"targetHeight"`
}

// UpdateNodeImagePayload patches spec.image on the target SeiNode and
// completes when status.currentImage observes the new image. No readiness
// check — see LLD for the rationale (major-upgrade scenarios expect
// transient CrashLoop during early upgrade).
type UpdateNodeImagePayload struct {
	// Image is the desired seid container image (with tag/digest). Patched
	// onto target.spec.image via SSA with fieldOwner=seinode-task-controller.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image"`
}

// AwaitNodesAtHeightPayload waits until the target SeiNode's local height
// crosses targetHeight. The controller-side task submits an
// await-condition(height=H) to the target's sidecar and polls until done.
type AwaitNodesAtHeightPayload struct {
	// TargetHeight is the block height the target node must reach.
	// +kubebuilder:validation:Minimum=1
	TargetHeight int64 `json:"targetHeight"`
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// SeiNodeTaskStatus defines the observed state of a SeiNodeTask.
type SeiNodeTaskStatus struct {
	// ObservedGeneration is the most recent .metadata.generation observed
	// by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the high-level lifecycle state.
	// +optional
	Phase SeiNodeTaskPhase `json:"phase,omitempty"`

	// Task tracks the synthesized one-shot task that backs this CR. Populated
	// atomically on the first reconcile that successfully resolves the
	// target and mints the deterministic task ID. Once set, .task.id is
	// stable for the lifetime of the CR.
	// +optional
	Task *SeiNodeTaskExecution `json:"task,omitempty"`

	// Outputs surfaces typed per-kind results. Exactly one sub-field is
	// populated, matching spec.kind. Populated only on phase=Complete.
	// +optional
	Outputs *SeiNodeTaskOutputs `json:"outputs,omitempty"`

	// PhaseTransitionTime is when the task last changed phases.
	// +optional
	PhaseTransitionTime *metav1.Time `json:"phaseTransitionTime,omitempty"`

	// StartedAt is when the controller first observed the task (used as the
	// reference point for spec.timeoutSeconds).
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// TargetFirstObservedAt is when the controller first observed the target
	// SeiNode in a non-RequirePhase state (used as the reference point for
	// spec.target.requirePhaseTimeout). Cleared once the phase is met.
	// +optional
	TargetFirstObservedAt *metav1.Time `json:"targetFirstObservedAt,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SeiNodeTaskExecution is the controller's view of the one synthesized task
// backing this CR. The task ID is deterministic so a reconciler restart
// re-derives the same ID and rejoins the in-flight execution.
type SeiNodeTaskExecution struct {
	// ID is the deterministic UUID v5 used as the sidecar task ID.
	ID string `json:"id"`

	// Status is the lifecycle state of the underlying execution.
	Status TaskStatus `json:"status"`

	// Err is the error message if the task failed.
	// +optional
	Err string `json:"err,omitempty"`

	// SubmittedAt is when the task was first submitted to the sidecar.
	// Nil before submission.
	// +optional
	SubmittedAt *metav1.Time `json:"submittedAt,omitempty"`
}

// SeiNodeTaskOutputs holds typed per-kind results. Exactly one sub-field is
// populated, matching the spec union. Populated only on phase=Complete.
type SeiNodeTaskOutputs struct {
	// GovSoftwareUpgrade outputs for kind=GovSoftwareUpgrade.
	// +optional
	GovSoftwareUpgrade *GovSoftwareUpgradeOutputs `json:"govSoftwareUpgrade,omitempty"`

	// GovVote outputs for kind=GovVote.
	// +optional
	GovVote *GovVoteOutputs `json:"govVote,omitempty"`

	// AwaitCondition outputs for kind=AwaitCondition.
	// +optional
	AwaitCondition *AwaitConditionOutputs `json:"awaitCondition,omitempty"`

	// UpdateNodeImage outputs for kind=UpdateNodeImage.
	// +optional
	UpdateNodeImage *UpdateNodeImageOutputs `json:"updateNodeImage,omitempty"`

	// AwaitNodesAtHeight outputs for kind=AwaitNodesAtHeight.
	// +optional
	AwaitNodesAtHeight *AwaitNodesAtHeightOutputs `json:"awaitNodesAtHeight,omitempty"`
}

// GovSoftwareUpgradeOutputs are the typed results for a completed
// GovSoftwareUpgrade task.
type GovSoftwareUpgradeOutputs struct {
	// TxHash is the upper-case hex-encoded transaction hash.
	// +optional
	TxHash string `json:"txHash,omitempty"`

	// Height is the block height at which the tx was included, when
	// inclusion was observed by the sidecar.
	// +optional
	Height int64 `json:"height,omitempty"`

	// ProposalID is the on-chain proposal ID parsed from the inclusion
	// response events. Empty when the sidecar could not determine
	// inclusion before result-persist.
	// +optional
	ProposalID uint64 `json:"proposalId,omitempty"`
}

// GovVoteOutputs are the typed results for a completed GovVote task.
type GovVoteOutputs struct {
	// TxHash is the upper-case hex-encoded transaction hash.
	// +optional
	TxHash string `json:"txHash,omitempty"`

	// Height is the block height at which the tx was included.
	// +optional
	Height int64 `json:"height,omitempty"`
}

// AwaitConditionOutputs are the typed results for a completed
// AwaitCondition task.
type AwaitConditionOutputs struct {
	// ObservedValue is the value of the awaited quantity at the moment the
	// condition was satisfied. For condition=height, this is the latest
	// block height observed by the sidecar at the time it returned.
	// +optional
	ObservedValue int64 `json:"observedValue,omitempty"`
}

// UpdateNodeImageOutputs are the typed results for a completed
// UpdateNodeImage task.
type UpdateNodeImageOutputs struct {
	// AppliedImage is the image now observed on target.status.currentImage.
	// Equal to spec.updateNodeImage.image.
	// +optional
	AppliedImage string `json:"appliedImage,omitempty"`
}

// AwaitNodesAtHeightOutputs are the typed results for a completed
// AwaitNodesAtHeight task.
type AwaitNodesAtHeightOutputs struct {
	// CurrentHeight is the latest height observed on the target node at
	// task completion (i.e. >= spec.awaitNodesAtHeight.targetHeight).
	// +optional
	CurrentHeight int64 `json:"currentHeight,omitempty"`
}

// ---------------------------------------------------------------------------
// Root object
// ---------------------------------------------------------------------------

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=snt
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.nodeRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SeiNodeTask is the Schema for the seinodetasks API — a single-shot
// operation against a single SeiNode. See docs/design/seinode-task-lld.md
// (PR sei-protocol/sei-k8s-controller#277) for the interface contract.
type SeiNodeTask struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SeiNodeTaskSpec   `json:"spec,omitempty"`
	Status SeiNodeTaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SeiNodeTaskList contains a list of SeiNodeTask.
type SeiNodeTaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SeiNodeTask `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SeiNodeTask{}, &SeiNodeTaskList{})
}
