package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SeiNodeDeploymentSpec defines the desired state of a SeiNodeDeployment.
//
// +kubebuilder:validation:XValidation:rule="!has(self.genesis) || has(self.template.spec.validator)",message="genesis is meaningful only for validator-role deployments (full nodes inherit genesis from the validator ceremony's S3 artifact); remove spec.genesis or set template.spec.validator: {}"
type SeiNodeDeploymentSpec struct {
	// Replicas is the number of SeiNode instances to create.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas"`

	// Template defines the SeiNode spec stamped out for each replica.
	// Each SeiNode is named "{group-name}-{ordinal}".
	Template SeiNodeTemplate `json:"template"`

	// DeletionPolicy controls what happens to child SeiNodes and managed
	// networking resources when the SeiNodeDeployment is deleted.
	// "Delete" (default) cascades deletion. "Retain" orphans children
	// and networking resources so they continue running independently.
	// +optional
	// +kubebuilder:default=Delete
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// Genesis configures genesis ceremony orchestration for this group.
	// When set, the controller generates GenesisCeremonyNodeConfig for each
	// child SeiNode and coordinates assembly of the final genesis.json.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.genesis is immutable after creation"
	Genesis *GenesisCeremonyConfig `json:"genesis,omitempty"`

	// Networking enables public networking for the deployment.
	// When present, the controller creates a ClusterIP Service and
	// HTTPRoutes on the platform Gateway. When absent, the deployment
	// is private with only per-node headless Services.
	// +optional
	Networking *NetworkingConfig `json:"networking,omitempty"`

	// UpdateStrategy controls how changes to the template are rolled out
	// to child SeiNodes. Every deployment must declare an explicit strategy.
	UpdateStrategy UpdateStrategy `json:"updateStrategy"`
}

// UpdateStrategyType identifies the deployment strategy.
// +kubebuilder:validation:Enum=InPlace
type UpdateStrategyType string

const (
	UpdateStrategyInPlace UpdateStrategyType = "InPlace"
)

// UpdateStrategy controls how spec changes propagate to child SeiNodes.
type UpdateStrategy struct {
	// Type selects the deployment strategy.
	Type UpdateStrategyType `json:"type"`
}

