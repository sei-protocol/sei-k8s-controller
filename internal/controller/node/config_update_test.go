package node

import (
	"context"
	"testing"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/client-go/tools/record"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
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

// Exercise both controller reconciles through the fake API status subresource,
// rather than relying only on the planner/executor's in-memory mutations.
func TestConfigUpdateReconcilePersistsHashAndSecondReconcileDoesNotRestart(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	node, sts := runningFullNode(t, "config-reconcile", "default")
	node.Status.CurrentConfigValuesHash = "before-materialization"
	node.Spec.ConfigValues = []seiv1alpha1.ConfigValue{{
		FileName: "config.toml", Key: "custom", Value: apiextensionsv1.JSON{Raw: []byte("true")},
	}}
	mustBuildPlan(t, node)
	capturedHash := node.Status.Plan.ConfigValuesHash
	g.Expect(capturedHash).NotTo(BeEmpty())
	mock := &mockSidecarClient{taskResults: make(map[uuid.UUID]*sidecar.TaskResult)}
	for _, pt := range node.Status.Plan.Tasks {
		id := uuid.MustParse(pt.ID)
		mock.taskResults[id] = completedResult(id, pt.Type, nil)
	}
	reconciler, kube := newNodeReconcilerWithSidecar(t, mock, node, sts)
	_, err := reconciler.Reconcile(ctx, nodeReqFor(node.Name, node.Namespace))
	g.Expect(err).NotTo(HaveOccurred())
	observed := fetchNode(t, kube, node.Name, node.Namespace)
	g.Expect(observed.Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
	g.Expect(observed.Status.CurrentConfigValuesHash).To(Equal(capturedHash))
	submissions := len(mock.submitted)
	g.Expect(submissions).To(BeNumerically(">", 0))

	_, err = reconciler.Reconcile(ctx, nodeReqFor(node.Name, node.Namespace))
	g.Expect(err).NotTo(HaveOccurred())
	observed = fetchNode(t, kube, node.Name, node.Namespace)
	g.Expect(observed.Status.Plan).To(BeNil())
	g.Expect(observed.Status.CurrentConfigValuesHash).To(Equal(capturedHash))
	g.Expect(mock.submitted).To(HaveLen(submissions))
}
