package v1alpha1

// DeletionPolicy controls what happens to managed networking resources
// and child SeiNodes when their parent is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
	DeletionPolicyDelete DeletionPolicy = "Delete"
	DeletionPolicyRetain DeletionPolicy = "Retain"
)

// NetworkingConfig is a presence-signal struct: its presence on a
// SeiNodeDeployment opts the deployment into public networking.
//
// Present: the controller provisions a ClusterIP Service plus HTTPRoute
// resources on the platform Gateway for each protocol the node mode
// supports, with hostnames derived from the deployment name, namespace,
// and platform domain env vars.
//
// Absent (nil): the deployment is in-cluster only — the unconditional
// internal ClusterIP Service and per-node headless Services exist, but
// no Gateway-attached routes are created.
//
// The empty-struct shape is intentional. Concrete knobs (TLS, hostname
// override, gateway-class selection) will be added here when real needs
// emerge; the type exists today only to carry presence.
type NetworkingConfig struct{}
