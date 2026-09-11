package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SeiNetworkSpec defines the desired state of a SeiNetwork.
//
// A SeiNetwork bootstraps a new Sei chain: a required genesis ceremony that
// mints the chain's genesis.json/genesisHash and its founding validator set.
// spec.genesis is mandatory — the Kind cannot exist without it. The replicas
// are the genesis validators; the ceremony generates a distinct identity
// (consensus key, P2P node key, operator account) for each one.
//
// Consumer note: editing spec.image (or other propagatable fields) on a
// running network rolls every genesis validator near-simultaneously, which
// briefly interrupts consensus. Drain in-flight load before an image bump.
//
// The spec is purpose-built for genesis bootstrap, not a SeiNodeSpec template.
// Follower/multi-role fields (signingKey, nodeKey, operatorKeyring, snapshot,
// peers, externalAddress, fullNode/archive/replayer) are structurally absent:
// supplying them describes a SeiNode, not a genesis validator pool. The
// controller synthesizes each child SeiNode's validator spec from these
// scalars (see generateSeiNode).
//
// +kubebuilder:validation:XValidation:rule="self.genesis == oldSelf.genesis",message="spec.genesis is immutable once set; the ceremony's outputs (chain ID, validator gentxs, account balances) are baked into chain state and cannot be retroactively rewritten by editing the spec"
// +kubebuilder:validation:XValidation:rule="self.replicas == oldSelf.replicas",message="spec.replicas is fixed at the genesis ceremony; the validator set is minted into genesis state and cannot be grown or shrunk by editing the spec"
// consensus is create-only on its EFFECTIVE value (absent == {engine: Tendermint,
// evmOnly: false}), so adding an explicit default to an existing object is an
// accepted no-op while a real engine or evmOnly change is caught.
// +kubebuilder:validation:XValidation:rule="(has(self.consensus) && has(self.consensus.engine) ? self.consensus.engine : 'Tendermint') == (has(oldSelf.consensus) && has(oldSelf.consensus.engine) ? oldSelf.consensus.engine : 'Tendermint')",message="spec.consensus.engine is create-only: the engine is baked into the ceremony's autobahn.json and every validator's home directory; recreate the network to change it"
// +kubebuilder:validation:XValidation:rule="(has(self.consensus) && has(self.consensus.evmOnly) ? self.consensus.evmOnly : false) == (has(oldSelf.consensus) && has(oldSelf.consensus.evmOnly) ? oldSelf.consensus.evmOnly : false)",message="spec.consensus.evmOnly is create-only: the application is baked into every validator's home directory; recreate the network to change it"
// +kubebuilder:validation:XValidation:rule="(has(self.consensus) && has(self.consensus.autobahn)) ? (has(oldSelf.consensus) && has(oldSelf.consensus.autobahn) && self.consensus.autobahn == oldSelf.consensus.autobahn) : !(has(oldSelf.consensus) && has(oldSelf.consensus.autobahn))",message="spec.consensus.autobahn is create-only: its values are written into the ceremony's autobahn.json, which every validator and follower already holds; recreate the network to change them"
// +kubebuilder:validation:XValidation:rule="!(has(self.consensus) && has(self.consensus.evmOnly) && self.consensus.evmOnly) || !has(self.configOverrides) || !('network.rpc.listen_address' in self.configOverrides || 'api.rest.enable' in self.configOverrides || 'api.grpc.enable' in self.configOverrides || 'api.grpc_web.enable' in self.configOverrides)",message="an EVM-only network owns network.rpc.listen_address and api.{rest,grpc,grpc_web}.enable: the EVM-only executor serves no CometBFT RPC, REST or gRPC"
// dataVolume is create-only (change, unset, first-time set all rejected).
// Presence parity only here; values are pinned on the shared DataVolume* types,
// covering both Kinds. A structural == cannot stay once a Quantity lives here (a
// typed re-encode would reject the controller's own write), and enumerating
// values — reaching import.pvcName — exceeds the CEL per-rule cost budget.
//
// COMPLETENESS: a new DataVolume* field is silently mutable until it gets BOTH a
// value rule on its sub-type AND a presence term here, on both Kinds.
// +kubebuilder:validation:XValidation:rule="((has(self.dataVolume)) == (has(oldSelf.dataVolume))) && ((has(self.dataVolume) && has(self.dataVolume.import)) == (has(oldSelf.dataVolume) && has(oldSelf.dataVolume.import))) && ((has(self.dataVolume) && has(self.dataVolume.storage)) == (has(oldSelf.dataVolume) && has(oldSelf.dataVolume.storage))) && ((has(self.dataVolume) && has(self.dataVolume.storage) && has(self.dataVolume.storage.resources) && has(self.dataVolume.storage.resources.requests) && ('storage' in self.dataVolume.storage.resources.requests)) == (has(oldSelf.dataVolume) && has(oldSelf.dataVolume.storage) && has(oldSelf.dataVolume.storage.resources) && has(oldSelf.dataVolume.storage.resources.requests) && ('storage' in oldSelf.dataVolume.storage.resources.requests))) && ((has(self.dataVolume) && has(self.dataVolume.storage) && has(self.dataVolume.storage.volumeAttributesClassName)) == (has(oldSelf.dataVolume) && has(oldSelf.dataVolume.storage) && has(oldSelf.dataVolume.storage.volumeAttributesClassName)))",message="spec.dataVolume is create-only: each validator's data PVC is created once (ensure-data-pvc is Get-then-Create with no update path) and nothing replaces a node on storage drift, so a later edit could never reach the pool's volumes; recreate the network to change its storage"
// resources is create-only, compared PER-DIMENSION via quantity() rather than
// structural == — the values are int-or-string Quantities, and the network
// controller's finalizer Update re-encodes a bare-int footprint as a string, so
// a structural == would reject the controller's own write and wedge the network
// (mirrors the SeiNode rule). limits rides on the equality rule + request-derived
// limit, so freezing requests freezes the footprint.
// +kubebuilder:validation:XValidation:rule="(!has(self.resources) && !has(oldSelf.resources)) || (has(self.resources) && has(oldSelf.resources) && (has(self.resources.requests) == has(oldSelf.resources.requests)) && (!has(self.resources.requests) || ((('cpu' in self.resources.requests) == ('cpu' in oldSelf.resources.requests)) && (('memory' in self.resources.requests) == ('memory' in oldSelf.resources.requests)) && (!('cpu' in self.resources.requests) || !('cpu' in oldSelf.resources.requests) || quantity(string(self.resources.requests['cpu'])).compareTo(quantity(string(oldSelf.resources.requests['cpu']))) == 0) && (!('memory' in self.resources.requests) || !('memory' in oldSelf.resources.requests) || quantity(string(self.resources.requests['memory'])).compareTo(quantity(string(oldSelf.resources.requests['memory']))) == 0))))",message="spec.resources is create-only: a validator pool is fixed-shape at the genesis ceremony, and each child SeiNode's own spec.resources is itself create-only, so an edit here could never reach the pool; replace the network to resize"
type SeiNetworkSpec struct {
	// Image is the seid image the genesis validators run.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Genesis is REQUIRED. The ceremony that bootstraps this chain.
	// genesis.chainId is the sole chain identity (no redundant top-level
	// chainId). The ceremony GENERATES every validator's identity (consensus
	// key, P2P node key, operator account) — there is no bring-your-own-key
	// path. Immutable once set; enforced by spec-level CEL.
	// +required
	Genesis GenesisCeremonyConfig `json:"genesis"`

	// Consensus selects the engine every validator runs and, under Autobahn,
	// tunes the ceremony's autobahn.json. Absent means Tendermint. Create-only;
	// engine and evmOnly propagate to every validator child.
	// +optional
	Consensus *NetworkConsensusSpec `json:"consensus,omitempty"`

	// Replicas is the number of genesis validators to create. Each gets a
	// DISTINCT generated identity, so replicas>1 is the normal safe case
	// (no shared key → no double-sign). Each SeiNode is named
	// "{network-name}-{ordinal}".
	//
	// FIXED at genesis: the validator set is minted into genesis state at the
	// ceremony, so replicas is immutable after create (spec-level CEL). You
	// cannot add a validator to an already-minted genesis, nor shrink the set.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas"`

	// ConfigOverrides are seid runtime config (config.toml/app.toml)
	// overrides, a flat map of dotted sei-config keys (e.g. "evm.http_port")
	// to string values. DISTINCT from genesis.overrides, which writes
	// genesis.json chain state — these are mutable node runtime config that
	// propagate to children in-place.
	// +optional
	ConfigOverrides map[string]string `json:"configOverrides,omitempty"`

	// ConfigValues supplies typed values by config file and dotted TOML path
	// for every validator in the pool. The controller copies the set onto each
	// validator child's spec.configValues, where the SeiNode substrate merges
	// it over the base configuration and restarts seid on a change; see the
	// SeiNode field for the merge, precedence, and restart behavior.
	//
	// The network's set is authoritative: an edit here rewrites every child's
	// set, and a direct edit on a child reconciles back. Like every other
	// propagated field, the rewrite is deferred while spec.paused is set or a
	// plan is in progress, and lands on the reconcile after that clears.
	//
	// The rewrite reaches the whole pool at once. There is no rolling window:
	// every validator sees the drift in the same reconcile and restarts seid
	// independently, so a one-key edit stops block production until more than
	// 2/3 of the set is back. There is no staged path for a network-owned pool:
	// a child edit reconciles back, and pausing defers the rewrite rather than
	// splitting it, so the edit has to be timed against a pool-wide restart.
	//
	// The set is validated here before it reaches any child: a value the CRD
	// admits but the TOML overlay cannot build — a null nested in a table, a
	// number outside int64 and float64, two overlapping dotted paths under one
	// fileName — leaves ConfigValuesValid=False on this object and every child
	// on its last good set, rather than wedging all of their plans at once.
	//
	// Deliberately unguarded, like the SeiNode field: no allow-list and no
	// denylist, so a config value may name chain.freeze_height, chain.halt_height,
	// or chain.halt_time across the whole validator set. Do not add a key guard
	// here without amending spec 003-config-substrate-parity-seinetwork.
	// +kubebuilder:validation:MaxItems=100
	// +optional
	// +listType=map
	// +listMapKey=fileName
	// +listMapKey=key
	ConfigValues []ConfigValue `json:"configValues,omitempty"`

	// DataVolume configures the data PersistentVolumeClaim for each genesis
	// validator. The ceremony-generated consensus identity lives here; set
	// DeletionPolicy to Retain on a pool whose identity must outlive the
	// SeiNetwork.
	//
	// Create-only (spec-level CEL): each child's PVC is created once and nothing
	// replaces a node on storage drift, so a later edit would be inert.
	// +optional
	DataVolume *DataVolumeSpec `json:"dataVolume,omitempty"`

	// Resources is the seid-container footprint every genesis validator in this
	// pool receives — one field sizes the whole pool, so the operator does not
	// restate it per replica. The controller stamps it onto each child SeiNode's
	// spec.resources at creation, where it becomes that node's
	// highest-precedence sizing source (see noderesource.ResourcesForNode).
	// Unset leaves every child on the app-config override or the per-mode code
	// default, unchanged.
	//
	// Immutable after create (spec-level CEL), for two reasons that compound.
	// The pool is fixed-shape at the genesis ceremony, like replicas. And the
	// child's own spec.resources is create-only too, so the controller
	// deliberately does not sync this field onto existing children (see
	// ensureSeiNode) — an edit here would be rejected downstream even if
	// admission let it through. Replace the network to resize the pool.
	// +optional
	Resources *Resources `json:"resources,omitempty"`

	// Scheduling configures worker-node isolation.
	// +optional
	Scheduling *SchedulingConfig `json:"scheduling,omitempty"`

	// Sidecar configures the sei-sidecar container on each genesis validator.
	// +optional
	Sidecar *SidecarConfig `json:"sidecar,omitempty"`

	// PodLabels are additional labels merged into each child SeiNode's pod
	// template. The controller always adds the reserved group labels — the
	// canonical sei.io/seinetwork{,-ordinal} keys, plus the frozen
	// sei.io/nodedeployment{,-ordinal} GitOps selector keys retained for
	// selector continuity; these are additive.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// Paused freezes plan-driven orchestration. While true, no new plans
	// start, no spec changes propagate to children, and any active plan
	// freezes in place. Children inherit the paused state.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// DeletionPolicy controls what happens to child SeiNodes when the
	// SeiNetwork is deleted. "Delete" (default) leaves the owner references in
	// place so Kubernetes garbage collection removes every child SeiNode, its
	// StatefulSet, and its pods. "Retain" orphans children so they continue
	// running independently — set it on a validator pool whose PVCs hold
	// ceremony-generated, unrecoverable consensus identity. Each retained child
	// is annotated with sei.io/retained-from-seinetwork and sei.io/retain-reason.
	// +optional
	// +kubebuilder:default=Delete
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// GenesisCeremonyConfig configures genesis ceremony orchestration for a network.
type GenesisCeremonyConfig struct {
	// ChainID for the new network.
	// Constrained to DNS-1123 label characters because child SeiNodes
	// compose it into P2P endpoint hostnames; the address is a one-way
	// door once peers cache it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	ChainID string `json:"chainId"`

	// StakingAmount is the amount each validator self-delegates in its gentx.
	// +optional
	// +kubebuilder:default="10000000usei"
	StakingAmount string `json:"stakingAmount,omitempty"`

	// AccountBalance is the initial balance for each validator's genesis account.
	// +optional
	// +kubebuilder:default="1000000000000000000000usei,1000000000000000000000uusdc,1000000000000000000000uatom"
	AccountBalance string `json:"accountBalance,omitempty"`

	// Accounts adds non-validator genesis accounts (e.g. for load test funding).
	// +optional
	Accounts []GenesisAccount `json:"accounts,omitempty"`

	// Overrides is a flat map of dotted snake_case key paths to JSON values,
	// merged on top of sei-config's GenesisDefaults() before gentx generation.
	// Keys follow cosmos JSON encoding (e.g. "staking.params.unbonding_time")
	// and values are arbitrary JSON (string, number, bool, object, array)
	// matching the type at that path in the underlying genesis schema.
	// +optional
	Overrides map[string]apiextensionsv1.JSON `json:"overrides,omitempty"`

	// ConsensusParams is one nested JSON object shaped like genesis.json's
	// top-level consensus_params (e.g. {"block": {"max_gas": "35000000"}}),
	// deep-merged over what seid init wrote after app_state overrides. It is the
	// typed route to a key overrides cannot reach: overrides address app_state
	// only. A null anywhere in the object is rejected at plan-build.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	ConsensusParams *apiextensionsv1.JSON `json:"consensusParams,omitempty"`

	// MaxCeremonyDuration is the maximum time from network creation to genesis
	// assembly completion. Default: "15m".
	// +optional
	MaxCeremonyDuration *metav1.Duration `json:"maxCeremonyDuration,omitempty"`
}

// GenesisAccount represents a non-validator genesis account to fund.
type GenesisAccount struct {
	// Address is the bech32-encoded account address.
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`

	// Balance is the initial balance in coin notation (e.g. "1000000usei").
	// +kubebuilder:validation:MinLength=1
	Balance string `json:"balance"`

	// Vesting, when set, locks part of Balance on an unlock schedule instead
	// of funding a fully-spendable account; nil funds a standard account.
	// The locked coins still count toward the balance and can be staked, but
	// cannot be transferred until they unlock.
	// +optional
	Vesting *GenesisAccountVesting `json:"vesting,omitempty"`
}

// GenesisAccountVesting locks part of a GenesisAccount's Balance on an unlock
// schedule that completes at EndTime.
type GenesisAccountVesting struct {
	// Amount is the vesting-locked portion of the account's Balance, in coin
	// notation (e.g. "1000000usei"). Must not exceed Balance.
	// +kubebuilder:validation:MinLength=1
	Amount string `json:"amount"`

	// EndTime is the unix timestamp at which the locked Amount fully unlocks.
	// Must be after the network's genesis time.
	// +kubebuilder:validation:Minimum=1
	EndTime int64 `json:"endTime"`

	// Delayed unlocks the full Amount all at once at EndTime. The default
	// (false) unlocks it linearly from the network's genesis time to EndTime.
	// +optional
	Delayed bool `json:"delayed,omitempty"`
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// SeiNetworkPhase represents the high-level lifecycle state.
// +kubebuilder:validation:Enum=Pending;Initializing;Ready;Paused;Degraded;Failed;Terminating
type SeiNetworkPhase string

const (
	GroupPhasePending      SeiNetworkPhase = "Pending"
	GroupPhaseInitializing SeiNetworkPhase = "Initializing"
	GroupPhaseReady        SeiNetworkPhase = "Ready"
	GroupPhasePaused       SeiNetworkPhase = "Paused"
	GroupPhaseDegraded     SeiNetworkPhase = "Degraded"
	GroupPhaseFailed       SeiNetworkPhase = "Failed"
	GroupPhaseTerminating  SeiNetworkPhase = "Terminating"
)

// SeiNetworkStatus defines the observed state of a SeiNetwork. The flat shape
// (top-level genesisHash + genesisS3URI, endpoints, perPodServices,
// internalService) is the consumer contract — skills parse this shape.
type SeiNetworkStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase is the high-level lifecycle state.
	Phase SeiNetworkPhase `json:"phase,omitempty"`

	// Replicas is the desired number of SeiNodes.
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of SeiNodes in Running phase.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// UpToDateReplicas is the number of child SeiNodes whose
	// status.currentImage matches spec.image. Derived each reconcile from the
	// child snapshot — no plan or revision tracking. When it lags Replicas a
	// child is mid-roll (or wedged on a bad image tag); the derived
	// RolloutInProgress condition mirrors this for `kubectl wait`.
	// +optional
	UpToDateReplicas int32 `json:"upToDateReplicas,omitempty"`

	// Nodes reports the status of each child SeiNode.
	// +listType=map
	// +listMapKey=name
	// +optional
	Nodes []GroupNodeStatus `json:"nodes,omitempty"`

	// Plan tracks the active network-level task plan (genesis assembly,
	// deployment, etc.). Nil when no plan is in progress.
	// +optional
	Plan *TaskPlan `json:"plan,omitempty"`

	// GenesisHash is the SHA-256 hex digest of the assembled genesis.json
	// (bare hex, no algorithm prefix). It gates the genesis download: a node
	// booting from the S3 fallback must verify the downloaded genesis.json
	// against this value.
	// +optional
	GenesisHash string `json:"genesisHash,omitempty"`

	// GenesisS3URI is the S3 URI of the uploaded genesis. Followers boot from
	// this URI.
	// +optional
	GenesisS3URI string `json:"genesisS3URI,omitempty"`

	// IncumbentNodes lists the names of the child SeiNode resources. Refreshed
	// each reconcile so the genesis planner can read the current node set
	// directly from the network object. NOT rollout state — purely the
	// ceremony's child-list feed.
	// +optional
	IncumbentNodes []string `json:"incumbentNodes,omitempty"`

	// InternalService reports the in-cluster ClusterIP Service that kube-proxy
	// load-balances across healthy child pods. Populated unconditionally.
	// +optional
	InternalService *InternalServiceStatus `json:"internalService,omitempty"`

	// PerPodServices lists the per-replica headless Services. Resolve each
	// at {name}.{namespace}.svc; pod IPs are not included.
	// +listType=map
	// +listMapKey=name
	// +optional
	PerPodServices []PerPodServiceStatus `json:"perPodServices,omitempty"`

	// Endpoints exposes composed in-cluster URLs derived from
	// .status.internalService and .status.perPodServices. When
	// .status.phase == Ready, .nodes is non-empty.
	// +optional
	Endpoints *Endpoints `json:"endpoints,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Endpoints lists composed in-cluster URLs for consuming this network.
// Aggregate URLs sit at top-level scalars; per-pod URLs live in Nodes,
// keyed by SeiNode name. When .status.phase == Ready, Nodes is non-empty
// and each entry has at least Name plus its protocol URLs populated.
//
// Stateless protocols (Tendermint RPC, Tendermint REST) are surfaced only
// at the aggregate level — kube-proxy round-robins safely. Stateful
// protocols (EVM JSON-RPC: filters, mempool, finalized-tag; EVM WebSocket:
// subscriptions) are surfaced only per-pod, because they do not
// load-balance correctly behind a kube-proxy L4 LB. Consumers that need
// state-consistent EVM sequences pin to a single Nodes[N].
type Endpoints struct {
	// TendermintRpc is the aggregate Tendermint / CometBFT RPC URL (http://).
	// +optional
	TendermintRpc string `json:"tendermintRpc,omitempty"`

	// TendermintRest is the aggregate Cosmos REST (LCD) URL (http://).
	// +optional
	TendermintRest string `json:"tendermintRest,omitempty"`

	// Nodes lists per-pod URL bundles, keyed by SeiNode name. The list
	// mirrors .status.perPodServices and exposes the protocols that
	// require pod affinity (EVM JSON-RPC, EVM WebSocket).
	// +listType=map
	// +listMapKey=name
	// +optional
	Nodes []NodeEndpoint `json:"nodes,omitempty"`
}

// NodeEndpoint is the per-pod URL bundle for a single SeiNode replica.
// Name matches the SeiNode resource name and the corresponding entry in
// .status.perPodServices.
type NodeEndpoint struct {
	// Name is the SeiNode resource name (also the headless Service name).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// EvmJsonRpc is the per-pod EVM JSON-RPC HTTP URL (http://).
	// +optional
	EvmJsonRpc string `json:"evmJsonRpc,omitempty"`

	// EvmWs is the per-pod EVM WebSocket URL (ws://).
	// +optional
	EvmWs string `json:"evmWs,omitempty"`
}

// InternalServiceStatus reports the resolved in-cluster ClusterIP Service
// exposed for a SeiNetwork. Consumers resolve the service at
// {name}.{namespace}.svc and dial the named port from {ports}.
type InternalServiceStatus struct {
	// Name is the Kubernetes Service name (always "{network-name}-internal").
	Name string `json:"name"`

	// Namespace is the Service's namespace (always equal to the network's
	// namespace).
	Namespace string `json:"namespace"`

	// Ports enumerates the named ports on the Service.
	Ports InternalServicePorts `json:"ports"`
}

// InternalServicePorts is the set of named ports advertised on the internal
// ClusterIP Service. Only stateless HTTP request/response protocols are
// exposed here — stateful protocols (EVM WebSocket, gRPC streaming, P2P
// gossip) do not load-balance correctly behind a kube-proxy L4 LB, and
// consumers needing those use the per-node headless Services instead.
// Field names are part of the public interface contract.
type InternalServicePorts struct {
	// Rpc is the Tendermint / CometBFT RPC port (26657).
	Rpc int32 `json:"rpc"`
	// EvmHttp is the EVM JSON-RPC HTTP port (8545).
	EvmHttp int32 `json:"evmHttp"`
	// Rest is the Cosmos REST (LCD) port (1317).
	Rest int32 `json:"rest"`
}

// PerPodServiceStatus describes one child's headless Service.
// Name equals the child SeiNode name and the headless Service name.
type PerPodServiceStatus struct {
	Name      string             `json:"name"`
	Namespace string             `json:"namespace"`
	Ports     PerPodServicePorts `json:"ports"`
}

// PerPodServicePorts adds the stateful ports the cluster-internal Service
// omits. Field names are part of the public interface.
type PerPodServicePorts struct {
	EvmHttp int32 `json:"evmHttp"`
	EvmWs   int32 `json:"evmWs"`
}

// GroupNodeStatus is a summary of a child SeiNode's state.
type GroupNodeStatus struct {
	// Name is the SeiNode resource name.
	Name string `json:"name"`

	// Phase is the SeiNode's current phase.
	Phase SeiNodePhase `json:"phase,omitempty"`

	// CurrentImage is the seid image the child reports running
	// (mirrored from the child's status.currentImage). Compared against
	// spec.image to derive UpToDateReplicas and the RolloutInProgress
	// condition.
	// +optional
	CurrentImage string `json:"currentImage,omitempty"`

	// WorkerNode is the Kubernetes node (one EC2 instance on this platform)
	// running the child's pod, read from the pod each reconcile so it follows
	// a reschedule. Empty while Placement is Pending. Two validators naming
	// the same worker node share that instance's network bandwidth.
	// +optional
	WorkerNode string `json:"workerNode,omitempty"`

	// Placement reports whether the child's pod is bound to a worker node.
	// Pending covers both "no pod yet" and "pod exists but is unschedulable",
	// e.g. a Dedicated validator with no free single-tenant worker node.
	// +optional
	Placement Placement `json:"placement,omitempty"`
}

// Placement is the scheduling state of a child SeiNode's pod.
// +kubebuilder:validation:Enum=Pending;Scheduled
type Placement string

const (
	// PlacementPending means no pod of the child is bound to a worker node.
	PlacementPending Placement = "Pending"
	// PlacementScheduled means the child's pod is bound to WorkerNode.
	PlacementScheduled Placement = "Scheduled"
)

// Status condition types for SeiNetwork.
const (
	ConditionNodesReady              = "NodesReady"
	ConditionGenesisCeremonyComplete = "GenesisCeremonyComplete"
	ConditionPlanInProgress          = "PlanInProgress"
	// ConditionRolloutInProgress is a DERIVED projection (not a state machine):
	// True when UpToDateReplicas < Replicas (a child is mid-roll or wedged on a
	// bad image), False/AllUpToDate otherwise. Computed in updateStatus from the
	// child snapshot — no plan or revision tracking owns it.
	ConditionRolloutInProgress = "RolloutInProgress"
	ConditionPaused            = "Paused"
	// ConditionConfigValuesValid reports whether spec.configValues builds a
	// TOML overlay. The CRD schema checks shape, not TOML representability, so
	// the controller runs the child's own overlay builder once here rather than
	// stamping a set that fails identically on every validator.
	ConditionConfigValuesValid = "ConfigValuesValid"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sn
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.paused`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SeiNetwork is the Schema for the seinetworks API.
type SeiNetwork struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SeiNetworkSpec   `json:"spec,omitempty"`
	Status SeiNetworkStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SeiNetworkList contains a list of SeiNetwork.
type SeiNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SeiNetwork `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SeiNetwork{}, &SeiNetworkList{})
}
