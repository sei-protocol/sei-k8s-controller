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
	g.Expect(err.Error()).To(ContainSubstring("a seed cannot be EVM-only"))

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
	g.Expect(err.Error()).To(ContainSubstring("the execution engine (spec.executionEngine.mode, or the deprecated spec.consensus.evmOnly) is create-only"))
}

// --- spec.executionEngine ---

func executionEngineNode(ns, name string, engine *seiv1alpha1.ExecutionEngineSpec, consensus *seiv1alpha1.ConsensusSpec) *seiv1alpha1.SeiNode {
	node := consensusFullNode(ns, name, consensus)
	node.Spec.ExecutionEngine = engine
	return node
}

func autobahn() *seiv1alpha1.ConsensusSpec {
	return &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn}
}

func evmOnlyEngine(httpEnabled *bool) *seiv1alpha1.ExecutionEngineSpec {
	e := &seiv1alpha1.ExecutionEngineSpec{Mode: seiv1alpha1.ExecutionEngineEvmOnly}
	if httpEnabled != nil {
		e.EvmOnly = &seiv1alpha1.EvmOnlyExecutionSpec{HttpEnabled: httpEnabled}
	}
	return e
}

func TestExecutionEngine_Shapes(t *testing.T) {
	ns := makeNamespace(t)
	off := false
	cases := []struct {
		name      string
		engine    *seiv1alpha1.ExecutionEngineSpec
		consensus *seiv1alpha1.ConsensusSpec
		errorText string
	}{
		{name: "empty", engine: &seiv1alpha1.ExecutionEngineSpec{}},
		{name: "default", engine: &seiv1alpha1.ExecutionEngineSpec{Mode: seiv1alpha1.ExecutionEngineDefault}},
		{name: "evm-only-autobahn", engine: evmOnlyEngine(nil), consensus: autobahn()},
		{name: "evm-only-http-off", engine: evmOnlyEngine(&off), consensus: autobahn()},
		{name: "evm-only-tendermint", engine: evmOnlyEngine(nil), errorText: "executionEngine mode EvmOnly requires consensus engine Autobahn"},
		{name: "unknown-mode", engine: &seiv1alpha1.ExecutionEngineSpec{Mode: "Wasm"}, errorText: "supported values"},
		{name: "evm-only-settings-under-default", engine: &seiv1alpha1.ExecutionEngineSpec{
			Mode:    seiv1alpha1.ExecutionEngineDefault,
			EvmOnly: &seiv1alpha1.EvmOnlyExecutionSpec{HttpEnabled: &off},
		}, errorText: "evmOnly settings require mode EvmOnly"},
		// Both representations set and agreeing is fine; disagreeing is not.
		{name: "typed-and-legacy-agree", engine: evmOnlyEngine(nil), consensus: &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn, EvmOnly: true}},
		{name: "typed-default-legacy-true", engine: &seiv1alpha1.ExecutionEngineSpec{Mode: seiv1alpha1.ExecutionEngineDefault},
			consensus: &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn, EvmOnly: true}, errorText: "disagree"},
		{name: "typed-empty-legacy-true", engine: &seiv1alpha1.ExecutionEngineSpec{},
			consensus: &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn, EvmOnly: true}, errorText: "disagree"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			err := testCli.Create(testCtx, executionEngineNode(ns, "engine-"+tc.name, tc.engine, tc.consensus))
			if tc.errorText == "" {
				g.Expect(err).NotTo(HaveOccurred())
				return
			}
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.errorText))
		})
	}
}

func TestExecutionEngine_SeedCannotBeEvmOnly(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := seedNode(ns, "seed-evm-only-typed", "seed-0-node-key")
	node.Spec.Consensus = autobahn()
	node.Spec.ExecutionEngine = evmOnlyEngine(nil)
	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("a seed cannot be EVM-only"))
}

func TestExecutionEngine_EvmOnlyOwnsListenerOverrides(t *testing.T) {
	ns := makeNamespace(t)
	for _, key := range []string{"network.rpc.listen_address", "api.rest.enable", "api.grpc.enable", "api.grpc_web.enable", "evm.http_enabled"} {
		t.Run(key, func(t *testing.T) {
			g := NewWithT(t)
			node := executionEngineNode(ns, "evm-only-override-typed", evmOnlyEngine(nil), autobahn())
			node.Spec.Overrides = map[string]string{key: "x"}
			err := testCli.Create(testCtx, node)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("EVM-only node owns"))
		})
	}
}

// Migrating a node from the deprecated bool to the typed field is a no-op on
// the effective engine and is admitted; a real mode change, or a change to
// httpEnabled, is create-only.
func TestExecutionEngine_EffectiveValueCreateOnly(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	off := false

	legacy := consensusFullNode(ns, "engine-migrate", &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn, EvmOnly: true})
	g.Expect(testCli.Create(testCtx, legacy)).To(Succeed())
	key := client.ObjectKeyFromObject(legacy)

	g.Expect(updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.ExecutionEngine = evmOnlyEngine(nil)
	})).To(Succeed(), "adding the typed field that agrees with the bool is not a change")

	g.Expect(updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.Consensus.EvmOnly = false
	})).To(Succeed(), "dropping the deprecated bool once the typed field carries the mode is not a change")

	err := updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.ExecutionEngine.Mode = seiv1alpha1.ExecutionEngineDefault
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("is create-only"))

	err = updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.ExecutionEngine.EvmOnly = &seiv1alpha1.EvmOnlyExecutionSpec{HttpEnabled: &off}
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("spec.executionEngine.evmOnly.httpEnabled is create-only"))

	plain := consensusFullNode(ns, "engine-plain", autobahn())
	g.Expect(testCli.Create(testCtx, plain)).To(Succeed())
	g.Expect(updateNodeWithRetry(t, client.ObjectKeyFromObject(plain), func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.ExecutionEngine = &seiv1alpha1.ExecutionEngineSpec{Mode: seiv1alpha1.ExecutionEngineDefault}
	})).To(Succeed(), "spelling out Default is not a change")
	err = updateNodeWithRetry(t, client.ObjectKeyFromObject(plain), func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.ExecutionEngine = evmOnlyEngine(nil)
	})
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("is create-only"))
}
