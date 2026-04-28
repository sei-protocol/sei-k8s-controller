package task

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// validNodeKeyJSON is the on-disk shape `seid init` writes for node_key.json.
const validNodeKeyJSON = `{
  "priv_key": {
    "type": "tendermint/PrivKeyEd25519",
    "value": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACrHa8J7XPiLun7WZjdXyY7RZfWJT5n5NDnPq8T5Vtf9g=="
  }
}`

func validatorNodeWithNodeKey(secretName string) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "validator-0",
			Namespace: "default",
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "atlantic-2",
			Image:   "sei:v1.0.0",
			Validator: &seiv1alpha1.ValidatorSpec{
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: secretName},
				},
			},
		},
	}
}

func validNodeKeySecret(name, ns string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data: map[string][]byte{
			nodeKeyDataKey: []byte(validNodeKeyJSON),
		},
	}
}

func newValidateNodeKeyExec(t *testing.T, node *seiv1alpha1.SeiNode, objs ...client.Object) (TaskExecution, client.Client) {
	t.Helper()
	s := validatorScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	cfg := ExecutionConfig{
		KubeClient: c,
		Scheme:     s,
		Resource:   node,
	}
	params := ValidateNodeKeyParams{
		SecretName: node.Spec.Validator.NodeKey.Secret.SecretName,
		Namespace:  node.Namespace,
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeValidateNodeKey("validate-1", raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return exec, c
}

func nodeKeyReasonFor(node *seiv1alpha1.SeiNode) string {
	for _, c := range node.Status.Conditions {
		if c.Type == seiv1alpha1.ConditionNodeKeyReady {
			return c.Reason
		}
	}
	return ""
}

func nodeKeyConditionFor(node *seiv1alpha1.SeiNode) *metav1.Condition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == seiv1alpha1.ConditionNodeKeyReady {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}

// --- tendermintNodeKey serdes ---

// TestTendermintNodeKey_FixtureRoundTrip locks the on-disk shape that seid
// emits for node_key.json. Fails loudly if a Tendermint version change
// renames or removes the priv_key field.
func TestTendermintNodeKey_FixtureRoundTrip(t *testing.T) {
	g := NewWithT(t)

	var parsed tendermintNodeKey
	g.Expect(json.Unmarshal([]byte(validNodeKeyJSON), &parsed)).To(Succeed())
	g.Expect(parsed.PrivKey.Type).To(Equal("tendermint/PrivKeyEd25519"))
	g.Expect(parsed.PrivKey.Value).NotTo(BeEmpty())

	out, err := json.Marshal(parsed)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(out)).To(ContainSubstring(`"priv_key":`))
	g.Expect(string(out)).To(ContainSubstring(`"type":"tendermint/PrivKeyEd25519"`))
}

// --- Happy path ---

func TestValidateNodeKey_ValidSecret_Completes(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithNodeKey("validator-0-nodekey")
	exec, _ := newValidateNodeKeyExec(t, node, validNodeKeySecret("validator-0-nodekey", "default"))

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
	g.Expect(nodeKeyReasonFor(node)).To(Equal(seiv1alpha1.ReasonNodeKeyValidated))

	cond := nodeKeyConditionFor(node)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// --- Transient ---

func TestValidateNodeKey_SecretNotFound_Transient(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithNodeKey("validator-0-nodekey")
	exec, _ := newValidateNodeKeyExec(t, node)

	g.Expect(exec.Execute(context.Background())).ToNot(HaveOccurred())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(nodeKeyReasonFor(node)).To(Equal(seiv1alpha1.ReasonNodeKeyNotReady))
}

// --- Terminal ---

func TestValidateNodeKey_MissingDataKey_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithNodeKey("validator-0-nodekey")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0-nodekey", Namespace: "default"},
		Data:       map[string][]byte{"other.json": []byte("...")},
	}
	exec, _ := newValidateNodeKeyExec(t, node, secret)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(nodeKeyReasonFor(node)).To(Equal(seiv1alpha1.ReasonNodeKeyInvalid))
	g.Expect(nodeKeyConditionFor(node).Message).To(ContainSubstring(nodeKeyDataKey))
}

func TestValidateNodeKey_MalformedJSON_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithNodeKey("validator-0-nodekey")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0-nodekey", Namespace: "default"},
		Data:       map[string][]byte{nodeKeyDataKey: []byte("{not json")},
	}
	exec, _ := newValidateNodeKeyExec(t, node, secret)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(nodeKeyConditionFor(node).Message).To(ContainSubstring("not valid JSON"))
}

func TestValidateNodeKey_MissingPrivKey_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithNodeKey("validator-0-nodekey")
	bad := `{"priv_key":{"type":"","value":""}}`
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-0-nodekey", Namespace: "default"},
		Data:       map[string][]byte{nodeKeyDataKey: []byte(bad)},
	}
	exec, _ := newValidateNodeKeyExec(t, node, secret)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(nodeKeyConditionFor(node).Message).To(ContainSubstring("priv_key"))
}

// --- Convergence ---

func TestValidateNodeKey_MissingThenApplied_Completes(t *testing.T) {
	g := NewWithT(t)
	node := validatorNodeWithNodeKey("validator-0-nodekey")
	exec, c := newValidateNodeKeyExec(t, node)

	g.Expect(exec.Execute(context.Background())).ToNot(HaveOccurred())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))

	g.Expect(c.Create(context.Background(), validNodeKeySecret("validator-0-nodekey", "default"))).To(Succeed())

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
	g.Expect(nodeKeyReasonFor(node)).To(Equal(seiv1alpha1.ReasonNodeKeyValidated))
}
