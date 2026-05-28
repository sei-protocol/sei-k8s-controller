package v1alpha1

// DeletionPolicy controls what happens to managed networking resources
// and child SeiNodes when their parent is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
	DeletionPolicyDelete DeletionPolicy = "Delete"
	DeletionPolicyRetain DeletionPolicy = "Retain"
)

// NetworkingConfig opts a SeiNodeDeployment (or SeiNode child) into
// public networking, with one optional sub-struct per OSI layer.
//
// HTTP: presence enables L7 application exposure (RPC/EVM/REST/gRPC)
// via the platform Gateway HTTPRoutes.
//
// TCP: presence enables a per-pod L4 NLB exposing P2P/26656, with the
// NLB hostname published into SeiNode.Status.ExternalAddress.
//
// Either may be set independently. Both nil means no public networking.
//
// Backward compatibility: a legacy `networking: {}` manifest (neither
// sub-struct set) is treated as `networking: { http: {} }` so existing
// SNDs that relied on the empty-struct-means-HTTP behavior keep working.
// Evaluate this via HTTPEnabled() rather than checking HTTP directly.
type NetworkingConfig struct {
	// HTTP enables L7 application exposure via the platform Gateway.
	// Presence-signal: an empty struct is sufficient to enable.
	// +optional
	HTTP *HTTPConfig `json:"http,omitempty"`

	// TCP enables a per-pod L4 NLB for raw P2P/26656 publishability.
	// Presence-signal: an empty struct is sufficient to enable.
	// +optional
	TCP *TCPConfig `json:"tcp,omitempty"`
}

// HTTPConfig is a presence-signal placeholder for L7 exposure. Future
// knobs (TLS, hostname override, gateway-class selection) land here
// without breaking activation semantics.
type HTTPConfig struct{}

// TCPConfig is a presence-signal placeholder for L4 NLB exposure.
// Future knobs (alt port, source CIDR allowlist) land here without
// breaking activation semantics.
type TCPConfig struct{}

// HTTPEnabled reports whether L7 HTTP exposure is requested.
//
// Treats legacy `networking: {}` (neither sub-struct set) as enabled,
// preserving the pre-sub-struct implicit-HTTP behavior. Manifests that
// wrote `networking: {}` to opt into HTTP continue to work.
func (n *NetworkingConfig) HTTPEnabled() bool {
	if n == nil {
		return false
	}
	if n.HTTP != nil {
		return true
	}
	return n.TCP == nil
}
