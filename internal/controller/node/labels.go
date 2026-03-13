package node

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	nodeLabel           = "sei.io/node"
	dataDir             = "/sei"
	nodeServiceAccount  = "seid-node"
	defaultStorageSize  = "1000Gi"
	defaultStorageClass = ""
	defaultSidecarImage = "ghcr.io/sei-protocol/seictl@sha256:bdd1861e4d0803b5318fee649b781edb814ed845779b1433a8a7ebf6f856e907"
)

func resourceLabelsForNode(node *seiv1alpha1.SeiNode) map[string]string {
	return map[string]string{
		nodeLabel: node.Name,
	}
}

func nodeDataPVCName(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("data-%s", node.Name)
}
