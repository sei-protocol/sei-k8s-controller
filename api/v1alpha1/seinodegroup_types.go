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
	// +kubebuilder:validation:Maximum=16
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

	// Networking controls how the group is exposed to traffic.
	// Networking resources are shared across all replicas.
	// +optional
	Networking *NetworkingConfig `json:"networking,omitempty"`

	// Monitoring configures observability resources shared across
	// all replicas.
	// +optional
	Monitoring *MonitoringConfig `json:"monitoring,omitempty"`
}

// SeiNodeTemplate wraps a SeiNodeSpec for use in the group template.
type SeiNodeTemplate struct {
	// Metadata allows setting labels and annotations on child SeiNodes.
	// The controller always adds sei.io/group and sei.io/group-ordinal
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
// +kubebuilder:validation:Enum=Pending;Initializing;Ready;Degraded;Failed;Terminating
type SeiNodeGroupPhase string

const (
	GroupPhasePending      SeiNodeGroupPhase = "Pending"
	GroupPhaseInitializing SeiNodeGroupPhase = "Initializing"
	GroupPhaseReady        SeiNodeGroupPhase = "Ready"
	GroupPhaseDegraded     SeiNodeGroupPhase = "Degraded"
	GroupPhaseFailed       SeiNodeGroupPhase = "Failed"
	GroupPhaseTerminating  SeiNodeGroupPhase = "Terminating"
)

// SeiNodeGroupStatus defines the observed state of a SeiNodeGroup.
type SeiNodeGroupStatus struct {
	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

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

// Status condition types for SeiNodeGroup.
const (
	ConditionNodesReady           = "NodesReady"
	ConditionExternalServiceReady = "ExternalServiceReady"
	ConditionRouteReady           = "RouteReady"
	ConditionIsolationReady       = "IsolationReady"
	ConditionServiceMonitorReady  = "ServiceMonitorReady"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sng
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
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
