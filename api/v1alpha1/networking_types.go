package v1alpha1

// DeletionPolicy controls what happens to managed networking resources
// and child SeiNodes when their parent is deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
	DeletionPolicyDelete DeletionPolicy = "Delete"
	DeletionPolicyRetain DeletionPolicy = "Retain"
)

// NetworkingConfig enables public networking for the deployment.
//
// When present, the controller creates a ClusterIP Service (as the
// HTTPRoute backend) and generates HTTPRoute resources on the platform
// Gateway for each protocol the node mode supports. Hostnames are
// derived automatically from the deployment name, namespace, and the
// platform domain env vars.
//
// When absent (nil), the deployment is private — only per-node headless
// Services exist for in-cluster access.
type NetworkingConfig struct{}
