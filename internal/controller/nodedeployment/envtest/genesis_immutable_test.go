//go:build envtest

package envtest_test

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"

	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest/fixtures"
)

// TestGenesis_ImmutabilityGate asserts the CRD-level CEL rule rejects
// post-creation mutation of spec.genesis. The ceremony's outputs (chain
// ID, validator gentxs, account balances) are baked into chain state at
// bootstrap; later spec edits would silently diverge from on-chain truth.
// The gate also keeps the GenesisCeremonyComplete condition's latched-
// True semantics honest — clearing spec.genesis mid-flight would let a
// completed ceremony be downgraded to False/NotApplicable.
//
// The test runs without the controller reconciling because CEL validation
// fires at admission time before any controller sees the object.
func TestGenesis_ImmutabilityGate(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "genesis-immutable",
		fixtures.WithValidator(),
	)
	snd.Spec.Genesis = &seiv1alpha1.GenesisCeremonyConfig{
		ChainID:        "pacific-1",
		StakingAmount:  "10000000usei",
		AccountBalance: "1000000usei",
	}
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed(), "creating SND with genesis must succeed")

	t.Run("clearing spec.genesis is rejected", func(t *testing.T) {
		g := NewWithT(t)
		cur := &seiv1alpha1.SeiNodeDeployment{}
		g.Expect(testCli.Get(testCtx, client.ObjectKeyFromObject(snd), cur)).To(Succeed())
		cur.Spec.Genesis = nil
		err := testCli.Update(testCtx, cur)
		g.Expect(err).To(HaveOccurred(), "clearing spec.genesis must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.genesis is immutable"),
			"rejection must carry the CEL rule's immutability message; got: %s", err.Error())
	})

	t.Run("mutating chainId is rejected", func(t *testing.T) {
		g := NewWithT(t)
		cur := &seiv1alpha1.SeiNodeDeployment{}
		g.Expect(testCli.Get(testCtx, client.ObjectKeyFromObject(snd), cur)).To(Succeed())
		cur.Spec.Genesis.ChainID = "atlantic-2"
		err := testCli.Update(testCtx, cur)
		g.Expect(err).To(HaveOccurred(), "mutating spec.genesis.chainId must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.genesis is immutable"))
	})

	t.Run("mutating stakingAmount is rejected", func(t *testing.T) {
		g := NewWithT(t)
		cur := &seiv1alpha1.SeiNodeDeployment{}
		g.Expect(testCli.Get(testCtx, client.ObjectKeyFromObject(snd), cur)).To(Succeed())
		cur.Spec.Genesis.StakingAmount = "99999999usei"
		err := testCli.Update(testCtx, cur)
		g.Expect(err).To(HaveOccurred(), "mutating spec.genesis.stakingAmount must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.genesis is immutable"))
	})

	t.Run("same-content update is allowed", func(t *testing.T) {
		g := NewWithT(t)
		cur := &seiv1alpha1.SeiNodeDeployment{}
		g.Expect(testCli.Get(testCtx, client.ObjectKeyFromObject(snd), cur)).To(Succeed())
		// Touch an unrelated field to force a write through the same path.
		cur.Spec.Replicas = 2
		g.Expect(testCli.Update(testCtx, cur)).To(Succeed(),
			"editing non-genesis fields must still succeed while genesis is unchanged")
	})
}

// TestGenesis_CreationWithoutGenesisAllowsLaterAddition guards the CEL
// rule's `!has(oldSelf.genesis)` short-circuit: an SND created without
// genesis can have it added later. Only mutation of a previously-set
// genesis block is forbidden.
func TestGenesis_CreationWithoutGenesisAllowsLaterAddition(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "genesis-late-add",
		fixtures.WithValidator(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	cur := &seiv1alpha1.SeiNodeDeployment{}
	g.Expect(testCli.Get(testCtx, client.ObjectKeyFromObject(snd), cur)).To(Succeed())
	cur.Spec.Genesis = &seiv1alpha1.GenesisCeremonyConfig{ChainID: "pacific-1"}
	err := testCli.Update(testCtx, cur)
	g.Expect(err).NotTo(HaveOccurred(),
		"adding spec.genesis to an SND that didn't have it must be permitted (got: %s)",
		errString(err))
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return strings.TrimSpace(err.Error())
}
