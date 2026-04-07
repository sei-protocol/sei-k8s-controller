package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
)

// DeletionPolicy controls what happens to managed networking resources
// and child SeiNodes when their parent is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
	DeletionPolicyDelete DeletionPolicy = "Delete"
	DeletionPolicyRetain DeletionPolicy = "Retain"
)

// NetworkingConfig controls how the group is exposed to traffic.
//
// Routing uses the Kubernetes Gateway API exclusively; the platform must
// install the Gateway API CRDs (v1+) and a Gateway implementation such
// as Istio before HTTPRoute resources will take effect.
// +kubebuilder:validation:XValidation:rule="!has(self.gateway) || has(self.service)",message="gateway requires service to be configured"
type NetworkingConfig struct {
	// Service creates a non-headless Service shared across all replicas.
	// Each SeiNode still gets its own headless Service for pod DNS.
	// +optional
	Service *ExternalServiceConfig `json:"service,omitempty"`

	// Gateway creates a gateway.networking.k8s.io/v1 HTTPRoute
	// targeting a shared Gateway (e.g. Istio ingress gateway).
	// +optional
	Gateway *GatewayRouteConfig `json:"gateway,omitempty"`

	// Isolation configures network-level access control for node pods.
	// +optional
	Isolation *NetworkIsolationConfig `json:"isolation,omitempty"`
}

// ExternalServiceConfig defines the shared non-headless Service.
// Ports are derived automatically from the node mode via
// seiconfig.NodePortsForMode — no manual port selection needed.
type ExternalServiceConfig struct {
	// Type is the Kubernetes Service type.
	// +optional
	// +kubebuilder:default=ClusterIP
	// +kubebuilder:validation:Enum=ClusterIP;LoadBalancer;NodePort
	Type corev1.ServiceType `json:"type,omitempty"`

	// Annotations are merged onto the Service metadata.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// GatewayRouteConfig creates gateway.networking.k8s.io/v1 HTTPRoute resources
// targeting the platform Gateway (configured via SEI_GATEWAY_NAME and
// SEI_GATEWAY_NAMESPACE environment variables on the controller).
//
// +kubebuilder:validation:XValidation:rule="(has(self.hostnames) && size(self.hostnames) > 0) || (has(self.baseDomain) && self.baseDomain != ”)",message="at least one of hostnames or baseDomain must be set"
type GatewayRouteConfig struct {
	// Hostnames routes all listed hostnames to the RPC port (26657).
	// For multi-protocol routing, use BaseDomain instead.
	// +optional
	Hostnames []string `json:"hostnames,omitempty"`

	// BaseDomain generates HTTPRoutes for all standard Sei protocols
	// using conventional subdomain prefixes (rpc.*, rest.*, grpc.*,
	// evm-rpc.*, evm-ws.*), each routing to the correct backend port.
	// +optional
	BaseDomain string `json:"baseDomain,omitempty"`

	// Annotations are merged onto the HTTPRoute metadata.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// NetworkIsolationConfig defines network-level access control.
type NetworkIsolationConfig struct {
	// AuthorizationPolicy creates an Istio AuthorizationPolicy
	// restricting which identities can reach node pods.
	// +optional
	AuthorizationPolicy *AuthorizationPolicyConfig `json:"authorizationPolicy,omitempty"`
}

// AuthorizationPolicyConfig defines allowed traffic sources.
type AuthorizationPolicyConfig struct {
	// AllowedSources defines who can reach this group's pods.
	// The controller generates an ALLOW policy; traffic from
	// sources not listed here is denied.
	// +kubebuilder:validation:MinItems=1
	AllowedSources []TrafficSource `json:"allowedSources"`
}

// TrafficSource identifies a set of callers by Istio identity.
// +kubebuilder:validation:XValidation:rule="has(self.principals) || has(self.namespaces)",message="at least one of principals or namespaces must be set"
type TrafficSource struct {
	// Principals are SPIFFE identities (e.g.
	// "cluster.local/ns/istio-system/sa/istio-ingressgateway").
	// +optional
	Principals []string `json:"principals,omitempty"`

	// Namespaces allows all pods in these namespaces.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`
}
