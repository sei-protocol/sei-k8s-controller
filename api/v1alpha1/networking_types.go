package v1alpha1

// DeletionPolicy controls what happens to managed networking resources
// and child SeiNodes when their parent is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
	DeletionPolicyDelete DeletionPolicy = "Delete"
	DeletionPolicyRetain DeletionPolicy = "Retain"
)

// NetworkingConfig opts a SeiNodeDeployment into public networking.
// HTTP presence enables L7 Gateway exposure; TCP presence enables a
// per-pod L4 NLB for P2P/26656. Either is settable independently.
//
// Backcompat: a legacy `networking: {}` (no sub-struct set) means HTTP
// enabled — evaluate via HTTPEnabled() rather than nil-checking HTTP.
type NetworkingConfig struct {
	// HTTP enables L7 application exposure via the platform Gateway.
	// +optional
	HTTP *HTTPConfig `json:"http,omitempty"`

	// TCP enables a per-pod L4 NLB for raw P2P/26656 publishability.
	// +optional
	TCP *TCPConfig `json:"tcp,omitempty"`
}

// HTTPConfig is a presence-signal placeholder for L7 exposure.
type HTTPConfig struct{}

// TCPConfig is a presence-signal placeholder for L4 NLB exposure.
type TCPConfig struct{}

// HTTPEnabled reports whether L7 exposure is requested. Treats legacy
// `networking: {}` (no sub-struct set) as enabled for backcompat.
func (n *NetworkingConfig) HTTPEnabled() bool {
	if n == nil {
		return false
	}
	if n.HTTP != nil {
		return true
	}
	return n.TCP == nil
}

// TCPEnabled reports whether L4 NLB per-pod P2P exposure is requested.
// Mirrors HTTPEnabled but does not carry the legacy `networking: {}`
// back-compat — TCP must be explicitly opted into.
func (n *NetworkingConfig) TCPEnabled() bool {
	return n != nil && n.TCP != nil
}
