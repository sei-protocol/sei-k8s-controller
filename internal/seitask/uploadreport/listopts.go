package uploadreport

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// metaListOpts builds the ListOptions used by ListWorkflowPods. Extracted so
// tests + production wiring share the same label-selector contract.
func metaListOpts(workflowName string) metav1.ListOptions {
	return metav1.ListOptions{
		LabelSelector: "chaos-mesh.org/workflow=" + workflowName,
	}
}
