package task

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// waitTLSCfg builds an ExecutionConfig with an optional Secret and
// optional cert-manager Certificate (unstructured) seeded into the
// fake client.
func waitTLSCfg(t *testing.T, node *seiv1alpha1.SeiNode, sec *corev1.Secret, cert *unstructured.Unstructured) ExecutionConfig {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	builder := fake.NewClientBuilder().WithScheme(s)
	objs := []client.Object{}
	if sec != nil {
		objs = append(objs, sec)
	}
	if cert != nil {
		objs = append(objs, cert)
	}
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
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

// certManagerCertificate constructs an unstructured cert-manager
// Certificate with the given Ready condition.
func certManagerCertificate(name, namespace, status, reason, message string) *unstructured.Unstructured {
	cert := &unstructured.Unstructured{}
	cert.SetAPIVersion("cert-manager.io/v1")
	cert.SetKind("Certificate")
	cert.SetName(name)
	cert.SetNamespace(namespace)
	_ = unstructured.SetNestedSlice(cert.Object, []any{
		map[string]any{
			"type":    "Ready",
			"status":  status,
			"reason":  reason,
			"message": message,
		},
	}, "status", "conditions")
	return cert
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

	exec := newWaitTLSExec(t, waitTLSCfg(t, node, sec, nil))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
}

func TestWaitForSidecarTLSSecret_SecretMissing_StaysRunning(t *testing.T) {
	g := NewWithT(t)
	node := waitTLSNode()

	exec := newWaitTLSExec(t, waitTLSCfg(t, node, nil, nil))

	// cert-manager hasn't issued yet — Secret absent, Certificate absent.
	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
}

func TestWaitForSidecarTLSSecret_TLSCertEmpty_StaysRunning(t *testing.T) {
	g := NewWithT(t)
	node := waitTLSNode()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      noderesource.SidecarTLSSecretName(node),
			Namespace: node.Namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       []byte(""),
			corev1.TLSPrivateKeyKey: []byte("k"),
		},
	}

	exec := newWaitTLSExec(t, waitTLSCfg(t, node, sec, nil))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
}

func TestWaitForSidecarTLSSecret_TLSKeyEmpty_StaysRunning(t *testing.T) {
	// Defensive: a Secret with tls.crt but no tls.key would crash-loop
	// the proxy at boot. Both must be present.
	g := NewWithT(t)
	node := waitTLSNode()
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      noderesource.SidecarTLSSecretName(node),
			Namespace: node.Namespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey: []byte("-----BEGIN CERTIFICATE-----\n..."),
		},
	}

	exec := newWaitTLSExec(t, waitTLSCfg(t, node, sec, nil))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
}

func TestWaitForSidecarTLSSecret_CertificateFailed_ReturnsTerminal(t *testing.T) {
	g := NewWithT(t)
	node := waitTLSNode()
	cert := certManagerCertificate(
		noderesource.SidecarTLSSecretName(node),
		node.Namespace,
		"False", "Failed",
		"referenced ClusterIssuer not found",
	)

	exec := newWaitTLSExec(t, waitTLSCfg(t, node, nil, cert))

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	var te *TerminalError
	g.Expect(errors.As(err, &te)).To(BeTrue(), "must be a TerminalError so the plan fails fast")
	g.Expect(err.Error()).To(ContainSubstring("cert-manager Certificate"))
	g.Expect(err.Error()).To(ContainSubstring("referenced ClusterIssuer not found"))
}

func TestWaitForSidecarTLSSecret_CertificateIssuing_StaysRunning(t *testing.T) {
	// Transient False reasons (Issuing, etc.) must NOT terminal the plan.
	g := NewWithT(t)
	node := waitTLSNode()
	cert := certManagerCertificate(
		noderesource.SidecarTLSSecretName(node),
		node.Namespace,
		"False", "Issuing",
		"issuance in progress",
	)

	exec := newWaitTLSExec(t, waitTLSCfg(t, node, nil, cert))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
}

func TestWaitForSidecarTLSSecret_DeserializeEmptyParams(t *testing.T) {
	g := NewWithT(t)
	node := waitTLSNode()
	cfg := waitTLSCfg(t, node, nil, nil)

	exec, err := deserializeWaitForSidecarTLSSecret("wait-empty", nil, cfg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(exec).NotTo(BeNil())
}
