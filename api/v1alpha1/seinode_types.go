package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// SeiNodeSpec defines the desired state of a standalone Sei node.
// Exactly one mode sub-spec (fullNode, archive, replayer, validator, seed) must
// be set; the populated field determines the node's operating mode.
//
// The last rule below reduces both freeze-capable modes to a single effective
// freeze height, 0 when unfrozen, and pins it across updates. It reads dense, and
// it has to live here rather than on the mode sub-specs: a transition rule is
// skipped when its path is absent from the stored object, so a rule on the
// OPTIONAL fullNode/archive parent never fires for a mode switch. Written per
// mode, swapping an unfrozen fullNode for a frozen archive is admitted, and so is
// swapping between two frozen modes at different heights. Both leave a Running
// node carrying the frozen readiness probe with no freeze height in app.toml,
// because only a bootstrap plan writes config.
// +kubebuilder:validation:XValidation:rule="(has(self.fullNode) ? 1 : 0) + (has(self.archive) ? 1 : 0) + (has(self.replayer) ? 1 : 0) + (has(self.validator) ? 1 : 0) + (has(self.seed) ? 1 : 0) == 1",message="exactly one of fullNode, archive, replayer, validator, or seed must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.replayer) || (has(self.peers) && size(self.peers) > 0)",message="peers is required when replayer mode is set"
// +kubebuilder:validation:XValidation:rule="!has(self.overrides) || !('chain.freeze_height' in self.overrides)",message="set the freeze height via fullNode.freeze or archive.freeze, not overrides: user overrides outrank controller-derived ones"
// +kubebuilder:validation:XValidation:rule="!((has(self.fullNode) && has(self.fullNode.freeze)) || (has(self.archive) && has(self.archive.freeze))) || !has(self.overrides) || (!('chain.halt_height' in self.overrides) && !('chain.halt_time' in self.overrides))",message="a frozen node cannot also set chain.halt_height or chain.halt_time: seid refuses to load the combination"
// +kubebuilder:validation:XValidation:rule="(has(self.fullNode) && has(self.fullNode.freeze) ? self.fullNode.freeze.height : (has(self.archive) && has(self.archive.freeze) ? self.archive.freeze.height : 0)) == (has(oldSelf.fullNode) && has(oldSelf.fullNode.freeze) ? oldSelf.fullNode.freeze.height : (has(oldSelf.archive) && has(oldSelf.archive.freeze) ? oldSelf.archive.freeze.height : 0))",message="the effective freeze height is create-only: it cannot be added, removed, or changed on an existing node, including by switching mode; replace the node instead"
// dataVolume.storage size is create-only: presence parity here (a sub-type rule
// skips a first-time set), value on the sub-type. Size only — import still adds.
// +kubebuilder:validation:XValidation:rule="((has(self.dataVolume) && has(self.dataVolume.storage) && has(self.dataVolume.storage.resources) && has(self.dataVolume.storage.resources.requests) && ('storage' in self.dataVolume.storage.resources.requests)) == (has(oldSelf.dataVolume) && has(oldSelf.dataVolume.storage) && has(oldSelf.dataVolume.storage.resources) && has(oldSelf.dataVolume.storage.resources.requests) && ('storage' in oldSelf.dataVolume.storage.resources.requests)))",message="spec.dataVolume.storage.resources.requests.storage is create-only: it cannot be added to or removed from an existing node — the data PVC is created once (ensure-data-pvc is Get-then-Create with no update path), so the edit would be inert; delete and recreate the node to resize"
// The VAC selection's presence half — its own rule rather than a term on the
// size rule above, so a rejection names the field the operator actually edited.
// COMPLETENESS: a new DataVolume* field needs BOTH a value rule on its sub-type
// and a presence term at spec level, on both Kinds.
// +kubebuilder:validation:XValidation:rule="((has(self.dataVolume) && has(self.dataVolume.storage) && has(self.dataVolume.storage.volumeAttributesClassName)) == (has(oldSelf.dataVolume) && has(oldSelf.dataVolume.storage) && has(oldSelf.dataVolume.storage.volumeAttributesClassName)))",message="spec.dataVolume.storage.volumeAttributesClassName is create-only: it cannot be added to or removed from an existing node — the data PVC is created once (ensure-data-pvc is Get-then-Create with no update path) and the VAC name binds there, so the edit would be inert; delete and recreate the node to reselect"
// resources is create-only, but compared PER-DIMENSION through quantity() — NOT
// structural == on the object. The values are int-or-string Quantities, so a
// node applied with a bare int (cpu: 4) stores an int, while the controller's
// own typed Update (finalizer install) re-encodes it as the string "4"; a
// structural == would read int 4 != string "4", reject the controller's write,
// and wedge the node on its first reconcile. quantity(string(...)).compareTo
// compares the values, so a re-encode is a no-op while a real change is caught.
// limits is not compared here: the equality rule already pins limits.memory to
// requests.memory, and the controller derives the limit from the request, so the
// footprint is frozen by freezing requests.
// +kubebuilder:validation:XValidation:rule="(!has(self.resources) && !has(oldSelf.resources)) || (has(self.resources) && has(oldSelf.resources) && (has(self.resources.requests) == has(oldSelf.resources.requests)) && (!has(self.resources.requests) || ((('cpu' in self.resources.requests) == ('cpu' in oldSelf.resources.requests)) && (('memory' in self.resources.requests) == ('memory' in oldSelf.resources.requests)) && (!('cpu' in self.resources.requests) || !('cpu' in oldSelf.resources.requests) || quantity(string(self.resources.requests['cpu'])).compareTo(quantity(string(oldSelf.resources.requests['cpu']))) == 0) && (!('memory' in self.resources.requests) || !('memory' in oldSelf.resources.requests) || quantity(string(self.resources.requests['memory'])).compareTo(quantity(string(oldSelf.resources.requests['memory']))) == 0))))",message="spec.resources is create-only: the footprint is fixed at creation (a change is not rolled onto a running pod — the StatefulSet is OnDelete and drift detection is image-only), so replace the node to resize"
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
	// storage class and size; dataVolume.storage overrides the size (see
	// noderesource.StorageForNode).
	// +optional
	DataVolume *DataVolumeSpec `json:"dataVolume,omitempty"`

	// Resources overrides the seid-container footprint for this node. It is the
	// HIGHEST-precedence sizing source: it outranks the app-config
	// resources.<mode> override, which in turn outranks the per-mode code
	// default (see noderesource.ResourcesForNode for the full ladder).
	//
	// Resolution is per-dimension, not all-or-nothing: setting only the CPU
	// request leaves memory on whichever lower source supplies it. That keeps a
	// benchmark operator from having to restate a mode's whole footprint to
	// raise one axis.
	//
	// Immutable after creation (spec-level CEL). A change here would not reach a
	// running pod anyway — the StatefulSets use UpdateStrategy: OnDelete and
	// drift detection is image-only — so admission rejects the edit rather than
	// accept an inert one. Replace the node to resize.
	// +optional
	Resources *Resources `json:"resources,omitempty"`

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

	// Seed configures a peer-discovery seed node (P2P + PEX only).
	// +optional
	Seed *SeedSpec `json:"seed,omitempty"`

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

