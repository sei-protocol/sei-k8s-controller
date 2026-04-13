package node

import (
	"fmt"
	"maps"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
)

const (
	nodeLabel           = "sei.io/node"
	dataDir             = platform.DataDir
	defaultSidecarImage = platform.DefaultSidecarImage
)

// selectorLabelsForNode returns the minimal, immutable label set used as the
// StatefulSet selector. Only sei.io/node is included because each SeiNode
// has a unique name within a namespace, making it sufficient to select its
// pods. Mutable labels (sei.io/revision, podLabels) must NOT appear here
// because Kubernetes forbids changing a StatefulSet's selector after creation.
func selectorLabelsForNode(node *seiv1alpha1.SeiNode) map[string]string {
	return map[string]string{nodeLabel: node.Name}
}

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
