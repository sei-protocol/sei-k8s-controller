package task

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

func waitTLSNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testReplaceSTS, Namespace: testReplaceNs},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  opkChainID,
			Image:    opkImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Sidecar: &seiv1alpha1.SidecarConfig{
				TLS: &seiv1alpha1.SidecarTLSSpec{IssuerName: "ca", IssuerKind: testClusterIssuerKind},
			},
		},
	}
}

func waitTLSCfg(t *testing.T, node *seiv1alpha1.SeiNode, sec *corev1.Secret) ExecutionConfig {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(s)
	if sec != nil {
		builder = builder.WithObjects(sec)
	}
	c := builder.Build()
	return ExecutionConfig{
		KubeClient: c,
		APIReader:  c,
		Scheme:     s,
		Resource:   node,
	}
}

func newWaitTLSExec(t *testing.T, cfg ExecutionConfig) TaskExecution {
	t.Helper()
	params := WaitForSidecarTLSSecretParams{NodeName: testReplaceSTS, Namespace: testReplaceNs}
	raw, _ := json.Marshal(params)
	exec, err := deserializeWaitForSidecarTLSSecret("wait-tls-1", raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return exec
}

func TestWaitForSidecarTLSSecret_SecretReady_Completes(t *testing.T) {
	g := NewWithT(t)
	node := waitTLSNode()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      noderesource.SidecarTLSSecretName(node),
			Namespace: node.Namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte("-----BEGIN CERTIFICATE-----\n..."),
			corev1.TLSPrivateKeyKey: []byte("-----BEGIN PRIVATE KEY-----\n..."),
		},
	}

	exec := newWaitTLSExec(t, waitTLSCfg(t, node, sec))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
}

func TestWaitForSidecarTLSSecret_SecretMissing_StaysRunning(t *testing.T) {
	g := NewWithT(t)
	node := waitTLSNode()

	exec := newWaitTLSExec(t, waitTLSCfg(t, node, nil))

	// cert-manager hasn't issued yet — Secret absent.
	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
}

func TestWaitForSidecarTLSSecret_SecretEmpty_StaysRunning(t *testing.T) {
	g := NewWithT(t)
	node := waitTLSNode()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      noderesource.SidecarTLSSecretName(node),
			Namespace: node.Namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey: []byte(""), // present but empty
		},
	}

	exec := newWaitTLSExec(t, waitTLSCfg(t, node, sec))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning),
		"empty tls.crt is treated the same as missing — cert-manager is still issuing")
}

func TestWaitForSidecarTLSSecret_DeserializeEmptyParams(t *testing.T) {
	g := NewWithT(t)
	node := waitTLSNode()
	cfg := waitTLSCfg(t, node, nil)

	exec, err := deserializeWaitForSidecarTLSSecret("wait-empty", nil, cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exec).NotTo(BeNil())
}
