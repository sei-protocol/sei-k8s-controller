package task

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	updateImageTestNS    = "default"
	updateImageTestNode  = "node-1"
	updateImageTestUID   = "uid-1"
	updateImageTestChain = "sei"
	updateImageV1        = "sei:v1"
	updateImageV2        = "sei:v2"
)

func updateImageNode(spec, current string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: updateImageTestNode, Namespace: updateImageTestNS, UID: updateImageTestUID},
		Spec:       seiv1alpha1.SeiNodeSpec{ChainID: updateImageTestChain, Image: spec, Validator: &seiv1alpha1.ValidatorSpec{}},
		Status:     seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning, CurrentImage: current},
	}
}

func updateImageCfg(t *testing.T, objs ...client.Object) (ExecutionConfig, client.Client) {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}).
		Build()
	return ExecutionConfig{KubeClient: c, APIReader: c, Scheme: s}, c
}

func newExec(t *testing.T, cfg ExecutionConfig) TaskExecution {
	t.Helper()
	raw, _ := json.Marshal(UpdateNodeImageParams{NodeName: updateImageTestNode, Namespace: updateImageTestNS, Image: updateImageV2})
	exec, err := deserializeUpdateNodeImage("u-1", raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

func TestUpdateNodeImage_PatchesSpecImage(t *testing.T) {
	g := NewWithT(t)
	node := updateImageNode(updateImageV1, updateImageV1)
	cfg, c := updateImageCfg(t, node)
	exec := newExec(t, cfg)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	out := &seiv1alpha1.SeiNode{}
	g.Expect(c.Get(context.Background(), types.NamespacedName{Name: updateImageTestNode, Namespace: updateImageTestNS}, out)).To(Succeed())
	g.Expect(out.Spec.Image).To(Equal(updateImageV2))
}

func TestUpdateNodeImage_AlreadyPatched_NoOp(t *testing.T) {
	g := NewWithT(t)
	node := updateImageNode(updateImageV2, updateImageV1)
	cfg, _ := updateImageCfg(t, node)
	exec := newExec(t, cfg)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
}

func TestUpdateNodeImage_CurrentImageMatches_Complete(t *testing.T) {
	g := NewWithT(t)
	node := updateImageNode(updateImageV2, updateImageV2)
	cfg, _ := updateImageCfg(t, node)
	exec := newExec(t, cfg)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
}

func TestUpdateNodeImage_TargetMissing_TerminalOnExecute(t *testing.T) {
	g := NewWithT(t)
	cfg, _ := updateImageCfg(t)
	exec := newExec(t, cfg)
	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("not found"))
	var terminal *TerminalError
	g.Expect(errors.As(err, &terminal)).To(BeTrue())
}

func TestUpdateNodeImage_InvalidParams_TerminalOnDeserialize(t *testing.T) {
	g := NewWithT(t)
	cfg, _ := updateImageCfg(t)
	_, err := deserializeUpdateNodeImage("u-1", []byte(`{"nodeName":"","namespace":"","image":""}`), cfg)
	g.Expect(err).To(HaveOccurred())
}
