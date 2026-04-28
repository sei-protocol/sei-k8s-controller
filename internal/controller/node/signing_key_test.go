package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// signingKeyTestNode returns a validator SeiNode wired with a Secret-backed
// SigningKey. Mode is bare validator (no bootstrap, no genesis ceremony) so
// the plan path under test is buildBasePlan + the validate-signing-key
// insertion in §7 of the LLD.
func signingKeyTestNode(name, namespace, secretName string) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "sei-test",
			Image:   "ghcr.io/sei-protocol/seid:latest",
			Validator: &seiv1alpha1.ValidatorSpec{
				SigningKey: &seiv1alpha1.SigningKeySource{
					Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: secretName},
				},
				NodeKey: &seiv1alpha1.NodeKeySource{
					Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: secretName + "-nodekey"},
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Port: 7777},
		},
	}
}

// validatorKeySecret returns a Secret containing a well-formed Tendermint
// validator key. Cryptographic validity is not asserted — the controller's
// pre-flight check verifies shape only.
func validatorKeySecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data: map[string][]byte{
			"priv_validator_key.json": []byte(`{
  "address": "1B2C3D4E5F60718293A4B5C6D7E8F90112345678",
  "pub_key": {"type": "tendermint/PubKeyEd25519", "value": "qx2vCe1z4i7p+1mY3V8mO0WX1iU+Z+TQ5z6vE+VbX/Y="},
  "priv_key": {"type": "tendermint/PrivKeyEd25519", "value": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACrHa8J7XPiLun7WZjdXyY7RZfWJT5n5NDnPq8T5Vtf9g=="}
}`),
		},
	}
}

func TestController_SigningKey_ValidSecret_ConditionTrue(t *testing.T) {
	g := NewWithT(t)

	node := signingKeyTestNode("validator-0", "default", "validator-0-key")
	secret := validatorKeySecret("validator-0-key", "default")

	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node, secret)

	fetch := func() *seiv1alpha1.SeiNode {
		return fetchNode(t, c, node.Name, node.Namespace)
	}

	// Reconcile 1: finalizer + build plan.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	n := fetch()
	g.Expect(n.Status.Plan).NotTo(BeNil())
	g.Expect(findPlannedTask(n.Status.Plan, "validate-signing-key")).NotTo(BeNil(),
		"plan must include validate-signing-key when SigningKey is set")

	// Reconcile 2: ensure-data-pvc + validate-signing-key both run inline.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	n = fetch()
	cond := meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionSigningKeyReady)
	g.Expect(cond).NotTo(BeNil(), "SigningKeyReady condition should be set after validation")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonSigningKeyValidated))
}

func TestController_SigningKey_MissingSecret_StuckTransient_ThenConverges(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := signingKeyTestNode("validator-late", "default", "late-key")

	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node)

	fetch := func() *seiv1alpha1.SeiNode {
		return fetchNode(t, c, node.Name, node.Namespace)
	}

	// Reconcile 1+2: plan is built; first validation pass sees no Secret →
	// transient, condition goes False/SigningKeyNotReady.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	reconcileOnce(t, g, r, node.Name, node.Namespace)

	n := fetch()
	g.Expect(n.Status.Phase).To(Equal(seiv1alpha1.PhaseInitializing))
	cond := meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionSigningKeyReady)
	g.Expect(cond).NotTo(BeNil(), "SigningKeyReady should be set while stuck")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonSigningKeyNotReady))

	// Operator creates the Secret out-of-band.
	g.Expect(c.Create(ctx, validatorKeySecret("late-key", "default"))).To(Succeed())

	// Next reconcile: validation passes, task completes, condition flips.
	reconcileOnce(t, g, r, node.Name, node.Namespace)

	n = fetch()
	cond = meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionSigningKeyReady)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonSigningKeyValidated))
}

func TestController_SigningKey_MalformedSecret_PlanFailed(t *testing.T) {
	g := NewWithT(t)

	node := signingKeyTestNode("validator-bad", "default", "bad-key")
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-key", Namespace: "default"},
		Data: map[string][]byte{
			"priv_validator_key.json": []byte("{not valid json"),
		},
	}

	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node, secret)

	fetch := func() *seiv1alpha1.SeiNode {
		return fetchNode(t, c, node.Name, node.Namespace)
	}

	// Reconcile 1: build plan. Reconcile 2: ensure-data-pvc + validate-signing-key
	// fail terminally → plan Failed → phase Failed.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	reconcileOnce(t, g, r, node.Name, node.Namespace)

	n := fetch()
	g.Expect(n.Status.Phase).To(Equal(seiv1alpha1.PhaseFailed))
	g.Expect(n.Status.Plan).NotTo(BeNil())
	g.Expect(n.Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))

	cond := meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionSigningKeyReady)
	g.Expect(cond).NotTo(BeNil(), "condition should reflect terminal validation failure")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonSigningKeyInvalid))
}

func TestController_SigningKey_Deletion_PreservesSecret(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := signingKeyTestNode("validator-del", "default", "preserve-key")
	node.Finalizers = []string{nodeFinalizerName}
	secret := validatorKeySecret("preserve-key", "default")

	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node, secret)

	g.Expect(c.Delete(ctx, node)).To(Succeed())
	_ = c.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, node)

	_, err := r.Reconcile(ctx, nodeReqFor(node.Name, node.Namespace))
	g.Expect(err).NotTo(HaveOccurred())

	// Secret must still exist — the controller never touches Secrets it
	// references via SigningKey.
	remaining := &corev1.Secret{}
	err = c.Get(ctx, types.NamespacedName{Name: "preserve-key", Namespace: "default"}, remaining)
	g.Expect(err).NotTo(HaveOccurred(), "referenced Secret must be preserved across SeiNode deletion")
}
