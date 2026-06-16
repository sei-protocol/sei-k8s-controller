//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"

	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// withSigningAndNodeKey attaches a signingKey + nodeKey to the validator
// template, referencing distinct Secrets. Distinct names satisfy the
// signingKey/nodeKey-paired CEL rules so a test can isolate the replicas==1
// rule.
func withSigningAndNodeKey(network *seiv1alpha1.SeiNetwork) {
	network.Spec.Template.Spec.Validator.SigningKey = &seiv1alpha1.SigningKeySource{
		Secret: &seiv1alpha1.SecretSigningKeySource{SecretName: "test-signing-key"},
	}
	network.Spec.Template.Spec.Validator.NodeKey = &seiv1alpha1.NodeKeySource{
		Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: "test-node-key"},
	}
}

// TestValidator_SigningKeyRequiresSingleReplica asserts the spec-level CEL
// rule that rejects replicas > 1 when validator.signingKey is set. Every
// replica mounts the same priv_validator_key.json, so more than one replica
// double-signs (equivocation) and tombstones/slashes the validator.
func TestValidator_SigningKeyRequiresSingleReplica(t *testing.T) {
	t.Run("signingKey with replicas 1 is accepted", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)
		network := fixtures.NewNetwork(ns, "val-keys-r1", fixtures.WithReplicas(1))
		withSigningAndNodeKey(network)
		g.Expect(testCli.Create(testCtx, network)).To(Succeed(),
			"a signing validator with replicas:1 must be accepted")
	})

	t.Run("signingKey with replicas 2 is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)
		network := fixtures.NewNetwork(ns, "val-keys-r2", fixtures.WithReplicas(2))
		withSigningAndNodeKey(network)
		err := testCli.Create(testCtx, network)
		g.Expect(err).To(HaveOccurred(), "a signing validator with replicas:2 must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("must have replicas: 1"),
			"rejection must carry the CEL rule message; got: %s", err.Error())
	})

	t.Run("validator without a signingKey allows replicas > 1", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)
		// Genesis-ceremony validators generate per-replica identities
		// on-cluster (generate-identity), so no fixed signingKey is mounted
		// and multiple replicas are the composable-genesis N-validator fan-out.
		network := fixtures.NewNetwork(ns, "val-nokeys-r3", fixtures.WithReplicas(3))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed(),
			"a validator without a signingKey must allow replicas > 1")
	})

	t.Run("scaling a signing validator above 1 is rejected on update", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)
		network := fixtures.NewNetwork(ns, "val-keys-scale", fixtures.WithReplicas(1))
		withSigningAndNodeKey(network)
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.Replicas = 2
		})
		g.Expect(err).To(HaveOccurred(), "scaling a signing validator above 1 must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("must have replicas: 1"))
	})
}
