package seinetwork

import (
	"fmt"
	"strconv"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	// groupLabel / groupOrdinalLabel are the FROZEN GitOps selector keys.
	// Child Services, listing, and external GitOps selectors all match on them,
	// so they are kept for selector continuity and must not change or be
	// removed. They are stamped alongside the canonical seinetwork keys below.
	groupLabel        = "sei.io/nodedeployment"
	groupOrdinalLabel = "sei.io/nodedeployment-ordinal"

	// seinetworkLabel / seinetworkOrdinalLabel are the NEW canonical keys,
	// stamped on every child SeiNode/pod alongside the frozen nodedeployment
	// keys. Selectors migrate onto these later; once nothing selects on the
	// nodedeployment keys they get retired.
	seinetworkLabel        = "sei.io/seinetwork"
	seinetworkOrdinalLabel = "sei.io/seinetwork-ordinal"

	chainLabel = "sei.io/chain"
	// roleLabel is the observability/selection identity the dropped
	// SeiNodeTemplate used to carry. A SeiNetwork is always a validator
	// pool, so children are stamped role=validator here — matching the
	// pod-template sei.io/role the SeiNode controller derives — so GitOps
	// and selectors that filter SeiNodes by role still resolve.
	roleLabel     = "sei.io/role"
	roleValidator = "validator"

	managedByAnnotation = "sei.io/managed-by"
)

// seiNodeName is published as Status.PerPodServices[].Name and equals the
// headless Service name; the format is part of the public interface.
func seiNodeName(network *seiv1alpha1.SeiNetwork, ordinal int) string {
	return fmt.Sprintf("%s-%d", network.Name, ordinal)
}

// groupSelector returns the label selector used by the internal Service and
// child-node listing. Image changes update pods in place, so traffic must
// always reach every network member.
func groupSelector(network *seiv1alpha1.SeiNetwork) map[string]string {
	return map[string]string{groupLabel: network.Name}
}

// seiNodeLabels builds the metadata labels for a child SeiNode. The reserved
// group/ordinal/chain/role labels are controller-owned and authoritative. Both
// the frozen nodedeployment keys and the canonical seinetwork keys are stamped
// here so every controller-managed sei.io/* group label has a single
// authoritative origin. sei.io/role=validator records the pool's fixed role
// (every SeiNetwork is a validator pool).
func seiNodeLabels(network *seiv1alpha1.SeiNetwork, ordinal int) map[string]string {
	ord := strconv.Itoa(ordinal)
	labels := make(map[string]string, 6)
	labels[groupLabel] = network.Name
	labels[groupOrdinalLabel] = ord
	labels[seinetworkLabel] = network.Name
	labels[seinetworkOrdinalLabel] = ord
	labels[chainLabel] = network.Spec.Genesis.ChainID
	labels[roleLabel] = roleValidator
	return labels
}

// seiNodeAnnotations builds the metadata annotations for a child SeiNode.
// The scoped genesis spec carries no per-node annotation knob, so children
// get none.
func seiNodeAnnotations(_ *seiv1alpha1.SeiNetwork) map[string]string {
	return nil
}

// resourceLabels returns labels for resources owned by the network.
func resourceLabels(network *seiv1alpha1.SeiNetwork) map[string]string {
	return map[string]string{groupLabel: network.Name}
}

// managedByAnnotations returns the standard annotation that marks a resource
// as owned by the seinetwork controller. Useful for operators to identify
// resources subject to periodic drift correction via polling reconciliation.
func managedByAnnotations() map[string]string {
	return map[string]string{managedByAnnotation: controllerName}
}
