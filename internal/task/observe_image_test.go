package task

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func observeImageNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Namespace: "default", UID: "uid-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "atlantic-2",
			Image:    "sei:v2.0.0",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase:        seiv1alpha1.PhaseRunning,
			CurrentImage: "sei:v1.0.0",
		},
	}
}

func observeImageCfg(t *testing.T, node *seiv1alpha1.SeiNode, sts *appsv1.StatefulSet) ExecutionConfig {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(s)
	if sts != nil {
		builder = builder.WithObjects(sts)
	}
	c := builder.Build()
	return ExecutionConfig{
		KubeClient: c,
		Scheme:     s,
		Resource:   node,
	}
}

func int32Ptr(v int32) *int32 { return &v }

func newObserveImageExec(t *testing.T, cfg ExecutionConfig) TaskExecution {
	t.Helper()
	params := ObserveImageParams{NodeName: "node-1", Namespace: "default"}
	raw, _ := json.Marshal(params)
	exec, err := deserializeObserveImage("obs-test", raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

func TestObserveImage_RolloutComplete_SetsCurrentImage(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(1)},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdatedReplicas: 1, Replicas: 1},
	}

	exec := newObserveImageExec(t, observeImageCfg(t, node, sts))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
	g.Expect(node.Status.CurrentImage).To(Equal("sei:v2.0.0"))
}

func TestObserveImage_RollingUpdate_ReturnsRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(1)},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdatedReplicas: 0, Replicas: 1},
	}

	exec := newObserveImageExec(t, observeImageCfg(t, node, sts))

	// Execute returns nil (rollout not done), task stays Pending.
	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(node.Status.CurrentImage).To(Equal("sei:v1.0.0"))
}

func TestObserveImage_StaleGeneration_ReturnsRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Generation: 3},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(1)},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdatedReplicas: 1, Replicas: 1},
	}

	exec := newObserveImageExec(t, observeImageCfg(t, node, sts))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(node.Status.CurrentImage).To(Equal("sei:v1.0.0"))
}

func TestObserveImage_StatefulSetNotFound_ReturnsRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()

	exec := newObserveImageExec(t, observeImageCfg(t, node, nil))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(node.Status.CurrentImage).To(Equal("sei:v1.0.0"))
}

func TestObserveImage_NilReplicas_ReturnsRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Generation: 2},
		Spec:       appsv1.StatefulSetSpec{}, // nil Replicas
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdatedReplicas: 1, Replicas: 1},
	}

	exec := newObserveImageExec(t, observeImageCfg(t, node, sts))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
}

func TestObserveImage_DeserializeEmptyParams(t *testing.T) {
	g := NewWithT(t)
	node := observeImageNode()
	cfg := observeImageCfg(t, node, nil)

	exec, err := deserializeObserveImage("obs-empty", nil, cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exec).NotTo(BeNil())
}
