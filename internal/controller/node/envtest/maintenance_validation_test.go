//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// Admission coverage of spec.maintenance.hold (spec 010 Requirement 1,
// SC-001). These cases need no controller.

// 010 Req 1.1: both values are accepted on a nodeConfig node.
func TestMaintenanceHold_Values_Accepted(t *testing.T) {
	for _, hold := range []seiv1alpha1.MaintenanceHold{seiv1alpha1.MaintenanceHoldImmediate, seiv1alpha1.MaintenanceHoldAfterExit} {
		t.Run(string(hold), func(t *testing.T) {
			g := NewWithT(t)
			ns := makeNamespace(t)
			node := nodeConfigNode(ns, "hold-ok")
			node.Spec.Maintenance = &seiv1alpha1.MaintenanceSpec{Hold: hold}
			g.Expect(testCli.Create(testCtx, node)).To(Succeed())
		})
	}
}

// 010 Req 1.2: a hold needs spec.nodeConfig.
func TestMaintenanceHold_RequiresNodeConfig(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	node := nodeConfigNode(ns, "hold-no-nc")
	node.Spec.NodeConfig = nil
	node.Spec.Maintenance = &seiv1alpha1.MaintenanceSpec{Hold: seiv1alpha1.MaintenanceHoldImmediate}

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("needs spec.nodeConfig"))
}

// 010 Req 1.1: any other value is rejected by the enum.
func TestMaintenanceHold_UnknownValue_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "sei.io/v1alpha1",
		"kind":       "SeiNode",
		"metadata":   map[string]any{"name": "hold-bad", "namespace": ns},
		"spec": map[string]any{
			"chainId":  "envtest-1",
			"image":    "sei:latest",
			"fullNode": map[string]any{},
			"nodeConfig": map[string]any{
				"configRef": map[string]any{"name": "rpc-config-v1"},
				"appRef":    map[string]any{"name": "rpc-app-v1"},
			},
			"maintenance": map[string]any{"hold": "Forever"},
		},
	}}
	g.Expect(testCli.Create(testCtx, obj)).To(HaveOccurred())
}
