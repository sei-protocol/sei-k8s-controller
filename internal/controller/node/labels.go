package node

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	nodeLabel                  = "sei.io/node"
	dataDir                    = "/sei"
	nodeServiceAccount         = "seid-node"
	defaultStorageSize         = "1000Gi"
	defaultStorageClass        = ""
	snapshotSyncContainerImage = "189176372795.dkr.ecr.us-east-2.amazonaws.com/sei/snapshot-sync:latest"
)

func resourceLabelsForNode(node *seiv1alpha1.SeiNode) map[string]string {
	return map[string]string{
		nodeLabel: node.Name,
	}
}

func nodeDataPVCName(node *seiv1alpha1.SeiNode) string {
	return fmt.Sprintf("data-%s", node.Name)
}
