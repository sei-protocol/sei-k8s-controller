package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SeiNetworkSpec defines the desired state of a SeiNetwork.
//
// A SeiNetwork bootstraps a new Sei chain: a chainId plus a required genesis
// ceremony that mints the chain's genesis.json/genesisHash and its founding
// validator set. spec.genesis is mandatory — the Kind cannot exist without it.
// The replicas are the genesis validators; the template is the validator role.
//
// +kubebuilder:validation:XValidation:rule="self.genesis == oldSelf.genesis",message="spec.genesis is immutable once set; the ceremony's outputs (chain ID, validator gentxs, account balances) are baked into chain state and cannot be retroactively rewritten by editing the spec"
// +kubebuilder:validation:XValidation:rule="has(self.template.spec.validator)",message="a SeiNetwork bootstraps a genesis validator set; template.spec.validator must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.template.spec.validator.signingKey) || self.replicas == 1",message="a validator with a signingKey must have replicas: 1 — every replica mounts the same priv_validator_key.json, so >1 replica double-signs (equivocation) and tombstones/slashes the validator"
type SeiNetworkSpec struct {
	// Image is the seid image the genesis validators run.
	Image string `json:"image"`

	// Genesis is REQUIRED. The ceremony that bootstraps this chain.
	// Immutable once set; enforced by spec-level CEL. A SeiNetwork cannot
	// exist without it.
	// +required
	Genesis GenesisCeremonyConfig `json:"genesis"`

	// Replicas is the number of genesis validators to create.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas"`

	// Template defines the validator-role SeiNode spec stamped out for each
	// genesis validator. Each SeiNode is named "{network-name}-{ordinal}".
	Template SeiNodeTemplate `json:"template"`

	// DeletionPolicy controls what happens to child SeiNodes when the
	// SeiNetwork is deleted. "Delete" (default) cascades deletion. "Retain"
	// orphans children so they continue running independently — required for
	// validator pools, whose PVCs hold node_key.json / priv_validator_key.json.
	// +optional
	// +kubebuilder:default=Delete
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// Paused freezes plan-driven orchestration. While true, no new plans
	// start, no rollouts trigger, no template changes propagate to
	// children, and any active plan freezes in place. Children inherit the
	// paused state.
	// +optional
	Paused bool `json:"paused,omitempty"`
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

// SeiNodeTemplate wraps a SeiNodeSpec for use in the network template.
type SeiNodeTemplate struct {
	// Metadata allows setting labels and annotations on child SeiNodes.
	// The controller always adds sei.io/nodedeployment and sei.io/nodedeployment-ordinal
	// labels; user-specified labels are merged.
	// +optional
	Metadata *SeiNodeTemplateMeta `json:"metadata,omitempty"`

	// Spec is the SeiNodeSpec applied to each replica.
	Spec SeiNodeSpec `json:"spec"`
}

// SeiNodeTemplateMeta defines metadata for templated SeiNodes.
type SeiNodeTemplateMeta struct {
	// Labels are merged onto each child SeiNode's metadata.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations are merged onto each child SeiNode's metadata.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

// SeiNetworkPhase represents the high-level lifecycle state.
// +kubebuilder:validation:Enum=Pending;Initializing;Ready;Upgrading;Paused;Degraded;Failed;Terminating
type SeiNetworkPhase string

const (
	GroupPhasePending      SeiNetworkPhase = "Pending"
	GroupPhaseInitializing SeiNetworkPhase = "Initializing"
	GroupPhaseReady        SeiNetworkPhase = "Ready"
	GroupPhaseUpgrading    SeiNetworkPhase = "Upgrading"
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

	// TemplateHash is a hash of the spec fields that require deployment
	// orchestration when changed (image, chainId). Updated during steady-state
	// reconciliation. Compared against the current spec to detect
	// deployment-worthy changes.
	// +optional
	TemplateHash string `json:"templateHash,omitempty"`

	// Phase is the high-level lifecycle state.
	Phase SeiNetworkPhase `json:"phase,omitempty"`

	// Replicas is the desired number of SeiNodes.
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of SeiNodes in Running phase.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

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

	// IncumbentNodes lists the names of the currently active SeiNode resources.
	// Set during steady-state reconciliation so that the deployment planner
	// can read the current node set directly from the network object.
	// +optional
	IncumbentNodes []string `json:"incumbentNodes,omitempty"`

	// Rollout tracks an in-progress rollout across all strategy types.
	// Nil when no rollout is active.
	// +optional
	Rollout *RolloutStatus `json:"rollout,omitempty"`

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
}

// RolloutStatus tracks an in-progress rollout. Presence on the parent
// SeiNetwork is the single source of truth for "rollout in progress" —
// `Status.Rollout != nil` and the `RolloutInProgress` condition move together.
type RolloutStatus struct {
	// TargetHash is the templateHash being rolled out to.
	TargetHash string `json:"targetHash"`

	// StartedAt is when the rollout was first detected.
	StartedAt metav1.Time `json:"startedAt"`
}

// Status condition types for SeiNetwork.
const (
	ConditionNodesReady              = "NodesReady"
	ConditionGenesisCeremonyComplete = "GenesisCeremonyComplete"
	ConditionPlanInProgress          = "PlanInProgress"
	ConditionRolloutInProgress       = "RolloutInProgress"
	ConditionPaused                  = "Paused"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sn
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Paused",type=boolean,JSONPath=`.spec.paused`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.status.rollout.targetHash`,priority=1
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
