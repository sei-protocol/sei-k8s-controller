//go:build envtest

package envtest_test

import (
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest/fixtures"
)

// updateSNDWithRetry re-fetches and re-applies mutate on resourceVersion
// conflict. The controller patches status concurrently, so a single
// Get → mutate → Update can lose the 409 race before CEL validation
// fires. Returns the first non-conflict error (or nil on success) so
// assertions land on the CEL rejection itself, not on the optimistic-
// concurrency conflict.
func updateSNDWithRetry(t *testing.T, key client.ObjectKey, mutate func(*seiv1alpha1.SeiNodeDeployment)) error {
	t.Helper()
	var lastErr error
	for i := 0; i < 10; i++ {
		cur := &seiv1alpha1.SeiNodeDeployment{}
		if err := testCli.Get(testCtx, key, cur); err != nil {
			return err
		}
		mutate(cur)
		err := testCli.Update(testCtx, cur)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	return lastErr
}

// TestGenesis_ImmutabilityGate asserts the spec-level CEL rule rejects
// post-creation mutation of spec.genesis. The ceremony's outputs are
// baked into chain state; later edits would diverge from on-chain truth
// and could defeat the GenesisCeremonyComplete latch.
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

	key := client.ObjectKeyFromObject(snd)

	t.Run("clearing spec.genesis is rejected", func(t *testing.T) {
		g := NewWithT(t)
		err := updateSNDWithRetry(t, key, func(cur *seiv1alpha1.SeiNodeDeployment) {
			cur.Spec.Genesis = nil
		})
		g.Expect(err).To(HaveOccurred(), "clearing spec.genesis must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.genesis is immutable"),
			"rejection must carry the CEL rule's immutability message; got: %s", err.Error())
	})

	t.Run("mutating chainId is rejected", func(t *testing.T) {
		g := NewWithT(t)
		err := updateSNDWithRetry(t, key, func(cur *seiv1alpha1.SeiNodeDeployment) {
			cur.Spec.Genesis.ChainID = "atlantic-2"
		})
		g.Expect(err).To(HaveOccurred(), "mutating spec.genesis.chainId must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.genesis is immutable"))
	})

	t.Run("mutating stakingAmount is rejected", func(t *testing.T) {
		g := NewWithT(t)
		err := updateSNDWithRetry(t, key, func(cur *seiv1alpha1.SeiNodeDeployment) {
			cur.Spec.Genesis.StakingAmount = "99999999usei"
		})
		g.Expect(err).To(HaveOccurred(), "mutating spec.genesis.stakingAmount must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.genesis is immutable"))
	})

	t.Run("appending to accounts list is rejected", func(t *testing.T) {
		g := NewWithT(t)
		err := updateSNDWithRetry(t, key, func(cur *seiv1alpha1.SeiNodeDeployment) {
			cur.Spec.Genesis.Accounts = append(cur.Spec.Genesis.Accounts, seiv1alpha1.GenesisAccount{
				Address: "sei1example0000000000000000000000000000000",
				Balance: "1000usei",
			})
		})
		g.Expect(err).To(HaveOccurred(), "appending to spec.genesis.accounts must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.genesis is immutable"))
	})

	t.Run("adding an overrides entry is rejected", func(t *testing.T) {
		g := NewWithT(t)
		err := updateSNDWithRetry(t, key, func(cur *seiv1alpha1.SeiNodeDeployment) {
			if cur.Spec.Genesis.Overrides == nil {
				cur.Spec.Genesis.Overrides = map[string]apiextensionsv1.JSON{}
			}
			cur.Spec.Genesis.Overrides["staking.params.unbonding_time"] = apiextensionsv1.JSON{
				Raw: []byte(`"1814400s"`),
			}
		})
		g.Expect(err).To(HaveOccurred(), "adding spec.genesis.overrides entry must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.genesis is immutable"))
	})

	t.Run("same-content update is allowed", func(t *testing.T) {
		g := NewWithT(t)
		// Touching an unrelated field forces a write while genesis stays
		// the same — confirms the CEL rule passes value-equality checks.
		err := updateSNDWithRetry(t, key, func(cur *seiv1alpha1.SeiNodeDeployment) {
			cur.Spec.Replicas = 2
		})
		g.Expect(err).NotTo(HaveOccurred(),
			"editing non-genesis fields must still succeed while genesis is unchanged")
	})
}

// TestGenesis_CreationWithoutGenesisAllowsLaterAddition guards the
// `!has(oldSelf.genesis)` short-circuit: nil → set is permitted; only
// mutation of an existing genesis block is forbidden.
func TestGenesis_CreationWithoutGenesisAllowsLaterAddition(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "genesis-late-add",
		fixtures.WithValidator(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	err := updateSNDWithRetry(t, client.ObjectKeyFromObject(snd), func(cur *seiv1alpha1.SeiNodeDeployment) {
		cur.Spec.Genesis = &seiv1alpha1.GenesisCeremonyConfig{ChainID: "pacific-1"}
	})
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

// TestGenesis_ConditionSeededOnEveryReconcile guards the hoist in
// controller.go: setGenesisCeremonyCondition runs before any path
// that may early-return, so the condition is visible immediately
// once the controller has reconciled the SND.
func TestGenesis_ConditionSeededOnEveryReconcile(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "genesis-seeded",
		fixtures.WithValidator(),
	)
	snd.Spec.Genesis = &seiv1alpha1.GenesisCeremonyConfig{ChainID: "pacific-1"}
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	waitForStatus(t, client.ObjectKeyFromObject(snd), func(s *seiv1alpha1.SeiNodeDeployment) bool {
		return apimeta.FindStatusCondition(s.Status.Conditions, seiv1alpha1.ConditionGenesisCeremonyComplete) != nil
	}, "GenesisCeremonyComplete must be present after first reconcile")
}
