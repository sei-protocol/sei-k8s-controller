package nodegroup

import (
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

func seiNodeName(group *seiv1alpha1.SeiNodeGroup, ordinal int) string {
	return fmt.Sprintf("%s-%d", group.Name, ordinal)
}

// activeRevision returns the current active revision string for a group.
// During a deployment, this comes from status; otherwise from the group's
// generation.
func activeRevision(group *seiv1alpha1.SeiNodeGroup) string {
	if group.Status.Deployment != nil && group.Status.Deployment.IncumbentRevision != "" {
		return group.Status.Deployment.IncumbentRevision
	}
	return strconv.FormatInt(group.Generation, 10)
}

func externalServiceName(group *seiv1alpha1.SeiNodeGroup) string {
	return fmt.Sprintf("%s-external", group.Name)
}

// groupSelector returns the label selector used by the shared external
// Service, AuthorizationPolicy, and ServiceMonitor to target all pods
// in the group. Includes the active revision label so that during a
// blue-green deployment, only the active set receives traffic.
func groupSelector(group *seiv1alpha1.SeiNodeGroup) map[string]string {
	return map[string]string{
		groupLabel:    group.Name,
		revisionLabel: activeRevision(group),
	}
}

// seiNodeLabels builds the metadata labels for a child SeiNode.
// User-provided template labels are applied first; system labels
// overwrite to prevent accidental breakage.
func seiNodeLabels(group *seiv1alpha1.SeiNodeGroup, ordinal int) map[string]string {
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
func seiNodeAnnotations(group *seiv1alpha1.SeiNodeGroup) map[string]string {
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
func resourceLabels(group *seiv1alpha1.SeiNodeGroup) map[string]string {
	return map[string]string{groupLabel: group.Name}
}

// managedByAnnotations returns the standard annotation that marks a resource
// as owned by the seinodegroup controller. Useful for operators to identify
// resources subject to periodic drift correction via polling reconciliation.
func managedByAnnotations() map[string]string {
	return map[string]string{managedByAnnotation: controllerName}
}
