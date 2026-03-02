package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SeiNodePoolSpec defines the desired state of a genesis Sei node pool.
type SeiNodePoolSpec struct {

	// ChainID of the genesis network.
	// +kubebuilder:validation:MinLength=1
	ChainID string `json:"chainId"`

	NodeConfiguration NodeConfiguration `json:"nodeConfiguration"`

	// +optional
	Storage StorageConfig `json:"storage,omitempty"`
}

// NodeConfiguration defines the node class for the genesis network.
// All nodes share the same image and entrypoint; NodeCount controls how many are deployed.
type NodeConfiguration struct {
	// NodeCount is the number of nodes in the genesis network.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=64
	NodeCount int32 `json:"nodeCount,omitempty"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Image string `json:"image"`

	Entrypoint EntrypointConfig `json:"entrypoint"`
}

// EntrypointConfig defines the command and arguments for the node process.
type EntrypointConfig struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	Command []string `json:"command"`

	// +optional
	// +kubebuilder:validation:MaxItems=64
	Args []string `json:"args,omitempty"`
}

// StorageConfig defines storage parameters for node data volumes.
type StorageConfig struct {
	// RetainOnDelete prevents data PVCs from being deleted when the SeiNodePool is deleted.
	// +optional
	// +kubebuilder:default=false
	RetainOnDelete bool `json:"retainOnDelete,omitempty"`
}

// SeiNodePoolStatus defines the observed state of a SeiNodePool deployment.
type SeiNodePoolStatus struct {
	// +kubebuilder:validation:Enum=Pending;Running;Failed;Terminating
	Phase string `json:"phase,omitempty"`

	ReadyNodes int32 `json:"readyNodes,omitempty"`

	TotalNodes int32 `json:"totalNodes,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	NodeStatuses []NodeStatus `json:"nodeStatuses,omitempty"`
}

// NodeStatus reports the observed state of a single node.
type NodeStatus struct {
	Name          string `json:"name"`
	Ready         bool   `json:"ready"`
	BlockHeight   int64  `json:"blockHeight,omitempty"`
	StateRootHash string `json:"stateRootHash,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=snp
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyNodes`
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.totalNodes`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SeiNodePool is the Schema for the seinodepools API.
type SeiNodePool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SeiNodePoolSpec   `json:"spec,omitempty"`
	Status SeiNodePoolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SeiNodePoolList contains a list of SeiNodePool.
type SeiNodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SeiNodePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SeiNodePool{}, &SeiNodePoolList{})
}
