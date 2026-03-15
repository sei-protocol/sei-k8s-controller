package node

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	nodeLabel           = "sei.io/node"
	dataDir             = "/sei"
	nodeServiceAccount  = "seid-node"
	defaultSidecarImage = "ghcr.io/sei-protocol/seictl@sha256:ad50d546c3aa4bfa220b3148ffc40b59d34d7c8d09522fa1ff25f635f2f1e710"
)

func resourceLabelsForNode(node *seiv1alpha1.SeiNode) map[string]string {
	return map[string]string{
		nodeLabel: node.Name,
	}
}

func nodeDataPVCName(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("data-%s", node.Name)
}
