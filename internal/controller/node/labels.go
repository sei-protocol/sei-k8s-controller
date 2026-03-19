package node

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	nodeLabel           = "sei.io/node"
	componentLabel      = "sei.io/component"
	dataDir             = "/sei"
	defaultSidecarImage = "ghcr.io/sei-protocol/seictl@sha256:64f92fb5bc3f451b3cd23d95275685b7fed28ab4dff36a0182267dc77e266c49"
)

func resourceLabelsForNode(node *seiv1alpha1.SeiNode) map[string]string {
	return map[string]string{
		nodeLabel: node.Name,
	}
}

func preInitLabelsForNode(node *seiv1alpha1.SeiNode) map[string]string {
	return map[string]string{
		nodeLabel:      node.Name,
		componentLabel: "pre-init",
	}
}

func nodeDataPVCName(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("data-%s", node.Name)
}