// GenesisCeremonyConfig configures genesis ceremony orchestration for a node group.
type GenesisCeremonyConfig struct {
	// ChainID for the new network.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9-]*[a-z0-9]$`
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

	// MaxCeremonyDuration is the maximum time from group creation to genesis
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

// SeiNodeTemplate wraps a SeiNodeSpec for use in the group template.
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

// SeiNodeDeploymentPhase represents the high-level lifecycle state.
// +kubebuilder:validation:Enum=Pending;Initializing;Ready;Upgrading;Degraded;Failed;Terminating
type SeiNodeDeploymentPhase string

const (
	GroupPhasePending      SeiNodeDeploymentPhase = "Pending"
	GroupPhaseInitializing SeiNodeDeploymentPhase = "Initializing"
	GroupPhaseReady        SeiNodeDeploymentPhase = "Ready"
	GroupPhaseUpgrading    SeiNodeDeploymentPhase = "Upgrading"
	GroupPhaseDegraded     SeiNodeDeploymentPhase = "Degraded"
	GroupPhaseFailed       SeiNodeDeploymentPhase = "Failed"
	GroupPhaseTerminating  SeiNodeDeploymentPhase = "Terminating"
)

// SeiNodeDeploymentStatus defines the observed state of a SeiNodeDeployment.
type SeiNodeDeploymentStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// TemplateHash is a hash of the spec fields that require deployment
	// orchestration when changed (image, entrypoint, chainId). Updated
	// during steady-state reconciliation. Compared against the current
	// spec to detect deployment-worthy changes.
	// +optional
	TemplateHash string `json:"templateHash,omitempty"`

	// Phase is the high-level lifecycle state.
	Phase SeiNodeDeploymentPhase `json:"phase,omitempty"`

	// Replicas is the desired number of SeiNodes.
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of SeiNodes in Running phase.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Nodes reports the status of each child SeiNode.
	// +listType=map
	// +listMapKey=name
	// +optional
	Nodes []GroupNodeStatus `json:"nodes,omitempty"`

	// Plan tracks the active group-level task plan (genesis assembly,
	// deployment, etc.). Nil when no plan is in progress.
	// +optional
	Plan *TaskPlan `json:"plan,omitempty"`

	// GenesisHash is the SHA-256 hex digest of the assembled genesis.json.
	// +optional
	GenesisHash string `json:"genesisHash,omitempty"`

	// GenesisS3URI is the S3 URI of the uploaded genesis.
	// +optional
	GenesisS3URI string `json:"genesisS3URI,omitempty"`

	// IncumbentNodes lists the names of the currently active SeiNode resources.
	// Set during steady-state reconciliation so that the deployment planner
	// can read the current node set directly from the group object.
	// +optional
	IncumbentNodes []string `json:"incumbentNodes,omitempty"`

	// Rollout tracks an in-progress rollout across all strategy types.
	// Nil when no rollout is active.
	// +optional
	Rollout *RolloutStatus `json:"rollout,omitempty"`

	// NetworkingStatus reports the observed state of networking resources.
	// +optional
	NetworkingStatus *NetworkingStatus `json:"networkingStatus,omitempty"`

	// InternalService reports the in-cluster ClusterIP Service that kube-proxy
	// load-balances across healthy child pods. Populated unconditionally —
	// this path is independent of .spec.networking.
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

// Endpoints lists composed in-cluster URLs for consuming this deployment.
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
// exposed for a SeiNodeDeployment. Consumers resolve the service at
// {name}.{namespace}.svc and dial the named port from {ports}.
type InternalServiceStatus struct {
	// Name is the Kubernetes Service name (always "{deployment-name}-internal").
	Name string `json:"name"`

	// Namespace is the Service's namespace (always equal to the deployment's
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

// NetworkingStatus reports the observed state of networking resources.
type NetworkingStatus struct {
	// Routes lists the HTTPRoute hostnames managed by this deployment.
	// +optional
	Routes []RouteStatus `json:"routes,omitempty"`
}

// RouteStatus is the observed state of a single HTTPRoute hostname.
type RouteStatus struct {
	// Hostname is the public-facing DNS name for this route.
	Hostname string `json:"hostname"`

	// Protocol identifies the route type (e.g. "rpc", "evm", "grpc", "rest", "evm-ws").
	// +optional
	Protocol string `json:"protocol,omitempty"`
}

// RolloutStatus tracks an in-progress rollout. Presence on the parent
// SeiNodeDeployment is the single source of truth for "rollout in
// progress" — `Status.Rollout != nil` and the `RolloutInProgress`
// condition move together.
type RolloutStatus struct {
	// TargetHash is the templateHash being rolled out to.
	TargetHash string `json:"targetHash"`

	// StartedAt is when the rollout was first detected.
	StartedAt metav1.Time `json:"startedAt"`
}

// Status condition types for SeiNodeDeployment.
const (
	ConditionNodesReady              = "NodesReady"
	ConditionRouteReady              = "RouteReady"
	ConditionGenesisCeremonyComplete = "GenesisCeremonyComplete"
	ConditionPlanInProgress          = "PlanInProgress"
	ConditionGenesisCeremonyNeeded   = "GenesisCeremonyNeeded"
	ConditionRolloutInProgress       = "RolloutInProgress"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=snd
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.status.rollout.targetHash`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SeiNodeDeployment is the Schema for the seinodedeployments API.
type SeiNodeDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SeiNodeDeploymentSpec   `json:"spec,omitempty"`
	Status SeiNodeDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SeiNodeDeploymentList contains a list of SeiNodeDeployment.
type SeiNodeDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SeiNodeDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SeiNodeDeployment{}, &SeiNodeDeploymentList{})
}
