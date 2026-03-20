package node

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	nodeLabel           = "sei.io/node"
	componentLabel      = "sei.io/component"
	dataDir             = "/sei"
	defaultSidecarImage = "ghcr.io/sei-protocol/seictl@sha256:8834d1f312b3427ffc39ecba705114299a7a3964672f60001ad76b0bc708b155"
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
