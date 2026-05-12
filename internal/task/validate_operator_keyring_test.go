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

const (
	opkNs             = "default"
	opkValidatorName  = "validator-0"
	opkChainID        = "atlantic-2"
	opkImage          = "sei:v1.0.0"
	opkPassphraseKey  = "passphrase"
	opkAddressDataKey = "deadbeef.address"
	opkKeyringSecret  = "opk-data"
	opkPassphrSecret  = "opk-passphrase"
	opkDefaultKeyName = "node_admin"
)

func operatorKeyringNode(keyringSecret, passphraseSecret, keyName string) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opkValidatorName,
			Namespace: opkNs,
			UID:       "uid-validator-0",
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: opkChainID,
			Image:   opkImage,
			Validator: &seiv1alpha1.ValidatorSpec{
				OperatorKeyring: &seiv1alpha1.OperatorKeyringSource{
					Secret: &seiv1alpha1.SecretOperatorKeyringSource{
						SecretName: keyringSecret,
						KeyName:    keyName,
						PassphraseSecretRef: seiv1alpha1.PassphraseSecretRef{
							SecretName: passphraseSecret,
							Key:        opkPassphraseKey,
						},
					},
				},
			},
		},
	}
}

func validKeyringSecret(name, ns, keyName string) *corev1.Secret { //nolint:unparam // test helper designed for reuse
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data: map[string][]byte{
			keyName + ".info": []byte("encrypted-key-blob"),
			opkAddressDataKey: []byte("addr-index"),
		},
	}
}

func validPassphraseSecret(name, ns string) *corev1.Secret { //nolint:unparam // test helper designed for reuse
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string][]byte{opkPassphraseKey: []byte("hunter2")},
	}
}

func newValidateOperatorKeyringExec(t *testing.T, node *seiv1alpha1.SeiNode, objs ...client.Object) (TaskExecution, client.Client) {
	t.Helper()
	s := validatorScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	cfg := ExecutionConfig{
		KubeClient: c,
		APIReader:  c,
		Scheme:     s,
		Resource:   node,
	}
	src := node.Spec.Validator.OperatorKeyring.Secret
	params := ValidateOperatorKeyringParams{
		SecretName:           src.SecretName,
		KeyName:              src.KeyName,
		PassphraseSecretName: src.PassphraseSecretRef.SecretName,
		PassphraseSecretKey:  src.PassphraseSecretRef.Key,
		Namespace:            node.Namespace,
	}
	raw, _ := json.Marshal(params)
	exec, err := deserializeValidateOperatorKeyring("validate-op-1", raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return exec, c
}

func operatorKeyringReasonFor(node *seiv1alpha1.SeiNode) string {
	for _, c := range node.Status.Conditions {
		if c.Type == seiv1alpha1.ConditionOperatorKeyringReady {
			return c.Reason
		}
	}
	return ""
}

func operatorKeyringConditionFor(node *seiv1alpha1.SeiNode) *metav1.Condition {
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == seiv1alpha1.ConditionOperatorKeyringReady {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}

// --- Happy path ---

func TestValidateOperatorKeyring_Valid_Completes(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode(opkKeyringSecret, opkPassphrSecret, opkDefaultKeyName)
	exec, _ := newValidateOperatorKeyringExec(t, node,
		validKeyringSecret(opkKeyringSecret, opkNs, opkDefaultKeyName),
		validPassphraseSecret(opkPassphrSecret, opkNs),
	)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionComplete))
	g.Expect(operatorKeyringReasonFor(node)).To(Equal(seiv1alpha1.ReasonOperatorKeyringValidated))

	cond := operatorKeyringConditionFor(node)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// --- Transient (Secret not yet applied) ---

func TestValidateOperatorKeyring_KeyringSecretNotFound_Transient(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode(opkKeyringSecret, opkPassphrSecret, opkDefaultKeyName)
	exec, _ := newValidateOperatorKeyringExec(t, node,
		validPassphraseSecret(opkPassphrSecret, opkNs),
	)

	err := exec.Execute(context.Background())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(operatorKeyringReasonFor(node)).To(Equal(seiv1alpha1.ReasonOperatorKeyringNotReady))
}

func TestValidateOperatorKeyring_PassphraseSecretNotFound_Transient(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode(opkKeyringSecret, opkPassphrSecret, opkDefaultKeyName)
	exec, _ := newValidateOperatorKeyringExec(t, node,
		validKeyringSecret(opkKeyringSecret, opkNs, opkDefaultKeyName),
	)

	err := exec.Execute(context.Background())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(operatorKeyringReasonFor(node)).To(Equal(seiv1alpha1.ReasonOperatorKeyringNotReady))
}

func TestValidateOperatorKeyring_SecretTerminating_Transient(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode(opkKeyringSecret, opkPassphrSecret, opkDefaultKeyName)
	keyring := validKeyringSecret(opkKeyringSecret, opkNs, opkDefaultKeyName)
	keyring.Finalizers = []string{"example.com/protect"}
	now := metav1.Now()
	keyring.DeletionTimestamp = &now
	exec, _ := newValidateOperatorKeyringExec(t, node, keyring,
		validPassphraseSecret(opkPassphrSecret, opkNs),
	)

	err := exec.Execute(context.Background())
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(operatorKeyringReasonFor(node)).To(Equal(seiv1alpha1.ReasonOperatorKeyringNotReady))
}

// --- Terminal (operator must fix) ---

func TestValidateOperatorKeyring_NoInfoKeys_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode(opkKeyringSecret, opkPassphrSecret, opkDefaultKeyName)
	keyring := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: opkKeyringSecret, Namespace: opkNs},
		Data:       map[string][]byte{opkAddressDataKey: []byte("addr")},
	}
	exec, _ := newValidateOperatorKeyringExec(t, node, keyring,
		validPassphraseSecret(opkPassphrSecret, opkNs),
	)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(operatorKeyringReasonFor(node)).To(Equal(seiv1alpha1.ReasonOperatorKeyringInvalid))
	g.Expect(operatorKeyringConditionFor(node).Message).To(ContainSubstring(".info"))
}

