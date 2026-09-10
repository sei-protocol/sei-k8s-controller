//go:build envtest

package envtest_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

func networkConfigValue(fileName, key, rawJSON string) seiv1alpha1.ConfigValue {
	return seiv1alpha1.ConfigValue{
		FileName: fileName,
		Key:      key,
		Value:    apiextensionsv1.JSON{Raw: []byte(rawJSON)},
	}
}

// childConfigValues reads a child's spec.configValues as a fileName/key ->
// raw JSON map, so assertions do not depend on list order.
func childConfigValues(t *testing.T, key types.NamespacedName) (map[string]string, bool) {
	t.Helper()
	child := &seiv1alpha1.SeiNode{}
	if err := testCli.Get(testCtx, key, child); err != nil {
		return nil, false
	}
	got := make(map[string]string, len(child.Spec.ConfigValues))
	for _, v := range child.Spec.ConfigValues {
		got[v.FileName+"/"+v.Key] = string(v.Value.Raw)
	}
	return got, true
}

// The network's spec.configValues are authoritative for the whole validator
// set: they land on every child at creation and every later edit — a changed
// value, a removed entry, a direct child edit — converges back through
// ensureSeiNode against a real apiserver, list-map merge semantics included.
func TestConfigValues_PropagateToEveryValidator(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	const replicas = 2

	network := fixtures.NewNetwork(ns, "config-values",
		fixtures.WithReplicas(replicas),
		fixtures.WithConfigValues(
			networkConfigValue("app.toml", "evm.enable", `true`),
			networkConfigValue("config.toml", "consensus.timeout_commit", `"2s"`),
			// A table and a float: the two shapes the apiserver is free to
			// re-serialize differently from what was written.
			networkConfigValue("app.toml", "state-sync.snapshot", `{"interval":1500,"keep-recent":2}`),
			networkConfigValue("app.toml", "evm.min-fee", `0.02`),
		),
	)
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	key := client.ObjectKeyFromObject(network)

	childKeys := []types.NamespacedName{
		{Name: network.Name + "-0", Namespace: ns},
		{Name: network.Name + "-1", Namespace: ns},
	}

	// 1. Creation-time stamping: both validators carry the network's set.
	waitFor(t, func() bool {
		for _, ck := range childKeys {
			got, ok := childConfigValues(t, ck)
			if !ok || got["app.toml/evm.enable"] != "true" ||
				got["config.toml/consensus.timeout_commit"] != `"2s"` {
				return false
			}
		}
		return true
	}, "every validator child is created with the network's config values")

	// 1b. A converged, populated set is not rewritten every reconcile. The
	//     parent compares the child's stored values byte-for-byte, so an
	//     apiserver re-serialization of a table or a float would show up here
	//     as an endless Update loop that the fake client cannot reproduce.
	//     Asserted on content, not on the object's version: the node controller
	//     and the genesis ceremony write the same child for their own reasons.
	g.Consistently(func() map[string]string {
		got, _ := childConfigValues(t, childKeys[0])
		return got
	}, 3*time.Second, pollInterval).Should(Equal(map[string]string{
		"app.toml/evm.enable":                  "true",
		"config.toml/consensus.timeout_commit": `"2s"`,
		"app.toml/state-sync.snapshot":         `{"interval":1500,"keep-recent":2}`,
		"app.toml/evm.min-fee":                 `0.02`,
	}), "a converged set must survive apiserver round-tripping byte-for-byte")

	// 2. A changed value and a removed entry in one edit: the network's set
	//    replaces the child's, it is not merged into it.
	g.Expect(updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
		cur.Spec.ConfigValues = []seiv1alpha1.ConfigValue{
			networkConfigValue("app.toml", "evm.enable", `false`),
		}
	})).To(Succeed())

	waitFor(t, func() bool {
		for _, ck := range childKeys {
			got, ok := childConfigValues(t, ck)
			if !ok || len(got) != 1 || got["app.toml/evm.enable"] != "false" {
				return false
			}
		}
		return true
	}, "a changed value and a removed entry both reach every validator child")

	// 3. A direct edit on one child is drift, not intent.
	g.Eventually(func() error {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKeys[0], child); err != nil {
			return err
		}
		patch := client.MergeFrom(child.DeepCopy())
		child.Spec.ConfigValues = []seiv1alpha1.ConfigValue{
			networkConfigValue("app.toml", "evm.enable", `true`),
			networkConfigValue("app.toml", "operator.only", `1`),
		}
		return testCli.Patch(testCtx, child, patch)
	}, 5*time.Second, pollInterval).Should(Succeed())

	waitFor(t, func() bool {
		got, ok := childConfigValues(t, childKeys[0])
		return ok && len(got) == 1 && got["app.toml/evm.enable"] == "false"
	}, "an operator's direct edit on a child reconciles back to the network's values")

	// 4. Clearing the field on the network clears it on every child, and the
	//    converged set does not oscillate across further reconcile laps.
	g.Expect(updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
		cur.Spec.ConfigValues = nil
	})).To(Succeed())

	waitFor(t, func() bool {
		for _, ck := range childKeys {
			got, ok := childConfigValues(t, ck)
			if !ok || len(got) != 0 {
				return false
			}
		}
		return true
	}, "clearing the network's config values clears every child's")

	g.Consistently(func() bool {
		got, ok := childConfigValues(t, childKeys[0])
		return ok && len(got) == 0
	}, 3*time.Second, pollInterval).Should(BeTrue(),
		"a cleared set must stay cleared, not be rewritten every reconcile")
}

// spec.replicas is fixed at the genesis ceremony, so the only validator
// created after the network already carries config values is a replacement
// for a lost one. It is stamped at creation, not left behind until the next
// unrelated edit.
func TestConfigValues_ReplacementValidatorReceivesExistingValues(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	network := fixtures.NewNetwork(ns, "config-values-replace",
		fixtures.WithReplicas(1),
		fixtures.WithConfigValues(networkConfigValue("app.toml", "evm.enable", `true`)),
	)
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())

	childKey := types.NamespacedName{Name: network.Name + "-0", Namespace: ns}
	waitFor(t, func() bool {
		got, ok := childConfigValues(t, childKey)
		return ok && got["app.toml/evm.enable"] == "true"
	}, "the initial validator carries the network's config values")

	child := &seiv1alpha1.SeiNode{}
	g.Expect(testCli.Get(testCtx, childKey, child)).To(Succeed())
	originalUID := child.UID
	if len(child.Finalizers) > 0 {
		patch := client.MergeFrom(child.DeepCopy())
		child.Finalizers = nil
		g.Expect(testCli.Patch(testCtx, child, patch)).To(Succeed())
	}
	g.Expect(testCli.Delete(testCtx, child)).To(Succeed())

	waitFor(t, func() bool {
		fresh := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, fresh); err != nil || fresh.UID == originalUID {
			return false
		}
		got, ok := childConfigValues(t, childKey)
		return ok && got["app.toml/evm.enable"] == "true"
	}, "the recreated validator is stamped with the existing network values")
}
