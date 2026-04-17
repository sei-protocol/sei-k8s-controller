package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SeiNodeDeploymentSpec defines the desired state of a SeiNodeDeployment.
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
	// networking/monitoring resources when the SeiNodeDeployment is deleted.
	// "Delete" (default) cascades deletion. "Retain" orphans children
	// and networking resources so they continue running independently.
	// +optional
	// +kubebuilder:default=Delete
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// Genesis configures genesis ceremony orchestration for this group.
	// When set, the controller generates GenesisCeremonyNodeConfig for each
	// child SeiNode and coordinates assembly of the final genesis.json.
	// +optional
	Genesis *GenesisCeremonyConfig `json:"genesis,omitempty"`

	// Networking enables public networking for the deployment.
	// When present, the controller creates a ClusterIP Service and
	// HTTPRoutes on the platform Gateway. When absent, the deployment
	// is private with only per-node headless Services.
	// +optional
	Networking *NetworkingConfig `json:"networking,omitempty"`

	// Monitoring configures observability resources shared across
	// all replicas.
	// +optional
	Monitoring *MonitoringConfig `json:"monitoring,omitempty"`

	// UpdateStrategy controls how changes to the template are rolled out
	// to child SeiNodes. Every deployment must declare an explicit strategy.
	UpdateStrategy UpdateStrategy `json:"updateStrategy"`
}

// UpdateStrategyType identifies the deployment strategy.
// +kubebuilder:validation:Enum=InPlace;BlueGreen;HardFork
type UpdateStrategyType string

const (
	UpdateStrategyInPlace   UpdateStrategyType = "InPlace"
	UpdateStrategyBlueGreen UpdateStrategyType = "BlueGreen"
	UpdateStrategyHardFork  UpdateStrategyType = "HardFork"
)

// UpdateStrategy controls how spec changes propagate to child SeiNodes.
// +kubebuilder:validation:XValidation:rule="self.type != 'HardFork' || (has(self.hardFork) && self.hardFork.haltHeight > 0)",message="hardFork strategy requires haltHeight > 0"
type UpdateStrategy struct {
	// Type selects the deployment strategy.
	Type UpdateStrategyType `json:"type"`

	// HardFork configures blue-green deployment at a specific block height.
	// Required when type is HardFork.
	// +optional
	HardFork *HardForkStrategy `json:"hardFork,omitempty"`
}

// HardForkStrategy configures a blue-green deployment at a specific block height.
type HardForkStrategy struct {
	// HaltHeight is the block height at which the old binary is stopped
	// via sidecar SIGTERM. The new binary must include an upgrade handler
	// that continues from this height.
	// +kubebuilder:validation:Minimum=1
	HaltHeight int64 `json:"haltHeight"`
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

	// Overrides is a flat map of dotted key paths merged on top of sei-config's
	// GenesisDefaults(). Applied BEFORE gentx generation.
	// +optional
	Overrides map[string]string `json:"overrides,omitempty"`

	// MaxCeremonyDuration is the maximum time from group creation to genesis
	// assembly completion. Default: "15m".
	// +optional
	MaxCeremonyDuration *metav1.Duration `json:"maxCeremonyDuration,omitempty"`

	// Fork configures this genesis ceremony to fork from an existing
	// chain's exported state rather than building genesis from scratch.
	// When set, the assembler downloads the exported state, rewrites
	// the chain identity, strips old validators, and runs collect-gentxs
	// with the new validator set.
	// +optional
	Fork *ForkConfig `json:"fork,omitempty"`
}

