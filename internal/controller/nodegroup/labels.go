package nodegroup

import (
	"fmt"
	"maps"
	"strconv"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	groupLabel        = "sei.io/group"
	groupOrdinalLabel = "sei.io/group-ordinal"
	nodeLabel         = "sei.io/node"
)

func seiNodeName(group *seiv1alpha1.SeiNodeGroup, ordinal int) string {
	return fmt.Sprintf("%s-%d", group.Name, ordinal)
}

func externalServiceName(group *seiv1alpha1.SeiNodeGroup) string {
	return fmt.Sprintf("%s-external", group.Name)
}

// groupSelector returns the label selector used by the shared external
// Service, AuthorizationPolicy, and ServiceMonitor to target all pods
// in the group.
func groupSelector(group *seiv1alpha1.SeiNodeGroup) map[string]string {
	return map[string]string{groupLabel: group.Name}
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