// Resources overrides the seid-container footprint in pod-resource shape.
// Narrow by design (not corev1.ResourceRequirements) so admission accepts only
// cpu/memory. The CEL rules pin the per-mode couplings — no CPU limit, memory
// limit == request, positive values — and reject a bad value by name at apply
// time. Keep the quantity(string(...)) form; see the envtest cases for why ==
// on a raw string would be wrong.
//
// +kubebuilder:validation:XValidation:rule="!has(self.requests) || self.requests.all(k, k in ['cpu', 'memory'])",message="resources.requests accepts only cpu and memory"
// +kubebuilder:validation:XValidation:rule="!has(self.limits) || self.limits.all(k, k == 'memory')",message="resources.limits accepts only memory: seid deliberately carries no CPU limit"
// +kubebuilder:validation:XValidation:rule="(!has(self.requests) || !('cpu' in self.requests) || quantity(string(self.requests['cpu'])).compareTo(quantity('0')) > 0) && (!has(self.requests) || !('memory' in self.requests) || quantity(string(self.requests['memory'])).compareTo(quantity('0')) > 0)",message="resources.requests values must be positive"
// +kubebuilder:validation:XValidation:rule="!has(self.limits) || !('memory' in self.limits) || (has(self.requests) && 'memory' in self.requests && quantity(string(self.limits['memory'])).compareTo(quantity(string(self.requests['memory']))) == 0)",message="resources.limits.memory must equal resources.requests.memory (the mode's memory-Guaranteed footprint)"
type Resources struct {
	// Requests is the seid container's resource request. Only cpu and memory
	// are accepted.
	// +optional
	Requests corev1.ResourceList `json:"requests,omitempty"`

	// Limits accepts only memory, pinned equal to the request by CEL. The
	// controller derives the limit from the request, so it is redundant but
	// kept as a served field — do not remove.
	// +optional
	Limits corev1.ResourceList `json:"limits,omitempty"`
}

