//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// The network's spec.consensus is stamped onto every validator child at
// creation; the engine is create-only on both Kinds so nothing later diverges.
func TestConsensus_PropagatesToValidators(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	network := fixtures.NewNetwork(ns, "consensus-autobahn", fixtures.WithReplicas(2))
	allow := true
	network.Spec.Consensus = &seiv1alpha1.NetworkConsensusSpec{
		ConsensusSpec: seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn, EvmOnly: true},
		Autobahn:      &seiv1alpha1.AutobahnCeremonySpec{BlockInterval: "1s", AllowEmptyBlocks: &allow},
	}
	network.Spec.Genesis.ConsensusParams = &apiextensionsv1.JSON{Raw: []byte(`{"block":{"max_gas":"35000000"}}`)}
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())

	for _, name := range []string{network.Name + "-0", network.Name + "-1"} {
		key := types.NamespacedName{Name: name, Namespace: ns}
		waitFor(t, func() bool {
			child := &seiv1alpha1.SeiNode{}
			if err := testCli.Get(testCtx, key, child); err != nil {
				return false
			}
			return child.Spec.Consensus != nil &&
				child.Spec.Consensus.Engine == seiv1alpha1.ConsensusEngineAutobahn &&
				child.Spec.Consensus.EvmOnly
		}, name+" carries the network's consensus")
	}

	stored := &seiv1alpha1.SeiNetwork{}
	g.Expect(testCli.Get(testCtx, client.ObjectKeyFromObject(network), stored)).To(Succeed())
	g.Expect(stored.Spec.Genesis.ConsensusParams).NotTo(BeNil())
	g.Expect(string(stored.Spec.Genesis.ConsensusParams.Raw)).To(MatchJSON(`{"block":{"max_gas":"35000000"}}`))
	g.Expect(stored.Spec.Consensus.Autobahn).NotTo(BeNil())
	g.Expect(stored.Spec.Consensus.Autobahn.BlockInterval).To(Equal("1s"))
}

func TestConsensus_NetworkShapes(t *testing.T) {
	ns := makeNamespace(t)
	one := int64(1)
	zero := int64(0)
	overCap := int64(2001)
	cases := []struct {
		name      string
		consensus *seiv1alpha1.NetworkConsensusSpec
		errorText string
	}{
		{name: "omitted"},
		{name: "autobahn", consensus: &seiv1alpha1.NetworkConsensusSpec{ConsensusSpec: seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn}}},
		{name: "tendermint-evm-only", consensus: &seiv1alpha1.NetworkConsensusSpec{ConsensusSpec: seiv1alpha1.ConsensusSpec{EvmOnly: true}}, errorText: "evmOnly requires engine Autobahn"},
		{name: "autobahn-knobs", consensus: &seiv1alpha1.NetworkConsensusSpec{
			ConsensusSpec: seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn},
			Autobahn:      &seiv1alpha1.AutobahnCeremonySpec{BlockInterval: "250ms", MaxTxsPerBlock: &one},
		}},
		{name: "tendermint-knobs", consensus: &seiv1alpha1.NetworkConsensusSpec{
			Autobahn: &seiv1alpha1.AutobahnCeremonySpec{BlockInterval: "250ms"},
		}, errorText: "autobahn settings require engine Autobahn"},
		{name: "bad-interval", consensus: &seiv1alpha1.NetworkConsensusSpec{
			ConsensusSpec: seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn},
			Autobahn:      &seiv1alpha1.AutobahnCeremonySpec{BlockInterval: "400"},
		}, errorText: "blockInterval"},
		{name: "zero-max-txs", consensus: &seiv1alpha1.NetworkConsensusSpec{
			ConsensusSpec: seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn},
			Autobahn:      &seiv1alpha1.AutobahnCeremonySpec{MaxTxsPerBlock: &zero},
		}, errorText: "maxTxsPerBlock"},
		{name: "over-cap-max-txs", consensus: &seiv1alpha1.NetworkConsensusSpec{
			ConsensusSpec: seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn},
			Autobahn:      &seiv1alpha1.AutobahnCeremonySpec{MaxTxsPerBlock: &overCap},
		}, errorText: "maxTxsPerBlock"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			network := fixtures.NewNetwork(ns, "shape-"+tc.name)
			network.Spec.Consensus = tc.consensus
			err := testCli.Create(testCtx, network)
			if tc.errorText == "" {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.errorText))
		})
	}
}

func TestConsensus_NetworkEffectiveValueCreateOnly(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	network := fixtures.NewNetwork(ns, "consensus-immutable")
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	key := client.ObjectKeyFromObject(network)

	g.Expect(updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
		cur.Spec.Consensus = &seiv1alpha1.NetworkConsensusSpec{ConsensusSpec: seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineTendermint}}
	})).To(Succeed(), "making the default explicit is not a change")

	err := updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
		cur.Spec.Consensus = &seiv1alpha1.NetworkConsensusSpec{ConsensusSpec: seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn}}
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.consensus.engine is create-only"))
}

// The autobahn knobs are in the ceremony artifact every node already holds:
// changing, adding or dropping the block is rejected after creation.
func TestConsensus_AutobahnKnobsCreateOnly(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	network := fixtures.NewNetwork(ns, "autobahn-knobs-immutable")
	network.Spec.Consensus = &seiv1alpha1.NetworkConsensusSpec{
		ConsensusSpec: seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn},
		Autobahn:      &seiv1alpha1.AutobahnCeremonySpec{BlockInterval: "1s"},
	}
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	key := client.ObjectKeyFromObject(network)

	for name, mutate := range map[string]func(*seiv1alpha1.SeiNetwork){
		"change": func(cur *seiv1alpha1.SeiNetwork) { cur.Spec.Consensus.Autobahn.BlockInterval = "2s" },
		"add":    func(cur *seiv1alpha1.SeiNetwork) { v := true; cur.Spec.Consensus.Autobahn.AllowEmptyBlocks = &v },
		"drop":   func(cur *seiv1alpha1.SeiNetwork) { cur.Spec.Consensus.Autobahn = nil },
	} {
		err := updateNetworkWithRetry(t, key, mutate)
		g.Expect(err).To(HaveOccurred(), name)
		g.Expect(err.Error()).To(ContainSubstring("spec.consensus.autobahn is create-only"), name)
	}

	plain := fixtures.NewNetwork(ns, "autobahn-knobs-absent")
	plain.Spec.Consensus = &seiv1alpha1.NetworkConsensusSpec{ConsensusSpec: seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn}}
	g.Expect(testCli.Create(testCtx, plain)).To(Succeed())
	err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(plain), func(cur *seiv1alpha1.SeiNetwork) {
		cur.Spec.Consensus.Autobahn = &seiv1alpha1.AutobahnCeremonySpec{BlockInterval: "1s"}
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.consensus.autobahn is create-only"))
}
