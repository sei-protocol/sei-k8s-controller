package seinetwork

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func configValue(fileName, key, rawJSON string) seiv1alpha1.ConfigValue {
	return seiv1alpha1.ConfigValue{
		FileName: fileName,
		Key:      key,
		Value:    apiextensionsv1.JSON{Raw: []byte(rawJSON)},
	}
}

func TestGenerateSeiNode_StampsConfigValues(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.ConfigValues = []seiv1alpha1.ConfigValue{
		configValue("app.toml", "evm.enable", `true`),
		configValue("config.toml", "consensus.timeout_commit", `"2s"`),
	}

	for _, ordinal := range []int{0, 1, 2} {
		child := generateSeiNode(network, ordinal)
		g.Expect(child.Spec.ConfigValues).To(Equal(network.Spec.ConfigValues),
			"every validator child is stamped with the network's config values")
	}
}

// An unset network field leaves the child field unset, so a network that never
// uses config values does not hand the SeiNode substrate an empty non-nil set.
func TestGenerateSeiNode_NoConfigValuesLeavesChildUnset(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)

	g.Expect(generateSeiNode(network, 0).Spec.ConfigValues).To(BeNil())
}

func TestGenerateSeiNode_ConfigValuesNotAliased(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.ConfigValues = []seiv1alpha1.ConfigValue{configValue("app.toml", "evm.enable", `true`)}

	child := generateSeiNode(network, 0)

	child.Spec.ConfigValues[0].Key = "evm.http_port"
	child.Spec.ConfigValues[0].Value.Raw[0] = 'f'

	g.Expect(network.Spec.ConfigValues[0].Key).To(Equal("evm.enable"),
		"writing through the child must not reach the network spec")
	g.Expect(string(network.Spec.ConfigValues[0].Value.Raw)).To(Equal("true"),
		"the child's JSON value must not share the network's backing array")
}

// Config overrides and config values are independent fields; the network
// carries both to the child, where the SeiNode substrate resolves precedence.
func TestGenerateSeiNode_ConfigValuesCoexistWithOverrides(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.ConfigOverrides = map[string]string{testOverrideKey: testOverrideVal}
	network.Spec.ConfigValues = []seiv1alpha1.ConfigValue{configValue("app.toml", "evm.enable", `true`)}

	child := generateSeiNode(network, 0)

	g.Expect(child.Spec.Overrides).To(HaveKeyWithValue(testOverrideKey, testOverrideVal))
	g.Expect(child.Spec.ConfigValues).To(HaveLen(1))
}

// The network's set is authoritative across every kind of edit: a new set on a
// child that had none, a changed value, a removed entry, and a full clear.
func TestEnsureSeiNode_PropagatesConfigValueEdits(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	r := newPlanTestReconciler(t, network)
	childKey := types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}

	readChild := func() *seiv1alpha1.SeiNode {
		t.Helper()
		child := &seiv1alpha1.SeiNode{}
		g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
		return child
	}

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(readChild().Spec.ConfigValues).To(BeEmpty())

	network.Spec.ConfigValues = []seiv1alpha1.ConfigValue{
		configValue("app.toml", "evm.enable", `true`),
		configValue("config.toml", "consensus.timeout_commit", `"2s"`),
	}
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(readChild().Spec.ConfigValues).To(Equal(network.Spec.ConfigValues))

	network.Spec.ConfigValues[0] = configValue("app.toml", "evm.enable", `false`)
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(string(readChild().Spec.ConfigValues[0].Value.Raw)).To(Equal("false"))

	network.Spec.ConfigValues = network.Spec.ConfigValues[:1]
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(readChild().Spec.ConfigValues).To(HaveLen(1),
		"a removed entry is dropped from the child, not merged back in")

	network.Spec.ConfigValues = nil
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(readChild().Spec.ConfigValues).To(BeEmpty(),
		"clearing the network's set clears every child's set")
}

// A direct edit on a child is drift, not intent: the next reconcile restores
// the network's set.
func TestEnsureSeiNode_ReconcilesDirectChildConfigValueEdit(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	network.Spec.ConfigValues = []seiv1alpha1.ConfigValue{configValue("app.toml", "evm.enable", `true`)}
	r := newPlanTestReconciler(t, network)
	childKey := types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	child := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	child.Spec.ConfigValues = []seiv1alpha1.ConfigValue{
		configValue("app.toml", "evm.enable", `false`),
		configValue("app.toml", "operator.only", `1`),
	}
	g.Expect(r.Update(ctx, child)).To(Succeed())

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.Spec.ConfigValues).To(Equal(network.Spec.ConfigValues),
		"the network's values are authoritative over a direct child edit")
}

// An unchanged set must not re-encode into a write: a spurious child Update
// every reconcile would churn the SeiNode config-values hash.
func TestEnsureSeiNode_ConfigValuesNoOpWhenUnchanged(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	network.Spec.ConfigValues = []seiv1alpha1.ConfigValue{configValue("app.toml", "evm.enable", `true`)}
	r := newPlanTestReconciler(t, network)
	childKey := types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	child := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	rvBefore := child.ResourceVersion

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.ResourceVersion).To(Equal(rvBefore))
}

// A validator created after the values were set receives them at creation,
// with no aliasing between siblings.
func TestReconcileSeiNodes_NewChildReceivesExistingConfigValues(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	network.Spec.ConfigValues = []seiv1alpha1.ConfigValue{configValue("app.toml", "evm.enable", `true`)}
	r := newPlanTestReconciler(t, network)

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(r.ensureSeiNode(ctx, network, 1)).To(Succeed())

	first := &seiv1alpha1.SeiNode{}
	second := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}, first)).To(Succeed())
	g.Expect(r.Get(ctx, types.NamespacedName{Name: "syncer-1", Namespace: testNamespace}, second)).To(Succeed())

	g.Expect(first.Spec.ConfigValues).To(Equal(network.Spec.ConfigValues))
	g.Expect(second.Spec.ConfigValues).To(Equal(network.Spec.ConfigValues))
}
