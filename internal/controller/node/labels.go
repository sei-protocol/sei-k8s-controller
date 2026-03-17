package node

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	nodeLabel           = "sei.io/node"
	dataDir             = "/sei"
	defaultSidecarImage = "ghcr.io/sei-protocol/seictl@sha256:80add5808602b0f9e92d6d7a8951ec8eb8bbcc9aaa8b2501bc1c4ea65adfa659"
)

func resourceLabelsForNode(node *seiv1alpha1.SeiNode) map[string]string {
	return map[string]string{
		nodeLabel: node.Name,
	}
}

func nodeDataPVCName(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("data-%s", node.Name)
}
