package nodegroup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strconv"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	groupLabel          = "sei.io/nodegroup"
	groupOrdinalLabel   = "sei.io/nodegroup-ordinal"
	revisionLabel       = "sei.io/revision"
	nodeLabel           = "sei.io/node"
	managedByAnnotation = "sei.io/managed-by"
)

func seiNodeName(group *seiv1alpha1.SeiNodeDeployment, ordinal int) string {
	return fmt.Sprintf("%s-%d", group.Name, ordinal)
}

// activeRevision returns the revision string for the currently active
// node set. Used for traffic routing during deployments and for
// observability labels on pods.
func activeRevision(group *seiv1alpha1.SeiNodeDeployment) string {
	if group.Status.Deployment != nil && group.Status.Deployment.IncumbentRevision != "" {
		return group.Status.Deployment.IncumbentRevision
	}
	return strconv.FormatInt(group.Generation, 10)
}

func externalServiceName(group *seiv1alpha1.SeiNodeDeployment) string {
	return fmt.Sprintf("%s-external", group.Name)
}

// groupSelector returns the label selector used by the shared external
// Service, AuthorizationPolicy, and ServiceMonitor. During an active
// deployment, it includes the revision label to pin traffic to the
// active set. At steady state, it selects by group membership only.
func groupSelector(group *seiv1alpha1.SeiNodeDeployment) map[string]string {
	if group.Status.Deployment != nil {
		return map[string]string{
			groupLabel:    group.Name,
			revisionLabel: activeRevision(group),
		}
	}
	return map[string]string{groupLabel: group.Name}
}

// groupOnlySelector returns a selector matching all nodes owned by the
// group, regardless of revision. Used for listing and scale-down where
// we need to see every child node.
func groupOnlySelector(group *seiv1alpha1.SeiNodeDeployment) map[string]string {
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

// templateHash computes a hash over spec fields that require new nodes
// when changed. Any container image change triggers a full pod restart,
// so both the chain binary and sidecar images are included.
func templateHash(spec *seiv1alpha1.SeiNodeSpec) string {
	h := sha256.New()
	h.Write([]byte(spec.ChainID))
	h.Write([]byte(spec.Image))
	if spec.Entrypoint != nil {
		for _, c := range spec.Entrypoint.Command {
			h.Write([]byte(c))
		}
		for _, a := range spec.Entrypoint.Args {
			h.Write([]byte(a))
		}
	}
	if spec.Sidecar != nil {
		h.Write([]byte(spec.Sidecar.Image))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
