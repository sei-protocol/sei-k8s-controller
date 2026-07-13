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
// +kubebuilder:validation:XValidation:rule="(!has(self.dataVolume) && !has(oldSelf.dataVolume)) || self.dataVolume == oldSelf.dataVolume",message="spec.dataVolume is immutable once set; it backs a StatefulSet volumeClaimTemplate, which Kubernetes forbids changing after create"
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

	// DataVolume configures the data PersistentVolumeClaim for each genesis
	// validator. The ceremony-generated consensus identity lives here, so
	// DeletionPolicy defaults to Retain.
	//
	// Immutable after create (spec-level CEL): it backs a StatefulSet
	// volumeClaimTemplate, which Kubernetes forbids changing post-create, so a
	// later edit could never take effect — admission rejects it rather than
	// letting the controller silently ignore it.
	// +optional
	DataVolume *DataVolumeSpec `json:"dataVolume,omitempty"`

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
	// SeiNetwork is deleted. "Retain" (default) orphans children so they
	// continue running independently — a validator pool's PVCs hold
	// ceremony-generated, unrecoverable consensus identity. "Delete" cascades
	// deletion.
	// +optional
	// +kubebuilder:default=Retain
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
}

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