// ForkConfig configures forking from an existing chain. The controller
// creates a temporary exporter SeiNode that bootstraps from the source
// chain (using the same pipeline as replayers), then the group plan
// submits seid export to the exporter's sidecar and uploads the result.
type ForkConfig struct {
	// SourceChainID is the chain ID of the network being forked.
	// +kubebuilder:validation:MinLength=1
	SourceChainID string `json:"sourceChainId"`

	// SourceImage is the seid container image compatible with the source
	// chain at ExportHeight. Used as both the bootstrap and main image
	// for the temporary exporter node.
	// +kubebuilder:validation:MinLength=1
	SourceImage string `json:"sourceImage"`

	// ExportHeight is the block height at which to export state.
	// seid export --height N reads committed state at exactly this height.
	// +kubebuilder:validation:Minimum=1
	ExportHeight int64 `json:"exportHeight"`
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

	// RpcService reports the in-cluster ClusterIP Service that kube-proxy
	// load-balances across healthy child pods. Populated unconditionally —
	// this path is independent of .spec.networking.
	// +optional
	RpcService *RpcServiceStatus `json:"rpcService,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RpcServiceStatus reports the resolved in-cluster ClusterIP Service that
// exposes RPC for a SeiNodeDeployment. Consumers resolve the service at
// {name}.{namespace}.svc and dial the named port from {ports}.
type RpcServiceStatus struct {
	// Name is the Kubernetes Service name (always "{deployment-name}-rpc").
	Name string `json:"name"`

	// Namespace is the Service's namespace (always equal to the deployment's
	// namespace).
	Namespace string `json:"namespace"`

	// Ports enumerates the named ports on the Service. All fields are populated
	// with the canonical seid port numbers; absence from the node's active port
	// set (e.g. validator mode) is reported via the Service's Ports list, not
	// by omitting fields here.
	Ports RpcServicePorts `json:"ports"`
}

// RpcServicePorts is the set of named ports advertised on the internal
// RPC Service. Field names are part of the public interface contract.
type RpcServicePorts struct {
	// Rpc is the Tendermint / CometBFT RPC port (26657).
	Rpc int32 `json:"rpc"`
	// EvmHttp is the EVM JSON-RPC HTTP port (8545).
	EvmHttp int32 `json:"evmHttp"`
	// EvmWs is the EVM JSON-RPC WebSocket port (8546).
	EvmWs int32 `json:"evmWs"`
	// Rest is the Cosmos REST (LCD) port (1317).
	Rest int32 `json:"rest"`
	// Grpc is the Cosmos gRPC port (9090).
	Grpc int32 `json:"grpc"`
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

// RolloutStatus tracks an in-progress rollout. Used by all strategies
// to report per-node convergence state.
type RolloutStatus struct {
	// Strategy is the strategy type driving this rollout.
	Strategy UpdateStrategyType `json:"strategy"`

	// TargetHash is the templateHash being rolled out to.
	TargetHash string `json:"targetHash"`

	// StartedAt is when the rollout was first detected.
	StartedAt metav1.Time `json:"startedAt"`

	// IncumbentNodes lists the names of the currently active SeiNode
	// resources. Only populated for BlueGreen and HardFork strategies.
	// +optional
	IncumbentNodes []string `json:"incumbentNodes,omitempty"`

	// EntrantNodes lists the names of the new SeiNode resources being
	// created. Only populated for BlueGreen and HardFork strategies.
	// +optional
	EntrantNodes []string `json:"entrantNodes,omitempty"`

	// IncumbentRevision identifies the generation of the currently live nodes.
	// Only populated for BlueGreen and HardFork strategies.
	// +optional
	IncumbentRevision string `json:"incumbentRevision,omitempty"`

	// EntrantRevision identifies the generation of the new nodes.
	// Only populated for BlueGreen and HardFork strategies.
	// +optional
	EntrantRevision string `json:"entrantRevision,omitempty"`
}

// Status condition types for SeiNodeDeployment.
const (
	ConditionNodesReady                = "NodesReady"
	ConditionRouteReady                = "RouteReady"
	ConditionServiceMonitorReady       = "ServiceMonitorReady"
	ConditionGenesisCeremonyComplete   = "GenesisCeremonyComplete"
	ConditionPlanInProgress            = "PlanInProgress"
	ConditionGenesisCeremonyNeeded     = "GenesisCeremonyNeeded"
	ConditionForkGenesisCeremonyNeeded = "ForkGenesisCeremonyNeeded"
	ConditionRolloutInProgress         = "RolloutInProgress"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=snd
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.rollout.entrantRevision`,priority=1
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
