package node

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	tlsTestChainID = "atlantic-2"
	tlsTestImage   = "ghcr.io/sei-protocol/seid:latest"
)

// makeSelfSignedCertPEM produces a PEM-encoded ECDSA P-256 self-signed
// certificate with the given DNS SANs. Cryptographic verification is not
// the controller's concern — the preflight check only parses the cert and
// inspects DNSNames — so a self-signed leaf is sufficient test material.
func makeSelfSignedCertPEM(t *testing.T, dnsNames []string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating ECDSA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-leaf"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		DNSNames:     dnsNames,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating cert: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func tlsTestNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "tls-node", Namespace: "ns-1"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  tlsTestChainID,
			Image:    tlsTestImage,
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Sidecar: &seiv1alpha1.SidecarConfig{
				TLS: &seiv1alpha1.SidecarTLSSpec{SecretName: "tls-node-cert"},
			},
		},
	}
}

func tlsTestNodeNoTLS() *seiv1alpha1.SeiNode {
	n := tlsTestNode()
	n.Spec.Sidecar = nil
	return n
}

func tlsSecret(name, namespace string, crt, key []byte, secretType corev1.SecretType) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       secretType,
		Data: map[string][]byte{
			corev1.TLSCertKey:       crt,
			corev1.TLSPrivateKeyKey: key,
		},
	}
}

func tlsReader(t *testing.T, objs ...client.Object) client.Reader {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// --- requiredDNSNames ---

func TestRequiredDNSNames_ServiceAndPod0(t *testing.T) {
	g := NewWithT(t)
	got := requiredDNSNames(tlsTestNode())
	g.Expect(got).To(Equal([]string{
		"tls-node.ns-1.svc.cluster.local",
		"tls-node-0.tls-node.ns-1.svc.cluster.local",
	}))
}

func TestRequiredDNSNames_DifferentNamespace(t *testing.T) {
	g := NewWithT(t)
	n := tlsTestNode()
	n.Namespace = "eng-brandon"
	n.Name = "validator-0"
	got := requiredDNSNames(n)
	g.Expect(got).To(Equal([]string{
		"validator-0.eng-brandon.svc.cluster.local",
		"validator-0-0.validator-0.eng-brandon.svc.cluster.local",
	}))
}

// --- validateTLSSecret matrix ---

func TestValidateTLSSecret_Ready(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNode()
	required := requiredDNSNames(node)
	crt := makeSelfSignedCertPEM(t, required)
	sec := tlsSecret(node.Spec.Sidecar.TLS.SecretName, node.Namespace, crt, []byte("k"), corev1.SecretTypeTLS)

	reason, msg := validateTLSSecret(context.Background(), tlsReader(t, sec), node, required)
	g.Expect(reason).To(Equal(seiv1alpha1.ReasonTLSSecretReady))
	g.Expect(msg).To(ContainSubstring("well-formed"))
}

func TestValidateTLSSecret_NotFound(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNode()
	reason, msg := validateTLSSecret(context.Background(), tlsReader(t), node, requiredDNSNames(node))
	g.Expect(reason).To(Equal(seiv1alpha1.ReasonTLSSecretNotFound))
	g.Expect(msg).To(ContainSubstring("not found"))
}

func TestValidateTLSSecret_WrongType(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNode()
	sec := tlsSecret(node.Spec.Sidecar.TLS.SecretName, node.Namespace,
		[]byte("c"), []byte("k"), corev1.SecretTypeOpaque)

	reason, msg := validateTLSSecret(context.Background(), tlsReader(t, sec), node, requiredDNSNames(node))
	g.Expect(reason).To(Equal(seiv1alpha1.ReasonTLSSecretMalformed))
	g.Expect(msg).To(ContainSubstring("type"))
}

func TestValidateTLSSecret_EmptyCert(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNode()
	sec := tlsSecret(node.Spec.Sidecar.TLS.SecretName, node.Namespace,
		[]byte(""), []byte("k"), corev1.SecretTypeTLS)

	reason, msg := validateTLSSecret(context.Background(), tlsReader(t, sec), node, requiredDNSNames(node))
	g.Expect(reason).To(Equal(seiv1alpha1.ReasonTLSSecretMalformed))
	g.Expect(msg).To(ContainSubstring(corev1.TLSCertKey))
}

func TestValidateTLSSecret_EmptyKey(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNode()
	sec := tlsSecret(node.Spec.Sidecar.TLS.SecretName, node.Namespace,
		makeSelfSignedCertPEM(t, requiredDNSNames(node)), []byte(""), corev1.SecretTypeTLS)

	reason, msg := validateTLSSecret(context.Background(), tlsReader(t, sec), node, requiredDNSNames(node))
	g.Expect(reason).To(Equal(seiv1alpha1.ReasonTLSSecretMalformed))
	g.Expect(msg).To(ContainSubstring(corev1.TLSPrivateKeyKey))
}

func TestValidateTLSSecret_NotPEM(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNode()
	sec := tlsSecret(node.Spec.Sidecar.TLS.SecretName, node.Namespace,
		[]byte("not-a-pem-block"), []byte("k"), corev1.SecretTypeTLS)

	reason, msg := validateTLSSecret(context.Background(), tlsReader(t, sec), node, requiredDNSNames(node))
	g.Expect(reason).To(Equal(seiv1alpha1.ReasonTLSSecretMalformed))
	g.Expect(msg).To(ContainSubstring("PEM"))
}

func TestValidateTLSSecret_PEMButUnparseable(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNode()
	garbage := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("\x00\x01\x02\x03")})
	sec := tlsSecret(node.Spec.Sidecar.TLS.SecretName, node.Namespace,
		garbage, []byte("k"), corev1.SecretTypeTLS)

	reason, msg := validateTLSSecret(context.Background(), tlsReader(t, sec), node, requiredDNSNames(node))
	g.Expect(reason).To(Equal(seiv1alpha1.ReasonTLSSecretMalformed))
	g.Expect(msg).To(ContainSubstring("does not parse"))
}

