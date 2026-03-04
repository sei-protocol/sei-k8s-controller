package nodepool

import (
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	nodepoolLabel       = "sei.io/nodepool"
	dataDir             = "/sei"
	genesisDir          = "/genesis"
	efsStorageClass     = "efs-sc"
	nodeServiceAccount  = "seid-node"
	defaultStorageSize  = "1000Gi"
	defaultStorageClass = ""
)

func resourceLabels(sn *seiv1alpha1.SeiNodePool) map[string]string {
	return map[string]string{
		nodepoolLabel: sn.Name,
	}
}

func dataPVCName(sn *seiv1alpha1.SeiNodePool, ordinal int) string {
	return fmt.Sprintf("data-%s-%d", sn.Name, ordinal)
}

func seiNodeName(sn *seiv1alpha1.SeiNodePool, ordinal int) string {
	return fmt.Sprintf("%s-%d", sn.Name, ordinal)
}

func prepJobName(sn *seiv1alpha1.SeiNodePool, ordinal int) string {
	return fmt.Sprintf("%s-prep-%d", sn.Name, ordinal)
}

func genesisPVCName(sn *seiv1alpha1.SeiNodePool) string {
	return fmt.Sprintf("%s-genesis-data", sn.Name)
}

func genesisNodesDir(sn *seiv1alpha1.SeiNodePool) string {
	return fmt.Sprintf("/genesis/%s/nodes", sn.Name)
}
