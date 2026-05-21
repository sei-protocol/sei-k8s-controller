package nodedeployment

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strconv"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	groupLabel          = "sei.io/nodedeployment"
	groupOrdinalLabel   = "sei.io/nodedeployment-ordinal"
	revisionLabel       = "sei.io/revision"
	chainLabel          = "sei.io/chain"
	managedByAnnotation = "sei.io/managed-by"
)

// seiNodeName is published as Status.PerPodServices[].Name and equals the
// headless Service name; the format is part of the public interface.
func seiNodeName(group *seiv1alpha1.SeiNodeDeployment, ordinal int) string {
	return fmt.Sprintf("%s-%d", group.Name, ordinal)
}

// activeRevision returns the revision string stamped on child pods.
// Used purely as an observability label (`kubectl get -l sei.io/revision=...`);
// no Service selector filters on it.
func activeRevision(group *seiv1alpha1.SeiNodeDeployment) string {
	return strconv.FormatInt(group.Generation, 10)
}

func externalServiceName(group *seiv1alpha1.SeiNodeDeployment) string {
	return fmt.Sprintf("%s-external", group.Name)
}

// groupSelector returns the label selector used by Services, HTTPRoutes,
// and child-node listing. InPlace rollouts update pods in place, so traffic
// must always reach every group member regardless of generation.
func groupSelector(group *seiv1alpha1.SeiNodeDeployment) map[string]string {
	return map[string]string{groupLabel: group.Name}
}

// seiNodeLabels builds the metadata labels for a child SeiNode.
// User-provided template labels are applied first; system labels
// overwrite to prevent accidental breakage.
func seiNodeLabels(group *seiv1alpha1.SeiNodeDeployment, ordinal int) map[string]string {
	labels := make(map[string]string)
	if group.Spec.Template.Metadata != nil {
		maps.Copy(labels, group.Spec.Template.Metadata.Labels)
	}
	labels[groupLabel] = group.Name
	labels[groupOrdinalLabel] = strconv.Itoa(ordinal)
	labels[revisionLabel] = activeRevision(group)
	labels[chainLabel] = group.Spec.Template.Spec.ChainID
	return labels
}

// seiNodeAnnotations builds the metadata annotations for a child SeiNode.
func seiNodeAnnotations(group *seiv1alpha1.SeiNodeDeployment) map[string]string {
	if group.Spec.Template.Metadata == nil {
		return nil
	}
	if len(group.Spec.Template.Metadata.Annotations) == 0 {
		return nil
	}
	annotations := make(map[string]string, len(group.Spec.Template.Metadata.Annotations))
	maps.Copy(annotations, group.Spec.Template.Metadata.Annotations)
	return annotations
}

// resourceLabels returns labels for resources owned by the group.
func resourceLabels(group *seiv1alpha1.SeiNodeDeployment) map[string]string {
	return map[string]string{groupLabel: group.Name}
}

// managedByAnnotations returns the standard annotation that marks a resource
// as owned by the seinodedeployment controller. Useful for operators to identify
// resources subject to periodic drift correction via polling reconciliation.
func managedByAnnotations() map[string]string {
	return map[string]string{managedByAnnotation: controllerName}
}

// templateHash computes a hash over spec fields that trigger a deployment
// plan when changed. Currently tracked: chainId, image, and sidecar image.
// Fields like overrides, peers, and replica count propagate in-place via
// ensureSeiNode without requiring a deployment plan.
func templateHash(spec *seiv1alpha1.SeiNodeSpec) string {
	h := sha256.New()
	h.Write([]byte(spec.ChainID))
	h.Write([]byte(spec.Image))
	if spec.Sidecar != nil {
		h.Write([]byte(spec.Sidecar.Image))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