func TestValidateTLSSecret_SANsMismatch(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNode()
	// Cert has the wrong DNS names — covers neither required SAN.
	crt := makeSelfSignedCertPEM(t, []string{"unrelated.example.com"})
	sec := tlsSecret(node.Spec.Sidecar.TLS.SecretName, node.Namespace, crt, []byte("k"), corev1.SecretTypeTLS)

	reason, msg := validateTLSSecret(context.Background(), tlsReader(t, sec), node, requiredDNSNames(node))
	g.Expect(reason).To(Equal(seiv1alpha1.ReasonTLSSecretSANsMismatch))
	g.Expect(msg).To(ContainSubstring("missing"))
}

func TestValidateTLSSecret_PartialSANsMismatch(t *testing.T) {
	// Cert covers the service SAN but not the pod-0 SAN — still a mismatch.
	g := NewWithT(t)
	node := tlsTestNode()
	required := requiredDNSNames(node)
	crt := makeSelfSignedCertPEM(t, []string{required[0]}) // only the service entry
	sec := tlsSecret(node.Spec.Sidecar.TLS.SecretName, node.Namespace, crt, []byte("k"), corev1.SecretTypeTLS)

	reason, _ := validateTLSSecret(context.Background(), tlsReader(t, sec), node, required)
	g.Expect(reason).To(Equal(seiv1alpha1.ReasonTLSSecretSANsMismatch))
}

func TestValidateTLSSecret_SupersetSANsOK(t *testing.T) {
	// Cert covers required SANs plus extras — still Ready.
	g := NewWithT(t)
	node := tlsTestNode()
	required := requiredDNSNames(node)
	crt := makeSelfSignedCertPEM(t, append([]string{"extra.example.com"}, required...))
	sec := tlsSecret(node.Spec.Sidecar.TLS.SecretName, node.Namespace, crt, []byte("k"), corev1.SecretTypeTLS)

	reason, _ := validateTLSSecret(context.Background(), tlsReader(t, sec), node, required)
	g.Expect(reason).To(Equal(seiv1alpha1.ReasonTLSSecretReady))
}

