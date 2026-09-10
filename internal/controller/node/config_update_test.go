package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/tools/record"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

func TestConfigPlanBuildRejectionEmitsWarning(t *testing.T) {
	g := NewWithT(t)
	node := newGenesisNode("validator", "default")
	node.Status.Phase = seiv1alpha1.PhaseRunning
	node.Status.CurrentImage = node.Spec.Image
	node.Status.CurrentConfigValuesHash = "previous"
	node.Spec.ConfigValues = []seiv1alpha1.ConfigValue{{
		FileName: "config.toml", Key: "custom", Value: apiextensionsv1.JSON{Raw: []byte("null")},
	}}
	recorder := record.NewFakeRecorder(1)
	reconciler := &SeiNodeReconciler{Planner: &planner.NodeResolver{}, Recorder: recorder}
	_, _, err := reconciler.resolveDriftPlan(context.Background(), node, nil, nil)
	g.Expect(err).To(HaveOccurred())
	g.Expect(node.Status.Plan).To(BeNil())
	g.Expect(recorder.Events).To(Receive(ContainSubstring("Warning PlanBuildFailed")))
	g.Expect(node.Status.CurrentConfigValuesHash).To(Equal("previous"))
}
