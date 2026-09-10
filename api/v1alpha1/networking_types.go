package v1alpha1

// DeletionPolicy controls what happens to child SeiNodes when their parent is
// deleted.
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
	DeletionPolicyDelete DeletionPolicy = "Delete"
	DeletionPolicyRetain DeletionPolicy = "Retain"
)

// RetainedFromAnnotation names the SeiNetwork that released a SeiNode under
// DeletionPolicyRetain. Set alongside RetainReasonAnnotation in the same patch
// that drops the owner reference, so a validator that outlives its network is
// recognisable as a deliberate retain rather than a leak. Cleared when a
// SeiNetwork adopts the node again.
const RetainedFromAnnotation = "sei.io/retained-from-seinetwork"

// RetainReasonAnnotation carries the operator-facing reason the SeiNode was
// retained. See RetainedFromAnnotation.
const RetainReasonAnnotation = "sei.io/retain-reason"
