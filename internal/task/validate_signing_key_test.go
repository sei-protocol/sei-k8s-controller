package task

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// --- Fixtures ---

func validatorScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := seiv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func validatorNode(secretName string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "validator-0",
			Namespace: "default",
			UID:       "uid-validator-0",
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   "sei:v1.0.0",
			Validator: &seiv1alpha1.ValidatorSpec{
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{
						SecretName: secretName,
					},
				},
			},
		},
	}
}

// validKeyJSON returns a Tendermint priv_validator_key.json blob that
// satisfies the shape check. Cryptographic validity is not asserted.
const validKeyJSON = `{
  "address": "1B2C3D4E5F60718293A4B5C6D7E8F90112345678",
  "pub_key": {
    "type": "tendermint/PubKeyEd25519",
    "value": "qx2vCe1z4i7p+1mY3V8mO0WX1iU+Z+TQ5z6vE+VbX/Y="
  },
  "priv_key": {
    "type": "tendermint/PrivKeyEd25519",
    "value": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACrHa8J7XPiLun7WZjdXyY7RZfWJT5n5NDnPq8T5Vtf9g=="
  }
}`

func validKeySecret(name, ns string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data: map[string][]byte{
			privValidatorKeyDataKey: []byte(validKeyJSON),
		},
	}
}

func newValidateSigningKeyExec(t *testing.T, node *seiv1alpha1.SeiNode, objs ...client.Object) (TaskExecution, client.Client) {
	t.Helper()
	s := validatorScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	cfg := ExecutionConfig{
		KubeClient: c,
		Scheme:     s,
		Resource:   node,
	}
	params := ValidateSigningKeyParams{
		SecretName: node.Spec.Validator.SigningKey.Secret.SecretName,
		Namespace:  node.Namespace,
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeValidateSigningKey("validate-1", raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return exec, c
}

// signingKeyReasonFor returns the Reason of the SigningKeyReady condition,
// or "" if not set.
func signingKeyReasonFor(node *seiv1alpha1.SeiNode) string {
	for _, c := range node.Status.Conditions {
		if c.Type == seiv1alpha1.ConditionSigningKeyReady {
			return c.Reason
		}
	}
	return ""
}

func signingKeyConditionFor(node *seiv1alpha1.SeiNode) *metav1.Condition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == seiv1alpha1.ConditionSigningKeyReady {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}

// --- Happy path ---

func TestValidateSigningKey_ValidSecret_Completes(t *testing.T) {
	g := NewWithT(t)
	node := validatorNode("validator-0-key")
	exec, _ := newValidateSigningKeyExec(t, node, validKeySecret("validator-0-key", "default"))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
	g.Expect(signingKeyReasonFor(node)).To(Equal(seiv1alpha1.ReasonSigningKeyValidated))

	cond := signingKeyConditionFor(node)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// --- Transient (Secret not yet applied) ---

func TestValidateSigningKey_SecretNotFound_Transient(t *testing.T) {
	g := NewWithT(t)
	node := validatorNode("validator-0-key")
	exec, _ := newValidateSigningKeyExec(t, node) // no Secret

	err := exec.Execute(context.Background())
	g.Expect(err).ToNot(HaveOccurred()) // transient — execution returns nil, status stays Running
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(signingKeyReasonFor(node)).To(Equal(seiv1alpha1.ReasonSigningKeyNotReady))
}

func TestValidateSigningKey_SecretTerminating_Transient(t *testing.T) {
	g := NewWithT(t)
	node := validatorNode("validator-0-key")
	secret := validKeySecret("validator-0-key", "default")
	secret.Finalizers = []string{"example.com/protect"} // required for fake client to honor DeletionTimestamp
	now := metav1.Now()
	secret.DeletionTimestamp = &now
	exec, _ := newValidateSigningKeyExec(t, node, secret)

	err := exec.Execute(context.Background())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(signingKeyReasonFor(node)).To(Equal(seiv1alpha1.ReasonSigningKeyNotReady))
}

// --- Terminal (operator must fix) ---

func TestValidateSigningKey_MissingDataKey_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := validatorNode("validator-0-key")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0-key", Namespace: "default"},
		Data:       map[string][]byte{"other.json": []byte("...")},
	}
	exec, _ := newValidateSigningKeyExec(t, node, secret)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(signingKeyReasonFor(node)).To(Equal(seiv1alpha1.ReasonSigningKeyInvalid))
	g.Expect(signingKeyConditionFor(node).Message).To(ContainSubstring(privValidatorKeyDataKey))
}

func TestValidateSigningKey_EmptyDataKey_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := validatorNode("validator-0-key")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0-key", Namespace: "default"},
		Data:       map[string][]byte{privValidatorKeyDataKey: {}},
	}
	exec, _ := newValidateSigningKeyExec(t, node, secret)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(signingKeyReasonFor(node)).To(Equal(seiv1alpha1.ReasonSigningKeyInvalid))
}

