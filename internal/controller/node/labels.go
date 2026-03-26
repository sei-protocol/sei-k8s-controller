package node

import (
	"fmt"
	"maps"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	nodeLabel           = "sei.io/node"
	componentLabel      = "sei.io/component"
	dataDir             = "/sei"
	defaultSidecarImage = "ghcr.io/sei-protocol/seictl@sha256:8bfef078409c160f03c62fcd969702b3edc9d957369fb56dca9e34e09ac6c99a"
)

// resourceLabelsForNode returns labels for the StatefulSet pod template.
// User-provided podLabels are applied first; the system sei.io/node label
// is set last so it cannot be overridden.
func resourceLabelsForNode(node *seiv1alpha1.SeiNode) map[string]string {
	labels := make(map[string]string, len(node.Spec.PodLabels)+1)
	maps.Copy(labels, node.Spec.PodLabels)
	labels[nodeLabel] = node.Name
	return labels
}

func bootstrapLabelsForNode(node *seiv1alpha1.SeiNode) map[string]string {
	return map[string]string{
		nodeLabel:      node.Name,
		componentLabel: "bootstrap",
	}
}

func nodeDataPVCName(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("data-%s", node.Name)
}
