//go:build envtest

package envtest_test

import (
	"strconv"
	"testing"

	. "github.com/onsi/gomega"

	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// TestValidator_GeneratedIdentityMultiReplica asserts that a genesis validator
// pool with replicas > 1 is accepted and fans out distinct per-replica
// children. There is no signingKey field on a SeiNetwork — every replica's
// consensus/P2P/operator identity is ceremony-generated on-cluster
// (generate-identity), so multiple replicas carry distinct keys and replicas>1
// is the normal safe case (no shared priv_validator_key.json → no
// double-sign). This replaces the SND-era "signingKey ⇒ replicas==1" CEL rule,
// which is moot by construction.
func TestValidator_GeneratedIdentityMultiReplica(t *testing.T) {
	t.Run("replicas 1 is accepted", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)
		network := fixtures.NewNetwork(ns, "gen-r1", fixtures.WithReplicas(1))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	})

	t.Run("replicas 3 is accepted and fans out 3 distinct children", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)
		network := fixtures.NewNetwork(ns, "gen-r3", fixtures.WithReplicas(3))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())
		key := client.ObjectKeyFromObject(network)

		waitFor(t, func() bool {
			kids := listChildren(t, getNetwork(t, key))
			if len(kids) != 3 {
				return false
			}
			// Each child is a genesis-ceremony validator carrying a distinct
			// ordinal (Index) and NO bring-your-own key material.
			seen := map[int32]bool{}
			for i := range kids {
				v := kids[i].Spec.Validator
				if v == nil || v.GenesisCeremony == nil {
					return false
				}
				if v.SigningKey != nil || v.NodeKey != nil || v.OperatorKeyring != nil {
					return false
				}
				seen[v.GenesisCeremony.Index] = true
				if kids[i].Labels["sei.io/nodedeployment-ordinal"] != strconv.Itoa(int(v.GenesisCeremony.Index)) {
					return false
				}
			}
			return len(seen) == 3
		}, "3 children with distinct ceremony ordinals and no BYO keys")
	})

	t.Run("scaling up a generated-identity pool is accepted", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)
		network := fixtures.NewNetwork(ns, "gen-scale", fixtures.WithReplicas(1))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.Replicas = 3
		})
		g.Expect(err).NotTo(HaveOccurred(),
			"scaling a generated-identity validator pool above 1 must be accepted")
	})
}