// --- reconcileSidecarTLSReady lifecycle ---

func tlsReconciler(t *testing.T, objs ...client.Object) *SeiNodeReconciler {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &SeiNodeReconciler{Client: c, APIReader: c, Scheme: s}
}

func TestReconcileSidecarTLSReady_TLSDisabled_ClearsConditionAndStatus(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNodeNoTLS()
	// Seed stale state to verify it's cleared.
	node.Status.SidecarTLS = &seiv1alpha1.SidecarTLSStatus{SecretName: "stale", RequiredDNSNames: []string{"stale.example"}}
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type: seiv1alpha1.ConditionSidecarTLSSecretReady, Status: metav1.ConditionTrue, Reason: seiv1alpha1.ReasonTLSSecretReady,
	})

	r := tlsReconciler(t)
	r.reconcileSidecarTLSReady(context.Background(), node)

	g.Expect(node.Status.SidecarTLS).To(BeNil())
	g.Expect(apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionSidecarTLSSecretReady)).To(BeNil())
}

func TestReconcileSidecarTLSReady_TLSEnabledSecretReady_SetsConditionTrue(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNode()
	required := requiredDNSNames(node)
	crt := makeSelfSignedCertPEM(t, required)
	sec := tlsSecret(node.Spec.Sidecar.TLS.SecretName, node.Namespace, crt, []byte("k"), corev1.SecretTypeTLS)

	r := tlsReconciler(t, sec)
	r.reconcileSidecarTLSReady(context.Background(), node)

	g.Expect(node.Status.SidecarTLS).NotTo(BeNil())
	g.Expect(node.Status.SidecarTLS.SecretName).To(Equal(node.Spec.Sidecar.TLS.SecretName))
	g.Expect(node.Status.SidecarTLS.RequiredDNSNames).To(Equal(required))

	cond := apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionSidecarTLSSecretReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonTLSSecretReady))
}

func TestReconcileSidecarTLSReady_TLSEnabledSecretMissing_SetsConditionFalse(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNode()

	r := tlsReconciler(t) // no Secret seeded
	r.reconcileSidecarTLSReady(context.Background(), node)

	g.Expect(node.Status.SidecarTLS).NotTo(BeNil(), "status must still publish the required-SANs contract")
	cond := apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionSidecarTLSSecretReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonTLSSecretNotFound))
}

func TestReconcileSidecarTLSReady_SANsMismatch_SetsConditionFalse(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNode()
	crt := makeSelfSignedCertPEM(t, []string{"wrong.example.com"})
	sec := tlsSecret(node.Spec.Sidecar.TLS.SecretName, node.Namespace, crt, []byte("k"), corev1.SecretTypeTLS)

	r := tlsReconciler(t, sec)
	r.reconcileSidecarTLSReady(context.Background(), node)

	cond := apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionSidecarTLSSecretReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonTLSSecretSANsMismatch))
}

func TestReconcileSidecarTLSReady_TransitionEnabledToDisabled_Cleans(t *testing.T) {
	g := NewWithT(t)
	node := tlsTestNode()
	required := requiredDNSNames(node)
	crt := makeSelfSignedCertPEM(t, required)
	sec := tlsSecret(node.Spec.Sidecar.TLS.SecretName, node.Namespace, crt, []byte("k"), corev1.SecretTypeTLS)

	r := tlsReconciler(t, sec)
	r.reconcileSidecarTLSReady(context.Background(), node)
	g.Expect(node.Status.SidecarTLS).NotTo(BeNil())

	// CEL blocks the in-place mutation in prod; this exercises the
	// defensive cleanup branch.
	node.Spec.Sidecar = nil
	r.reconcileSidecarTLSReady(context.Background(), node)

	g.Expect(node.Status.SidecarTLS).To(BeNil())
	g.Expect(apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionSidecarTLSSecretReady)).To(BeNil())
}
