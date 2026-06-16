//go:build envtest

package envtest_test

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// Structural-only validator key blobs — validate-signing-key /
// validate-node-key check JSON shape, not cryptographic validity.
const (
	rolloutSigningKeyJSON = `{
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
	rolloutNodeKeyJSON = `{
  "priv_key": {
    "type": "tendermint/PrivKeyEd25519",
    "value": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACrHa8J7XPiLun7WZjdXyY7RZfWJT5n5NDnPq8T5Vtf9g=="
  }
}`

	// reasonRolloutComplete is the RolloutInProgress condition reason the SND
	// controller stamps on a finished rollout (see nodedeployment/plan.go).
	reasonRolloutComplete = "RolloutComplete"
)

// TestInPlaceRollout_BYOValidator_NoOperatorKeyring drives an image bump on
// a Running validator that has signingKey+nodeKey but no operatorKeyring —
// the arctic-1 node-19 migration shape — against a real apiserver with the
// SeiNode controller, stub sidecar, and StatefulSet faker wired in.
//
// Before the buildRunningPlan guard fix, the update path emitted an
// unconditional validate-operator-keyring task whose params carried an empty
// secretName, so the child's update plan latched Failed with
// "validate-operator-keyring: secretName is empty" and the new image never
// propagated — this test would time out at the second waitFor. With the fix,
// the unset operatorKeyring is skipped and the rollout completes.
func TestInPlaceRollout_BYOValidator_NoOperatorKeyring(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	const (
		oldImage      = "ghcr.io/sei-protocol/seid:v1.0.0"
		newImage      = "ghcr.io/sei-protocol/seid:v2.0.0"
		signingSecret = "byo-signing-key"
		nodeSecret    = "byo-node-key"
	)

	// Seed the BYO key Secrets so the validate-signing-key /
	// validate-node-key gates pass on both the create and update plans.
	g.Expect(testCli.Create(testCtx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: signingSecret, Namespace: ns},
		Data:       map[string][]byte{"priv_validator_key.json": []byte(rolloutSigningKeyJSON)},
	})).To(Succeed())
	g.Expect(testCli.Create(testCtx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: nodeSecret, Namespace: ns},
		Data:       map[string][]byte{"node_key.json": []byte(rolloutNodeKeyJSON)},
	})).To(Succeed())

	// Validator with signingKey+nodeKey and NO operatorKeyring. replicas:1
	// satisfies the signingKey CEL guard.
	snd := fixtures.NewNetwork(ns, "byo-val-rollout",
		fixtures.WithReplicas(1),
		fixtures.WithImage(oldImage),
	)
	snd.Spec.Template.Spec.Validator.SigningKey = &seiv1alpha1.SigningKeySource{
		Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: signingSecret},
	}
	snd.Spec.Template.Spec.Validator.NodeKey = &seiv1alpha1.NodeKeySource{
		Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: nodeSecret},
	}
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())
	key := client.ObjectKeyFromObject(snd)

	// 1. The create plan completes: the single child reaches Running at the
	//    old image. The child MUST be Running before the bump so the bump
	//    routes through buildRunningPlan (the fixed path) rather than the
	//    create plan.
	waitFor(t, func() bool {
		kids := listChildren(t, getNetwork(t, key))
		if len(kids) != 1 {
			return false
		}
		return kids[0].Status.Phase == seiv1alpha1.PhaseRunning &&
			kids[0].Status.CurrentImage == oldImage
	}, "BYO validator child reaches Running at the old image")

	// 2. Bump the image — drives the running-plan update.
	patchNetworkImage(t, getNetwork(t, key), newImage)

	// 3. The update plan completes and the new image propagates. Pre-fix
	//    this never happened: the plan wedged on the operator-keyring gate.
	waitFor(t, func() bool {
		kids := listChildren(t, getNetwork(t, key))
		if len(kids) != 1 {
			return false
		}
		return kids[0].Status.CurrentImage == newImage
	}, "BYO validator rolls to the new image via the running-plan update")

	// 4. Regression-specific: the child's update plan never failed on the
	//    operator-keyring gate (the precise pre-fix failure mode).
	for _, kid := range listChildren(t, getNetwork(t, key)) {
		if kid.Status.Plan != nil && kid.Status.Plan.FailedTaskDetail != nil {
			g.Expect(kid.Status.Plan.FailedTaskDetail.Error).NotTo(
				ContainSubstring("operator-keyring"),
				"child %s must not fail its update plan on the operator-keyring gate", kid.Name)
		}
	}
	for _, kid := range listChildren(t, getNetwork(t, key)) {
		if kid.Status.Plan == nil {
			continue
		}
		for _, task := range kid.Status.Plan.Tasks {
			g.Expect(strings.Contains(task.Type, "operator-keyring")).To(BeFalse(),
				"child %s plan must not include an operator-keyring task when operatorKeyring is unset", kid.Name)
		}
	}

	// 5. The SND-side rollout completes cleanly.
	waitForStatus(t, key, func(latest *seiv1alpha1.SeiNetwork) bool {
		cond := apimeta.FindStatusCondition(latest.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
		return latest.Status.Plan == nil &&
			latest.Status.Rollout == nil &&
			cond != nil &&
			cond.Status == metav1.ConditionFalse &&
			cond.Reason == reasonRolloutComplete
	}, "SND rollout completes for the BYO validator (no operatorKeyring)")
}
