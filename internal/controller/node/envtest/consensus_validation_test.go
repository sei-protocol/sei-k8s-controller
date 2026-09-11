//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// Admission-level coverage of spec.consensus on SeiNode: which engine/evmOnly
// shapes the API server accepts, which it rejects, and that the EFFECTIVE
// values are create-only. No controller is involved.

func consensusFullNode(ns, name string, consensus *seiv1alpha1.ConsensusSpec) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   "envtest-1",
			Image:     "sei:latest",
			FullNode:  &seiv1alpha1.FullNodeSpec{},
			Consensus: consensus,
		},
	}
}

func TestConsensus_Shapes(t *testing.T) {
	ns := makeNamespace(t)
	cases := []struct {
		name      string
		consensus *seiv1alpha1.ConsensusSpec
		errorText string
	}{
		{name: "omitted"},
		{name: "empty", consensus: &seiv1alpha1.ConsensusSpec{}},
		{name: "tendermint", consensus: &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineTendermint}},
		{name: "autobahn", consensus: &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn}},
		{name: "autobahn-evm-only", consensus: &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn, EvmOnly: true}},
		{name: "tendermint-evm-only", consensus: &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineTendermint, EvmOnly: true}, errorText: "evmOnly requires engine Autobahn"},
		{name: "implicit-engine-evm-only", consensus: &seiv1alpha1.ConsensusSpec{EvmOnly: true}, errorText: "evmOnly requires engine Autobahn"},
		{name: "unknown-engine", consensus: &seiv1alpha1.ConsensusSpec{Engine: "Narwhal"}, errorText: "supported values"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			err := testCli.Create(testCtx, consensusFullNode(ns, "consensus-"+tc.name, tc.consensus))
			if tc.errorText == "" {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.errorText))
		})
	}
}

func TestConsensus_SeedCannotBeEvmOnly(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := seedNode(ns, "seed-evm-only", "seed-0-node-key")
	node.Spec.Consensus = &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn, EvmOnly: true}
	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("seed cannot be evmOnly"))

	node.Spec.Consensus.EvmOnly = false
	g.Expect(testCli.Create(testCtx, node)).To(Succeed(), "an Autobahn seed is an ordinary seed")
}

func TestConsensus_AutobahnRefusesFreezeHeight(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := frozenFullNode(ns, "autobahn-frozen", 1000)
	node.Spec.Consensus = &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn}
	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("freeze height is not supported under engine Autobahn"))
}

func TestConsensus_EvmOnlyOwnsListenerOverrides(t *testing.T) {
	ns := makeNamespace(t)
	for _, key := range []string{"network.rpc.listen_address", "api.rest.enable", "api.grpc.enable", "api.grpc_web.enable"} {
		t.Run(key, func(t *testing.T) {
			g := NewWithT(t)
			node := consensusFullNode(ns, "evm-only-override", &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn, EvmOnly: true})
			node.Spec.Overrides = map[string]string{key: "x"}
			err := testCli.Create(testCtx, node)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("EVM-only node owns"))
		})
	}
}

// The effective value is what is create-only: spelling out the defaults on a
// node that omitted them is admitted, changing either value is not.
func TestConsensus_EffectiveValueCreateOnly(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := consensusFullNode(ns, "consensus-immutable", nil)
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
	key := client.ObjectKeyFromObject(node)

	g.Expect(updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.Consensus = &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineTendermint, EvmOnly: false}
	})).To(Succeed(), "making the defaults explicit is not a change")

	g.Expect(updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.Consensus = nil
	})).To(Succeed(), "dropping explicit defaults is not a change either")

	err := updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.Consensus = &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn}
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.consensus.engine is create-only"))

	autobahn := consensusFullNode(ns, "consensus-immutable-evm", &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn})
	g.Expect(testCli.Create(testCtx, autobahn)).To(Succeed())
	err = updateNodeWithRetry(t, client.ObjectKeyFromObject(autobahn), func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.Consensus.EvmOnly = true
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.consensus.evmOnly is create-only"))
}
