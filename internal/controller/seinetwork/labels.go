package seinetwork

import (
	"fmt"
	"strconv"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	// groupLabel is frozen as a GitOps networking selector key; child Services
	// and listing select on it.
	groupLabel          = "sei.io/nodedeployment"
	groupOrdinalLabel   = "sei.io/nodedeployment-ordinal"
	chainLabel          = "sei.io/chain"
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
// group/ordinal/chain labels are controller-owned and authoritative.
func seiNodeLabels(network *seiv1alpha1.SeiNetwork, ordinal int) map[string]string {
	labels := make(map[string]string, 3)
	labels[groupLabel] = network.Name
	labels[groupOrdinalLabel] = strconv.Itoa(ordinal)
	labels[chainLabel] = network.Spec.Genesis.ChainID
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
