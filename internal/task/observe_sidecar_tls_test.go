package task

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func observeTLSNode(secretName string) *seiv1alpha1.SeiNode {
	n := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testReplaceSTS, Namespace: testReplaceNs},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  opkChainID,
			Image:    opkImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	if secretName != "" {
		n.Spec.Sidecar = &seiv1alpha1.SidecarConfig{TLS: &seiv1alpha1.SidecarTLSSpec{SecretName: secretName}}
	}
	return n
}

func observeTLSCfg(t *testing.T, node *seiv1alpha1.SeiNode, sts *appsv1.StatefulSet) ExecutionConfig {
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
		APIReader:  c,
		Scheme:     s,
		Resource:   node,
	}
}

func newObserveTLSExec(t *testing.T, cfg ExecutionConfig) TaskExecution {
	t.Helper()
	params := ObserveSidecarTLSParams{NodeName: testReplaceSTS, Namespace: testReplaceNs}
	raw, _ := json.Marshal(params)
	exec, err := deserializeObserveSidecarTLS("obs-tls-1", raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

func TestObserveSidecarTLS_RolloutComplete_StampsCurrent(t *testing.T) {
	g := NewWithT(t)
	node := observeTLSNode("my-tls-cert")
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(1)},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdatedReplicas: 1, Replicas: 1},
	}

	exec := newObserveTLSExec(t, observeTLSCfg(t, node, sts))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
	g.Expect(node.Status.CurrentSidecarTLSSecretName).To(Equal("my-tls-cert"))
}

func TestObserveSidecarTLS_RolloutInProgress_StaysRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeTLSNode("my-tls-cert")
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(1)},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdatedReplicas: 0, Replicas: 1},
	}

	exec := newObserveTLSExec(t, observeTLSCfg(t, node, sts))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(node.Status.CurrentSidecarTLSSecretName).To(BeEmpty())
}

func TestObserveSidecarTLS_StatefulSetNotFound_StaysRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeTLSNode("my-tls-cert")

	exec := newObserveTLSExec(t, observeTLSCfg(t, node, nil))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
}

func TestObserveSidecarTLS_TLSDisabled_TerminalError(t *testing.T) {
	g := NewWithT(t)
	node := observeTLSNode("") // TLS not enabled

	exec := newObserveTLSExec(t, observeTLSCfg(t, node, nil))

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	var te *TerminalError
	g.Expect(errors.As(err, &te)).To(BeTrue(),
		"scheduling observe-sidecar-tls without spec.sidecar.tls is a planner bug; surface as Terminal")
}
