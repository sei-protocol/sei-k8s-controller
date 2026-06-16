//go:build envtest

package envtest_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// updateNetworkWithRetry re-fetches and re-applies mutate on resourceVersion
// conflict. The controller patches status concurrently, so a single
// Get → mutate → Update can lose the 409 race before CEL validation fires.
func updateNetworkWithRetry(t *testing.T, key client.ObjectKey, mutate func(*seiv1alpha1.SeiNetwork)) error {
	t.Helper()
	var lastErr error
	for i := 0; i < 10; i++ {
		cur := &seiv1alpha1.SeiNetwork{}
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
// post-creation mutation of spec.genesis. The ceremony's outputs are baked
// into chain state; later edits would diverge from on-chain truth and could
// defeat the GenesisCeremonyComplete latch. Every SeiNetwork has a genesis
// (it is required), so the gate is unconditional.
func TestGenesis_ImmutabilityGate(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	network := fixtures.NewNetwork(ns, "genesis-immutable")
	network.Spec.Genesis = seiv1alpha1.GenesisCeremonyConfig{
		ChainID:        "pacific-1",
		StakingAmount:  "10000000usei",
		AccountBalance: "1000000usei",
	}
	g.Expect(testCli.Create(testCtx, network)).To(Succeed(), "creating SeiNetwork with genesis must succeed")

	key := client.ObjectKeyFromObject(network)

	t.Run("mutating chainId is rejected", func(t *testing.T) {
		g := NewWithT(t)
		err := updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.Genesis.ChainID = "atlantic-2"
		})
		g.Expect(err).To(HaveOccurred(), "mutating spec.genesis.chainId must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("immutable"))
	})

	t.Run("mutating stakingAmount is rejected", func(t *testing.T) {
		g := NewWithT(t)
		err := updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.Genesis.StakingAmount = "99999999usei"
		})
		g.Expect(err).To(HaveOccurred(), "mutating spec.genesis.stakingAmount must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.genesis is immutable"))
	})

	t.Run("appending to accounts list is rejected", func(t *testing.T) {
		g := NewWithT(t)
		err := updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
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
		err := updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
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
		// Touching an unrelated field forces a write while genesis stays the
		// same — confirms the CEL rule passes value-equality checks.
		err := updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.Replicas = 2
		})
		g.Expect(err).NotTo(HaveOccurred(),
			"editing non-genesis fields must still succeed while genesis is unchanged")
	})
}

// TestGenesis_ConditionSeededOnFirstReconcile asserts GenesisCeremonyComplete
// is present after first reconcile. Asserting only presence (not reason)
// avoids a race: the planner schedules a ceremony plan immediately, which
// advances the reason from NotStarted to InProgress.
func TestGenesis_ConditionSeededOnFirstReconcile(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	network := fixtures.NewNetwork(ns, "genesis-seeded")
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())

	waitForStatus(t, client.ObjectKeyFromObject(network), func(n *seiv1alpha1.SeiNetwork) bool {
		return apimeta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionGenesisCeremonyComplete) != nil
	}, "GenesisCeremonyComplete must be present after first reconcile")
}
