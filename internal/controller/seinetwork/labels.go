package seinetwork

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	// groupLabel is frozen as a GitOps networking selector key; child Services
	// and listing select on it.
	groupLabel          = "sei.io/nodedeployment"
	groupOrdinalLabel   = "sei.io/nodedeployment-ordinal"
	revisionLabel       = "sei.io/revision"
	chainLabel          = "sei.io/chain"
	managedByAnnotation = "sei.io/managed-by"
)

// seiNodeName is published as Status.PerPodServices[].Name and equals the
// headless Service name; the format is part of the public interface.
func seiNodeName(network *seiv1alpha1.SeiNetwork, ordinal int) string {
	return fmt.Sprintf("%s-%d", network.Name, ordinal)
}

// activeRevision returns the revision string stamped on child pods.
// Used purely as an observability label (`kubectl get -l sei.io/revision=...`);
// no Service selector filters on it.
func activeRevision(network *seiv1alpha1.SeiNetwork) string {
	return strconv.FormatInt(network.Generation, 10)
}

// groupSelector returns the label selector used by the internal Service and
// child-node listing. InPlace rollouts update pods in place, so traffic
// must always reach every network member regardless of generation.
func groupSelector(network *seiv1alpha1.SeiNetwork) map[string]string {
	return map[string]string{groupLabel: network.Name}
}

// seiNodeLabels builds the metadata labels for a child SeiNode. The reserved
// group/ordinal/revision/chain labels are controller-owned and authoritative.
func seiNodeLabels(network *seiv1alpha1.SeiNetwork, ordinal int) map[string]string {
	labels := make(map[string]string, 4)
	labels[groupLabel] = network.Name
	labels[groupOrdinalLabel] = strconv.Itoa(ordinal)
	labels[revisionLabel] = activeRevision(network)
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

// templateHash computes a hash over spec fields that trigger a deployment
// plan when changed. Currently tracked: chainId, image, and sidecar image.
// Fields like configOverrides and replica count propagate in-place via
// ensureSeiNode without requiring a deployment plan. chainId is immutable, so
// in practice only an image (or sidecar image) change drives a rollout.
func templateHash(spec *seiv1alpha1.SeiNetworkSpec) string {
	h := sha256.New()
	h.Write([]byte(spec.Genesis.ChainID))
	h.Write([]byte(spec.Image))
	if spec.Sidecar != nil {
		h.Write([]byte(spec.Sidecar.Image))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