func TestValidateSigningKey_MalformedJSON_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := validatorNode("validator-0-key")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0-key", Namespace: "default"},
		Data:       map[string][]byte{privValidatorKeyDataKey: []byte("{not valid json")},
	}
	exec, _ := newValidateSigningKeyExec(t, node, secret)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(signingKeyReasonFor(node)).To(Equal(seiv1alpha1.ReasonSigningKeyInvalid))
	g.Expect(signingKeyConditionFor(node).Message).To(ContainSubstring("not valid JSON"))
}

func TestValidateSigningKey_MissingAddress_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := validatorNode("validator-0-key")
	bad := strings.Replace(validKeyJSON, `"address": "1B2C3D4E5F60718293A4B5C6D7E8F90112345678"`, `"address": ""`, 1)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0-key", Namespace: "default"},
		Data:       map[string][]byte{privValidatorKeyDataKey: []byte(bad)},
	}
	exec, _ := newValidateSigningKeyExec(t, node, secret)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(signingKeyConditionFor(node).Message).To(ContainSubstring("missing address"))
}

func TestValidateSigningKey_MissingPubKeyType_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := validatorNode("validator-0-key")
	bad := strings.Replace(validKeyJSON, `"type": "tendermint/PubKeyEd25519"`, `"type": ""`, 1)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0-key", Namespace: "default"},
		Data:       map[string][]byte{privValidatorKeyDataKey: []byte(bad)},
	}
	exec, _ := newValidateSigningKeyExec(t, node, secret)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(signingKeyConditionFor(node).Message).To(ContainSubstring("pub_key"))
}

// --- Empty secretName param ---

func TestValidateSigningKey_EmptySecretName_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := validatorNode("") // SigningKey is set but secretName is empty
	exec, _ := newValidateSigningKeyExec(t, node)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
}

// --- tendermintValidatorKey serdes ---

// TestTendermintValidatorKey_FixtureRoundTrip locks the on-disk shape that
// seid emits and our pre-flight check reads. Fails loudly if a Tendermint
// version change introduces a renamed/removed field that breaks parsing.
func TestTendermintValidatorKey_FixtureRoundTrip(t *testing.T) {
	g := NewWithT(t)

	var parsed tendermintValidatorKey
	g.Expect(json.Unmarshal([]byte(validKeyJSON), &parsed)).To(Succeed())

	g.Expect(parsed.Address).To(Equal("1B2C3D4E5F60718293A4B5C6D7E8F90112345678"))
	g.Expect(parsed.PubKey.Type).To(Equal("tendermint/PubKeyEd25519"))
	g.Expect(parsed.PubKey.Value).To(Equal("qx2vCe1z4i7p+1mY3V8mO0WX1iU+Z+TQ5z6vE+VbX/Y="))
	g.Expect(parsed.PrivKey.Type).To(Equal("tendermint/PrivKeyEd25519"))
	g.Expect(parsed.PrivKey.Value).NotTo(BeEmpty())

	// Re-marshal and confirm the round-trip preserves all five required fields.
	out, err := json.Marshal(parsed)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(out)).To(ContainSubstring(`"address":"1B2C3D4E5F60718293A4B5C6D7E8F90112345678"`))
	g.Expect(string(out)).To(ContainSubstring(`"pub_key":`))
	g.Expect(string(out)).To(ContainSubstring(`"priv_key":`))
	g.Expect(string(out)).To(ContainSubstring(`"type":"tendermint/PubKeyEd25519"`))
	g.Expect(string(out)).To(ContainSubstring(`"type":"tendermint/PrivKeyEd25519"`))
}

// --- Convergence: missing-then-applied ---

func TestValidateSigningKey_MissingThenApplied_Completes(t *testing.T) {
	g := NewWithT(t)
	node := validatorNode("validator-0-key")
	exec, c := newValidateSigningKeyExec(t, node) // no Secret yet

	// First reconcile: transient.
	err := exec.Execute(context.Background())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(signingKeyReasonFor(node)).To(Equal(seiv1alpha1.ReasonSigningKeyNotReady))

	// Apply the Secret out-of-band.
	g.Expect(c.Create(context.Background(), validKeySecret("validator-0-key", "default"))).To(Succeed())

	// Second reconcile: completes.
	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
	g.Expect(signingKeyReasonFor(node)).To(Equal(seiv1alpha1.ReasonSigningKeyValidated))
}
