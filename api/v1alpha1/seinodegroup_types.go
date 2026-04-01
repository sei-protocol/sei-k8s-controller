package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SeiNodeGroupSpec defines the desired state of a SeiNodeGroup.
// +kubebuilder:validation:XValidation:rule="!(has(self.monitoring) && has(self.monitoring.serviceMonitor) && has(self.networking) && has(self.networking.service) && has(self.networking.service.ports) && size(self.networking.service.ports) > 0) || self.networking.service.ports.exists(p, p == 'metrics')",message="networking.service.ports must include 'metrics' when monitoring.serviceMonitor is configured"
type SeiNodeGroupSpec struct {
	// Replicas is the number of SeiNode instances to create.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas"`

	// Template defines the SeiNode spec stamped out for each replica.
	// Each SeiNode is named "{group-name}-{ordinal}".
	Template SeiNodeTemplate `json:"template"`

	// DeletionPolicy controls what happens to child SeiNodes and managed
	// networking/monitoring resources when the SeiNodeGroup is deleted.
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

	// Networking controls how the group is exposed to traffic.
	// Networking resources are shared across all replicas.
	// +optional
	Networking *NetworkingConfig `json:"networking,omitempty"`

	// Monitoring configures observability resources shared across
	// all replicas.
	// +optional
	Monitoring *MonitoringConfig `json:"monitoring,omitempty"`

	// UpdateStrategy controls how changes to the template are rolled out
	// to child SeiNodes. When set, the controller uses blue-green
	// deployment orchestration instead of in-place updates.
	// When not set, template changes are applied in-place via ensureSeiNode.
	// +optional
	UpdateStrategy *UpdateStrategy `json:"updateStrategy,omitempty"`
}

// UpdateStrategyType identifies the deployment strategy.
// +kubebuilder:validation:Enum=BlueGreen;HardFork
type UpdateStrategyType string

const (
	// UpdateStrategyBlueGreen performs a blue-green deployment once the
	// green nodes have caught up to the chain tip (catching_up == false).
	UpdateStrategyBlueGreen UpdateStrategyType = "BlueGreen"

	// UpdateStrategyHardFork performs a blue-green deployment at a specific
	// block height. The old binary halts via sidecar SIGTERM at the
	// configured halt-height, and the new binary continues from that height.
	UpdateStrategyHardFork UpdateStrategyType = "HardFork"
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

// ForkConfig identifies the source chain to fork from. The exported
// genesis state is expected at {sourceChainId}/exported-state.json in
// the platform genesis bucket. The assembled fork genesis is written
// to {newChainId}/{groupName}/genesis.json — same convention as a
// standard genesis ceremony.
type ForkConfig struct {
	// SourceChainID is the chain ID of the network being forked.
	// +kubebuilder:validation:MinLength=1
	SourceChainID string `json:"sourceChainId"`
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
	// The controller always adds sei.io/nodegroup and sei.io/nodegroup-ordinal
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

// SeiNodeGroupPhase represents the high-level lifecycle state.
// +kubebuilder:validation:Enum=Pending;Initializing;Ready;Upgrading;Degraded;Failed;Terminating
type SeiNodeGroupPhase string

const (
	GroupPhasePending      SeiNodeGroupPhase = "Pending"
	GroupPhaseInitializing SeiNodeGroupPhase = "Initializing"
	GroupPhaseReady        SeiNodeGroupPhase = "Ready"
	GroupPhaseUpgrading    SeiNodeGroupPhase = "Upgrading"
	GroupPhaseDegraded     SeiNodeGroupPhase = "Degraded"
	GroupPhaseFailed       SeiNodeGroupPhase = "Failed"
	GroupPhaseTerminating  SeiNodeGroupPhase = "Terminating"
)

// SeiNodeGroupStatus defines the observed state of a SeiNodeGroup.
type SeiNodeGroupStatus struct {
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
	Phase SeiNodeGroupPhase `json:"phase,omitempty"`

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

	// Deployment tracks an in-progress deployment.
	// Nil when no deployment is active.
	// +optional
	Deployment *DeploymentStatus `json:"deployment,omitempty"`

	// NetworkingStatus reports the observed state of networking resources.
	// +optional
	NetworkingStatus *NetworkingStatus `json:"networkingStatus,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
	// ExternalServiceName is the name of the managed external Service.
	// +optional
	ExternalServiceName string `json:"externalServiceName,omitempty"`

	// LoadBalancerIngress contains the hostname/IP assigned by the cloud
	// provider once the LoadBalancer is provisioned.
	// +optional
	LoadBalancerIngress []corev1.LoadBalancerIngress `json:"loadBalancerIngress,omitempty"`
}

// DeploymentStatus tracks metadata for an in-progress deployment.
// The task plan itself lives in SeiNodeGroupStatus.Plan.
type DeploymentStatus struct {
	// IncumbentRevision identifies the generation of the currently live nodes.
	IncumbentRevision string `json:"incumbentRevision"`

	// EntrantRevision identifies the generation of the new nodes being deployed.
	EntrantRevision string `json:"entrantRevision"`

	// EntrantNodes lists the names of the new SeiNode resources.
	// +optional
	EntrantNodes []string `json:"entrantNodes,omitempty"`
}

// Status condition types for SeiNodeGroup.
const (
	ConditionNodesReady                = "NodesReady"
	ConditionExternalServiceReady      = "ExternalServiceReady"
	ConditionRouteReady                = "RouteReady"
	ConditionIsolationReady            = "IsolationReady"
	ConditionServiceMonitorReady       = "ServiceMonitorReady"
	ConditionGenesisCeremonyComplete   = "GenesisCeremonyComplete"
	ConditionPlanInProgress            = "PlanInProgress"
	ConditionGenesisCeremonyNeeded     = "GenesisCeremonyNeeded"
	ConditionForkGenesisCeremonyNeeded = "ForkGenesisCeremonyNeeded"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sng
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.deployment.entrantRevision`,priority=1
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.networking.gateway.hostnames[0]`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SeiNodeGroup is the Schema for the seinodegroups API.
type SeiNodeGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SeiNodeGroupSpec   `json:"spec,omitempty"`
	Status SeiNodeGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SeiNodeGroupList contains a list of SeiNodeGroup.
type SeiNodeGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SeiNodeGroup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SeiNodeGroup{}, &SeiNodeGroupList{})
}
