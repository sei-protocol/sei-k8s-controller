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

// testClusterIssuerKind is the kube-rbac-proxy default IssuerKind used
// across the task-level TLS tests in this package.
const testClusterIssuerKind = "ClusterIssuer"

func observeTLSNode(withTLS bool) *seiv1alpha1.SeiNode {
	n := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testReplaceSTS, Namespace: testReplaceNs},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  opkChainID,
			Image:    opkImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{Phase: seiv1alpha1.PhaseRunning},
	}
	if withTLS {
		n.Spec.Sidecar = &seiv1alpha1.SidecarConfig{
			TLS: &seiv1alpha1.SidecarTLSSpec{IssuerName: "ca", IssuerKind: testClusterIssuerKind},
		}
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

func TestObserveSidecarTLS_RolloutComplete_StampsStatus(t *testing.T) {
	g := NewWithT(t)
	node := observeTLSNode(true)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(1)},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdatedReplicas: 1, Replicas: 1},
	}

	exec := newObserveTLSExec(t, observeTLSCfg(t, node, sts))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
	g.Expect(node.Status.CurrentSidecarTLS).NotTo(BeNil())
	g.Expect(node.Status.CurrentSidecarTLS.IssuerName).To(Equal("ca"))
	g.Expect(node.Status.CurrentSidecarTLS.IssuerKind).To(Equal("ClusterIssuer"))
}

func TestObserveSidecarTLS_RollingUpdate_StaysRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeTLSNode(true)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(1)},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdatedReplicas: 0, Replicas: 1},
	}

	exec := newObserveTLSExec(t, observeTLSCfg(t, node, sts))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(node.Status.CurrentSidecarTLS).To(BeNil(), "status must not be stamped before rollout converges")
}

func TestObserveSidecarTLS_StaleGeneration_StaysRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeTLSNode(true)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Generation: 3},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(1)},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdatedReplicas: 1, Replicas: 1},
	}

	exec := newObserveTLSExec(t, observeTLSCfg(t, node, sts))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(node.Status.CurrentSidecarTLS).To(BeNil())
}

func TestObserveSidecarTLS_StatefulSetNotFound_StaysRunning(t *testing.T) {
	g := NewWithT(t)
	node := observeTLSNode(true)

	exec := newObserveTLSExec(t, observeTLSCfg(t, node, nil))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
}

func TestObserveSidecarTLS_SpecNilOnConverge_StampsNil(t *testing.T) {
	// Forward-compat: if a future disable-path plan reaches this observer
	// with spec.sidecar.tls == nil, the observer must stamp nil, not a
	// stale issuer. Exercises the helper.
	g := NewWithT(t)
	node := observeTLSNode(false) // no TLS spec
	// Seed a stale status to verify it gets cleared.
	node.Status.CurrentSidecarTLS = &seiv1alpha1.SidecarTLSStatus{IssuerName: "stale", IssuerKind: "ClusterIssuer"}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, Generation: 2},
		Spec:       appsv1.StatefulSetSpec{Replicas: int32Ptr(1)},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdatedReplicas: 1, Replicas: 1},
	}

	exec := newObserveTLSExec(t, observeTLSCfg(t, node, sts))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
	g.Expect(node.Status.CurrentSidecarTLS).To(BeNil(),
		"observer must mirror spec — nil spec stamps nil status")
}

func TestObserveSidecarTLS_DeserializeEmptyParams(t *testing.T) {
	g := NewWithT(t)
	node := observeTLSNode(true)
	cfg := observeTLSCfg(t, node, nil)

	exec, err := deserializeObserveSidecarTLS("obs-empty", nil, cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exec).NotTo(BeNil())
}
