package nodepool

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func genesisScriptConfigMapName(sn *seiv1alpha1.SeiNodePool) string {
	return sn.Name + "-genesis-script"
}

func generateGenesisScriptConfigMap(sn *seiv1alpha1.SeiNodePool) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      genesisScriptConfigMapName(sn),
			Namespace: sn.Namespace,
			Labels:    resourceLabels(sn),
		},
		Data: map[string]string{
			"generate.sh": genesisScript,
		},
	}
}
