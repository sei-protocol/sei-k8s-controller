package node

import (
	"fmt"
	"maps"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	nodeLabel           = "sei.io/node"
	dataDir             = "/sei"
	defaultSidecarImage = "189176372795.dkr.ecr.us-east-2.amazonaws.com/sei/seictl@sha256:54325467253ad86cb486a97f5e9e57789e7c17c1faf844e730c89f175374bfe1"
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

func nodeDataPVCName(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("data-%s", node.Name)
}