// DataVolumeSpec configures how the data PVC is sourced.
//
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.import) || has(self.import))",message="import cannot be unset once configured"
// +kubebuilder:validation:XValidation:rule="!(has(self.import) && has(self.storage))",message="dataVolume.storage and dataVolume.import are mutually exclusive: an imported PVC keeps the importer's class and size, and the controller never mutates it"
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

	// Storage configures the volume the controller provisions. Excludes Import.
	// +optional
	Storage *DataVolumeStorage `json:"storage,omitempty"`
}

// DataVolumeStorage carries the size in the volume-claim shape and the storage
// performance selection. Both are create-only: value rule here, presence half on
// the spec. The size compares via quantity(), not ==, because a typed re-encode
// of an int-or-string Quantity would reject the controller's write; the VAC name
// is a plain string, so == is safe for it.
//
// +kubebuilder:validation:XValidation:rule="!has(self.resources) || (has(self.resources.requests) && 'storage' in self.resources.requests)",message="dataVolume.storage.resources must carry resources.requests.storage: an empty or null storage request would silently provision the per-mode default while reading as a size request"
// +kubebuilder:validation:XValidation:rule="!has(self.volumeAttributesClassName) || !has(oldSelf.volumeAttributesClassName) || self.volumeAttributesClassName == oldSelf.volumeAttributesClassName",message="dataVolume.storage.volumeAttributesClassName is create-only: the data PVC is created once (ensure-data-pvc is Get-then-Create with no update path) and the VAC name binds at provision, so a later edit could never reach the volume; delete and recreate the owning resource (the node, or the SeiNetwork for a pooled validator) to reselect"
// +kubebuilder:validation:XValidation:rule="!has(self.resources) || !has(self.resources.requests) || !('storage' in self.resources.requests) || !has(oldSelf.resources) || !has(oldSelf.resources.requests) || !('storage' in oldSelf.resources.requests) || quantity(string(self.resources.requests['storage'])).compareTo(quantity(string(oldSelf.resources.requests['storage']))) == 0",message="dataVolume.storage.resources.requests.storage is create-only: the data PVC is created once (ensure-data-pvc is Get-then-Create with no update path), so a later size edit could never reach the volume; delete and recreate the owning resource (the node, or the SeiNetwork for a pooled validator) to resize"
// +kubebuilder:validation:XValidation:rule="!has(self.resources) || !has(self.resources.requests) || self.resources.requests.all(k, k == 'storage')",message="dataVolume.storage.resources.requests accepts only storage"
// +kubebuilder:validation:XValidation:rule="!has(self.resources) || !has(self.resources.requests) || !('storage' in self.resources.requests) || quantity(string(self.resources.requests['storage'])).compareTo(quantity('0')) > 0",message="dataVolume.storage.resources.requests.storage must be positive"
type DataVolumeStorage struct {
	// Resources is the volume claim request. The size overrides the per-mode
	// default; unset falls through.
	// +optional
	Resources *VolumeClaimResources `json:"resources,omitempty"`

	// VolumeAttributesClassName selects the volume's performance parameters
	// (for gp3: IOPS and throughput) as a platform-managed, cluster-scoped
	// VolumeAttributesClass, referenced by NAME — the field mirrors the PVC
	// field of the same name. The controller reads the named class to pre-flight
	// it (see the VolumeAttributesClassReady condition) and stamps the name onto
	// the PVC it provisions; it never creates one. The catalog is platform-owned
	// (GitOps), so a novel (IOPS, throughput) point is a platform change, not a
	// controller or CRD change.
	//
	// Unset means the PVC carries no volumeAttributesClassName at all and the
	// mode-default StorageClass supplies the baseline performance. This is a
	// distinct PVC field from storageClassName, which the controller always sets
	// from the per-mode default. There is no app-config rung for this name.
	//
	// Covers controller-provisioned volumes only: dataVolume.import is mutually
	// exclusive with dataVolume.storage, and an imported PVC keeps the
	// importer's parameters.
	//
	// Create-only (a value rule here, the presence half at spec level, on both
	// Kinds): the PVC is provisioned once, so a change, an unset, and a
	// first-time set are all rejected — the edit could never reach the volume.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	VolumeAttributesClassName *string `json:"volumeAttributesClassName,omitempty"`
}