func TestValidateOperatorKeyring_NoAddressKeys_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode(opkKeyringSecret, opkPassphrSecret, opkDefaultKeyName)
	keyring := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: opkKeyringSecret, Namespace: opkNs},
		Data:       map[string][]byte{"node_admin.info": []byte("blob")},
	}
	exec, _ := newValidateOperatorKeyringExec(t, node, keyring,
		validPassphraseSecret(opkPassphrSecret, opkNs),
	)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(operatorKeyringConditionFor(node).Message).To(ContainSubstring(".address"))
}

func TestValidateOperatorKeyring_EmptyInfoBlob_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode(opkKeyringSecret, opkPassphrSecret, opkDefaultKeyName)
	keyring := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: opkKeyringSecret, Namespace: opkNs},
		Data: map[string][]byte{
			"node_admin.info": {},
			opkAddressDataKey: []byte("addr"),
		},
	}
	exec, _ := newValidateOperatorKeyringExec(t, node, keyring,
		validPassphraseSecret(opkPassphrSecret, opkNs),
	)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(operatorKeyringConditionFor(node).Message).To(ContainSubstring("empty"))
}

func TestValidateOperatorKeyring_NamedKeyMissing_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode(opkKeyringSecret, opkPassphrSecret, opkDefaultKeyName)
	keyring := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: opkKeyringSecret, Namespace: opkNs},
		Data: map[string][]byte{
			"someone_else.info": []byte("blob"),
			opkAddressDataKey:   []byte("addr"),
		},
	}
	exec, _ := newValidateOperatorKeyringExec(t, node, keyring,
		validPassphraseSecret(opkPassphrSecret, opkNs),
	)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(operatorKeyringConditionFor(node).Message).To(ContainSubstring("node_admin.info"))
}

func TestValidateOperatorKeyring_PassphraseKeyMissing_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode(opkKeyringSecret, opkPassphrSecret, opkDefaultKeyName)
	passphrase := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: opkPassphrSecret, Namespace: opkNs},
		Data:       map[string][]byte{"wrong-key": []byte("hunter2")},
	}
	exec, _ := newValidateOperatorKeyringExec(t, node,
		validKeyringSecret(opkKeyringSecret, opkNs, opkDefaultKeyName),
		passphrase,
	)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(operatorKeyringConditionFor(node).Message).To(ContainSubstring("passphrase"))
}

func TestValidateOperatorKeyring_PassphraseEmpty_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode(opkKeyringSecret, opkPassphrSecret, opkDefaultKeyName)
	passphrase := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: opkPassphrSecret, Namespace: opkNs},
		Data:       map[string][]byte{opkPassphraseKey: {}},
	}
	exec, _ := newValidateOperatorKeyringExec(t, node,
		validKeyringSecret(opkKeyringSecret, opkNs, opkDefaultKeyName),
		passphrase,
	)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
	g.Expect(operatorKeyringConditionFor(node).Message).To(ContainSubstring("empty"))
}

// --- Empty params ---

func TestValidateOperatorKeyring_EmptySecretName_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode("", opkPassphrSecret, opkDefaultKeyName)
	exec, _ := newValidateOperatorKeyringExec(t, node)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
}

func TestValidateOperatorKeyring_EmptyPassphraseSecretName_Terminal(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode(opkKeyringSecret, "", opkDefaultKeyName)
	exec, _ := newValidateOperatorKeyringExec(t, node)

	err := exec.Execute(context.Background())
	g.Expect(err).To(HaveOccurred())
	g.Expect(isTerminal(err)).To(BeTrue())
}

// --- Convergence: missing-then-applied ---

func TestValidateOperatorKeyring_MissingThenApplied_Completes(t *testing.T) {
	g := NewWithT(t)
	node := operatorKeyringNode(opkKeyringSecret, opkPassphrSecret, opkDefaultKeyName)
	exec, c := newValidateOperatorKeyringExec(t, node)

	g.Expect(exec.Execute(context.Background())).To(Succeed())
	g.Expect(exec.Status(context.Background())).To(Equal(ExecutionRunning))
	g.Expect(operatorKeyringReasonFor(node)).To(Equal(seiv1alpha1.ReasonOperatorKeyringNotReady))

	ctx := context.Background()
	g.Expect(c.Create(ctx, validKeyringSecret(opkKeyringSecret, opkNs, opkDefaultKeyName))).To(Succeed())
	g.Expect(c.Create(ctx, validPassphraseSecret(opkPassphrSecret, opkNs))).To(Succeed())

	g.Expect(exec.Execute(ctx)).To(Succeed())
	g.Expect(exec.Status(ctx)).To(Equal(ExecutionComplete))
	g.Expect(operatorKeyringReasonFor(node)).To(Equal(seiv1alpha1.ReasonOperatorKeyringValidated))
}