// VolumeClaimResources is request-only, narrow rather than
// corev1.VolumeResourceRequirements: a claim has no limit dimension.
type VolumeClaimResources struct {
	// Requests carries only the storage size. A map, not a struct, because a
	// struct prunes a misspelled or empty key and silently defaults the size.
	// +optional
	Requests corev1.ResourceList `json:"requests,omitempty"`
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

// Freeze returns the FreezeSpec from whichever mode sub-spec is populated, or
// nil when the node is not frozen. Only fullNode and archive carry the field:
// seid refuses to freeze a validator, and a seed serves no query RPC.
func (s *SeiNodeSpec) Freeze() *FreezeSpec {
	switch {
	case s.FullNode != nil:
		return s.FullNode.Freeze
	case s.Archive != nil:
		return s.Archive.Freeze
	default:
		return nil
	}
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

// NodeKeySecret returns the Secret supplying this node's P2P identity
// (node_key.json), or nil when the node has none and `seid init` generates one
// onto the data volume.
//
// Two modes carry a node key on differing terms — a validator's is optional and
// paired with a signing key, a seed's is required and standalone — so each
// sub-spec declares its own field rather than hoisting one to SeiNodeSpec, out of
// reach of the validator's key-pairing CEL rules. This accessor resolves that
// split in one place, so callers needing only "which Secret holds the node key"
// stay mode-blind. Mirrors SnapshotSource.
func (s *SeiNodeSpec) NodeKeySecret() *SecretNodeKeySource {
	switch {
	case s.Validator != nil && s.Validator.NodeKey != nil:
		return s.Validator.NodeKey.Secret
	case s.Seed != nil:
		return s.Seed.NodeKey.Secret
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

	// ConditionVolumeAttributesClassReady reports the read-only pre-flight of
	// spec.dataVolume.storage.volumeAttributesClassName.
	//
	// Always-present: the node reconciler resolves it on EVERY reconcile, before
	// the Failed and Paused early-returns and independently of whether any plan
	// or task runs — so a Failed, Paused, state-sync-gated, or steady-state
	// Running node (which builds no plan at all, including one that predates this
	// field) still carries it. It is NOT conditioned on ensure-data-pvc having
	// run; that task only consumes the condition to hold provisioning.
	//
	// HOW TO READ THE REASON. Three semantics, not two — a runbook or PromQL
	// alert keyed on status alone cannot tell the middle one from the last:
	//
	//   - True/VolumeAttributesClassFound and True/ModeDefaultStorage: HEALTHY.
	//     A class was selected and exists, or none was selected and the
	//     mode-default storage supplies the volume's performance.
	//   - False/NotApplicable: the controller does not own this volume's
	//     parameters at all (an imported PVC keeps the importer's). Inapplicable,
	//     not broken.
	//   - Any other False/<reason>: BROKEN. The selection names a class the
	//     cluster does not have, or the read failed.
	//
	// The True on no selection is a deliberate departure from the LITERAL reading
	// of the Conditions standard in CLAUDE.md, which says a <Subject>Ready type
	// uses False/<reason> for both "not yet ready" and "not configured". DR-001
	// mandates the departure in as many words
	// (docs/specs/001-configurable-node-resources/decisions.md:51-59): present
	// even when no VAC is selected, True in that steady state, never absence.
	// It is consistent with the rest of the same standard, which says True is the
	// desired steady state for this family: the standard's "not configured"
	// example is a feature that is OFF (NetworkingDisabled, spec.networking
	// unset), whereas an unset selection here is a real choice, not a gap —
	// decisions.md:75-78 has the PVC carry no volumeAttributesClassName at all
	// and the mode-default StorageClass supply the baseline performance, and the
	// volume provisions correctly. Nothing is degraded or disabled. The import
	// branch is the genuinely inapplicable case and is the one rendered
	// False/NotApplicable, per the standard. Two reviewers independently derived
	// a conflict from the code alone, which is why the reasoning is recorded here
	// rather than in a commit message.
	//
	// What False/VolumeAttributesClassNotFound does and does NOT prevent. It
	// holds PROVISIONING: ensure-data-pvc creates no claim while this condition
	// is not True, and adding the class (a platform/GitOps change) lets the next
	// poll proceed. It does not stop the pod. The initial-StatefulSet hold keys
	// on the state-sync gate only, so the StatefulSet is applied on the same
	// reconcile and its pod sits Pending on the claim nothing has created yet.
	// What the condition buys is that the Pending is not SILENT — its cause is
	// named right here, which is the defect DR-001 commits against
	// (decisions.md:51-56); the Pending pod itself is not.
	//
	// True reports a best-effort existence check at the last reconcile, not a
	// binding guarantee: the class can be deleted afterwards, and the controller
	// reads a cache that may lag. It is also EXISTENCE only — nothing compares
	// the class's driver against the CSI provisioner of the StorageClass the same
	// claim gets (see reconcileVolumeAttributesClass for why). Treat it as the
	// pre-flight's verdict on the selection, not as a promise about the volume.
	ConditionVolumeAttributesClassReady = "VolumeAttributesClassReady"

	// ConditionSigningKeyReady indicates whether a referenced validator
	// signing-key Secret passes all validation requirements. Only set on
	// SeiNodes with spec.validator.signingKey.
	ConditionSigningKeyReady = "SigningKeyReady"

	// ConditionNodeKeyReady indicates whether a referenced node-key Secret
	// passes all validation requirements. Only set on SeiNodes that source a
	// node key from a Secret — spec.validator.nodeKey or spec.seed.nodeKey.
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

// Reasons for the VolumeAttributesClassReady condition.
const (
	// ReasonVolumeAttributesClassFound: the selected class exists; provisioning
	// may stamp its name onto the PVC.
	ReasonVolumeAttributesClassFound = "VolumeAttributesClassFound"
	// ReasonModeDefaultStorage: no class is selected, so the mode-default storage
	// supplies the baseline performance (decisions.md:75-78). A True steady state
	// — the no-selection case is reported, not left absent.
	//
	// Named for what IS in force, not for what is absent. The earlier
	// "NoVolumeAttributesClass" was accurate but read as a fault and sorted
	// straight into the same family as VolumeAttributesClassNotFound below, which
	// is the one thing a consumer grouping this condition by reason must not
	// conflate: one is healthy, the other is broken. Reasons are a stable public
	// API, so this was renamed before the field shipped and is fixed now.
	ReasonModeDefaultStorage = "ModeDefaultStorage"
	// ReasonVolumeAttributesClassNotFound: the selected class is not in the
	// cluster. Transient by intent — the platform adds it (GitOps) and the next
	// poll proceeds — so the message names the class the operator must add.
	ReasonVolumeAttributesClassNotFound = "VolumeAttributesClassNotFound"
	// ReasonVolumeAttributesClassLookupError: reading the class failed for a
	// reason other than absence, including a cluster that does not serve
	// storage.k8s.io VolumeAttributesClasses at all (transient: retry).
	ReasonVolumeAttributesClassLookupError = "VolumeAttributesClassLookupError"
	// ReasonVolumeAttributesClassNotApplicable: the node imports its data volume,
	// which keeps the importer's parameters, so there is no selection to
	// pre-flight. Reported rather than left absent (see the Conditions standard).
	ReasonVolumeAttributesClassNotApplicable = "NotApplicable"
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
