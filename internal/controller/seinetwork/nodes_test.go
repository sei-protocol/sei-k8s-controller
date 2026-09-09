package seinetwork

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestGenerateSeiNode_NameAndNamespace(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)

	node := generateSeiNode(network, 0)
	g.Expect(node.Name).To(Equal(testNode0))
	g.Expect(node.Namespace).To(Equal(testGroupNS))

	node2 := generateSeiNode(network, 2)
	g.Expect(node2.Name).To(Equal("genesis-net-2"))
}

func TestGenerateSeiNode_SystemLabels(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)

	node := generateSeiNode(network, 1)
	// Frozen GitOps selector keys (kept for selector continuity).
	g.Expect(node.Labels).To(HaveKeyWithValue(groupLabel, testNetworkName))
	g.Expect(node.Labels).To(HaveKeyWithValue(groupOrdinalLabel, "1"))
	// New canonical seinetwork keys, stamped alongside the frozen ones.
	g.Expect(node.Labels).To(HaveKeyWithValue(seinetworkLabel, testNetworkName))
	g.Expect(node.Labels).To(HaveKeyWithValue(seinetworkOrdinalLabel, "1"))
	g.Expect(node.Labels).To(HaveKeyWithValue(chainLabel, testNamespace))
	// A SeiNetwork is always a validator pool, so the dropped-template
	// sei.io/role is restamped here for role-filtering GitOps/selectors.
	g.Expect(node.Labels).To(HaveKeyWithValue(roleLabel, roleValidator))
}

// The reserved group/ordinal pod labels are controller-owned and
// must overwrite any same-keyed user PodLabels.
func TestGenerateSeiNode_SystemPodLabelsOverrideUserLabels(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.PodLabels = map[string]string{
		groupLabel: "user-attempt-to-override",
		"team":     "platform",
	}

	node := generateSeiNode(network, 0)

	g.Expect(node.Spec.PodLabels).To(HaveKeyWithValue(groupLabel, testNetworkName))
	g.Expect(node.Spec.PodLabels).To(HaveKeyWithValue(seinetworkLabel, testNetworkName),
		"canonical seinetwork key is stamped on pods alongside the frozen key")
	g.Expect(node.Spec.PodLabels).To(HaveKeyWithValue("team", "platform"),
		"non-reserved user pod labels pass through")
}

// generateSeiNode constructs the child spec from the scalar genesis fields:
// chainId, image, configOverrides, sidecar all flow through directly, and a
// genesis-ceremony validator is synthesized with no BYO key material.
func TestGenerateSeiNode_ConstructsFromScalars(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testNamespace)
	network.Spec.ConfigOverrides = map[string]string{testOverrideKey: testOverrideVal}

	node := generateSeiNode(network, 0)

	g.Expect(node.Name).To(Equal(testNode0))
	g.Expect(node.Namespace).To(Equal(testNamespace))
	g.Expect(node.Spec.ChainID).To(Equal(testNamespace))
	g.Expect(node.Spec.Image).To(Equal("ghcr.io/sei-protocol/seid:v1.0.0"))
	g.Expect(node.Spec.Overrides).To(HaveKeyWithValue(testOverrideKey, testOverrideVal))
	g.Expect(node.Spec.Sidecar).NotTo(BeNil())
	g.Expect(node.Spec.Validator).NotTo(BeNil())

	// No bring-your-own identity: every replica's identity is ceremony-generated.
	g.Expect(node.Spec.Validator.SigningKey).To(BeNil())
	g.Expect(node.Spec.Validator.NodeKey).To(BeNil())
	g.Expect(node.Spec.Validator.OperatorKeyring).To(BeNil())
	g.Expect(node.Spec.Validator.Snapshot).To(BeNil())

	// Follower/networking fields are never set on a genesis validator.
	g.Expect(node.Spec.Peers).To(BeEmpty())
	g.Expect(node.Spec.ExternalAddress).To(BeEmpty())
	g.Expect(node.Spec.FullNode).To(BeNil())
	g.Expect(node.Spec.Archive).To(BeNil())
	g.Expect(node.Spec.Replayer).To(BeNil())
}

// generateSeiNode synthesizes the per-node genesis ceremony config from the
// network's genesis block, with Index taken from the ordinal. ChainID is
// sourced unconditionally from genesis.chainId.
func TestGenerateSeiNode_StampsGenesisCeremony(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.Genesis = seiv1alpha1.GenesisCeremonyConfig{
		ChainID:        "loadtest-1",
		StakingAmount:  "5usei",
		AccountBalance: "1000usei",
	}

	node := generateSeiNode(network, 2)

	g.Expect(node.Spec.ChainID).To(Equal("loadtest-1"), "chainId is sourced from genesis.chainId")
	g.Expect(node.Spec.Validator).NotTo(BeNil())
	g.Expect(node.Spec.Validator.GenesisCeremony).NotTo(BeNil())
	g.Expect(node.Spec.Validator.GenesisCeremony.ChainID).To(Equal("loadtest-1"))
	g.Expect(node.Spec.Validator.GenesisCeremony.StakingAmount).To(Equal("5usei"))
	g.Expect(node.Spec.Validator.GenesisCeremony.AccountBalance).To(Equal("1000usei"))
	g.Expect(node.Spec.Validator.GenesisCeremony.Index).To(Equal(int32(2)))
}

// The scoped genesis spec carries no per-node annotation knob, so children get none.
func TestGenerateSeiNode_NoAnnotations(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)

	node := generateSeiNode(network, 0)
	g.Expect(node.Annotations).To(BeNil())
}

// setRolloutInProgressCondition is a derived projection: True when a child's
// reported image lags spec.image, False/AllUpToDate at steady state, and
// False before any children exist (nothing to roll).
func TestSetRolloutInProgressCondition_Derived(t *testing.T) {
	cases := []struct {
		name       string
		upToDate   int32
		desired    int32
		childCount int
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{"all up to date", 3, 3, 3, metav1.ConditionFalse, "AllUpToDate"},
		{"child mid-roll", 1, 3, 3, metav1.ConditionTrue, "ImageRolling"},
		{"wedged on bad tag", 0, 2, 2, metav1.ConditionTrue, "ImageRolling"},
		{"no children yet", 0, 3, 0, metav1.ConditionFalse, "AllUpToDate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			network := newTestNetwork(testNetworkName, testGroupNS)

			setRolloutInProgressCondition(network, tc.upToDate, tc.desired, tc.childCount)

			cond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
			g.Expect(cond).NotTo(BeNil(), "RolloutInProgress must always be present")
			g.Expect(cond.Status).To(Equal(tc.wantStatus))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}
}

// Editing spec.image on a live network must propagate in-place to the
// existing child every reconcile (no hash gate). The child's own SeiNode
// controller then rolls its StatefulSet.
func TestEnsureSeiNode_PropagatesImage(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	r := newPlanTestReconciler(t, network)

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	const newImage = "ghcr.io/sei-protocol/seid:v2.0.0"
	network.Spec.Image = newImage
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	child := &seiv1alpha1.SeiNode{}
	childKey := types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.Spec.Image).To(Equal(newImage))
}

// Editing spec.sidecar.resources on a live network must propagate the WHOLE
// Sidecar struct (not just image/port) to the existing child every reconcile,
// so a sidecar resource bump reaches children.
func TestEnsureSeiNode_PropagatesSidecarResources(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	r := newPlanTestReconciler(t, network)

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	childKey := types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}
	child := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.Spec.Sidecar).NotTo(BeNil())
	g.Expect(child.Spec.Sidecar.Resources).To(BeNil(), "no resources set at create")

	network.Spec.Sidecar.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.Spec.Sidecar.Resources).NotTo(BeNil(),
		"a spec.sidecar.resources change must propagate to the child")
	g.Expect(child.Spec.Sidecar.Resources.Requests.Memory().String()).To(Equal("256Mi"))
	g.Expect(child.Spec.Sidecar.Resources.Limits.Memory().String()).To(Equal("512Mi"))
}

// generateSeiNode must not alias the network's ConfigOverrides map into the
// child — a later mutation of the child spec must not write back through.
func TestGenerateSeiNode_OverridesNotAliased(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.ConfigOverrides = map[string]string{testOverrideKey: testOverrideVal}

	node := generateSeiNode(network, 0)
	// Mutate the returned child map in place. If the child aliased the parent
	// map, this write would surface on network.Spec.ConfigOverrides.
	node.Spec.Overrides["modified"] = "true"
	node.Spec.Overrides[testOverrideKey] = "mutated"

	g.Expect(network.Spec.ConfigOverrides).NotTo(HaveKey("modified"),
		"writing a new key into the child's overrides must not write through to the network spec")
	g.Expect(network.Spec.ConfigOverrides).To(HaveKeyWithValue(testOverrideKey, testOverrideVal),
		"overwriting an existing key in the child's overrides must not mutate the network spec")
}

// networkConfigValues is the two-file config-value set the ConfigValues tests
// declare on the network. A fresh slice per call, so a test that mutates the
// parent set cannot leak into the next one.
func networkConfigValues() []seiv1alpha1.ConfigValue {
	return []seiv1alpha1.ConfigValue{
		{File: testConfigFile, Key: testConfigKey, Value: testConfigVal},
		{File: testConfigFileApp, Key: testConfigKeyApp, Value: testConfigValApp},
	}
}

// ensureAllChildren runs ensureSeiNode across every replica ordinal, which is
// what reconcileSeiNodes does for a network not under an active plan.
func ensureAllChildren(t *testing.T, ctx context.Context, r *SeiNetworkReconciler, network *seiv1alpha1.SeiNetwork) {
	t.Helper()
	g := NewWithT(t)
	for i := range int(network.Spec.Replicas) {
		g.Expect(r.ensureSeiNode(ctx, network, i)).To(Succeed(), "ensuring ordinal %d", i)
	}
}

// childNames lists the child SeiNode names for every replica ordinal.
func childNames(network *seiv1alpha1.SeiNetwork) []string {
	names := make([]string, 0, network.Spec.Replicas)
	for i := range int(network.Spec.Replicas) {
		names = append(names, seiNodeName(network, i))
	}
	return names
}

// getChild reads one child SeiNode by name.
func getChild(t *testing.T, ctx context.Context, r *SeiNetworkReconciler, name string) *seiv1alpha1.SeiNode {
	t.Helper()
	g := NewWithT(t)
	child := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, child)).To(Succeed())
	return child
}

// A config value on the network is stamped onto every validator child at
// creation, so one entry configures the whole pool (spec Requirement 2,
// criterion 1). Checked at the generate layer, across ordinals.
func TestGenerateSeiNode_StampsConfigValues(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.ConfigValues = networkConfigValues()

	for _, ordinal := range []int{0, 1, 2} {
		node := generateSeiNode(network, ordinal)

		g.Expect(node.Spec.ConfigValues).To(ConsistOf(networkConfigValues()),
			"ordinal %d must carry the network's whole config-value set", ordinal)
	}
}

// An unset spec.configValues leaves the child's field nil — no empty slice that
// would read as "the operator declared an empty set".
func TestGenerateSeiNode_NoConfigValuesLeavesChildNil(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	g.Expect(network.Spec.ConfigValues).To(BeNil())

	g.Expect(generateSeiNode(network, 0).Spec.ConfigValues).To(BeNil())
}

// generateSeiNode must not alias the network's ConfigValues backing array into
// the child. Aliasing is invisible with one replica and corrupts the pool with
// several: every child would share one slice, so a write through any of them
// would rewrite the others. Mirrors TestGenerateSeiNode_ResourcesNotAliased.
func TestGenerateSeiNode_ConfigValuesNotAliased(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.ConfigValues = networkConfigValues()

	child := generateSeiNode(network, 0)

	// Write through the CHILD's slice. An aliased child would reach the parent.
	child.Spec.ConfigValues[0].Value = "mutated"
	child.Spec.ConfigValues = append(child.Spec.ConfigValues,
		seiv1alpha1.ConfigValue{File: testConfigFile, Key: "added", Value: "1"})

	g.Expect(network.Spec.ConfigValues).To(ConsistOf(networkConfigValues()),
		"writing through the child's config values must not mutate the network spec")

	// And the reverse direction: a later parent edit must not reach an
	// already-generated child.
	other := generateSeiNode(network, 1)
	network.Spec.ConfigValues[0].Value = "parent-changed"

	g.Expect(other.Spec.ConfigValues[0].Value).To(Equal(testConfigVal),
		"an already-generated child must not follow a later parent edit")
}

// Editing spec.configValues on a live network propagates in-place to every
// existing child (spec Requirement 2, criterion 2 — the create half of SC-001).
func TestEnsureSeiNode_PropagatesConfigValues(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	r := newPlanTestReconciler(t, network)
	ensureAllChildren(t, ctx, r, network)

	for _, name := range childNames(network) {
		g.Expect(getChild(t, ctx, r, name).Spec.ConfigValues).To(BeEmpty(),
			"%s starts with no config values", name)
	}

	network.Spec.ConfigValues = networkConfigValues()
	ensureAllChildren(t, ctx, r, network)

	for _, name := range childNames(network) {
		g.Expect(getChild(t, ctx, r, name).Spec.ConfigValues).
			To(ConsistOf(networkConfigValues()), "%s must hold the network's config values", name)
	}
}

// The network's config values are authoritative for its children, and the sync
// is a WHOLE-SET replacement rather than a per-entry merge. That single property
// is what makes all four of these converge: a changed value, a removed entry, a
// cleared set, and a direct edit to a child (spec Requirement 2, criteria 2-3;
// SC-005, SC-008).
func TestEnsureSeiNode_ConfigValuesDriftSync(t *testing.T) {
	// mutate runs after the children exist, and edits either the parent spec or
	// a child directly. want is the set every child must hold afterwards.
	cases := []struct {
		name   string
		mutate func(t *testing.T, ctx context.Context, r *SeiNetworkReconciler, network *seiv1alpha1.SeiNetwork)
		want   []seiv1alpha1.ConfigValue
	}{
		{
			name: "changed parent value reaches every child",
			mutate: func(_ *testing.T, _ context.Context, _ *SeiNetworkReconciler, network *seiv1alpha1.SeiNetwork) {
				network.Spec.ConfigValues[0].Value = testConfigValFalse
			},
			want: []seiv1alpha1.ConfigValue{
				{File: testConfigFile, Key: testConfigKey, Value: testConfigValFalse},
				{File: testConfigFileApp, Key: testConfigKeyApp, Value: testConfigValApp},
			},
		},
		{
			name: "removed parent entry disappears from every child",
			mutate: func(_ *testing.T, _ context.Context, _ *SeiNetworkReconciler, network *seiv1alpha1.SeiNetwork) {
				network.Spec.ConfigValues = network.Spec.ConfigValues[:1]
			},
			want: []seiv1alpha1.ConfigValue{
				{File: testConfigFile, Key: testConfigKey, Value: testConfigVal},
			},
		},
		{
			name: "clearing the parent set empties every child",
			mutate: func(_ *testing.T, _ context.Context, _ *SeiNetworkReconciler, network *seiv1alpha1.SeiNetwork) {
				network.Spec.ConfigValues = nil
			},
			want: nil,
		},
		{
			name: "a direct edit to a child reconciles back to the network set",
			mutate: func(t *testing.T, ctx context.Context, r *SeiNetworkReconciler, network *seiv1alpha1.SeiNetwork) {
				t.Helper()
				g := NewWithT(t)
				child := getChild(t, ctx, r, seiNodeName(network, 1))
				patch := client.MergeFrom(child.DeepCopy())
				child.Spec.ConfigValues = []seiv1alpha1.ConfigValue{
					{File: testConfigFile, Key: "operator.went.rogue", Value: "yes"},
				}
				g.Expect(r.Patch(ctx, child, patch)).To(Succeed())
			},
			want: networkConfigValues(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			network := newTestNetwork("syncer", testNamespace)
			network.Spec.ConfigValues = networkConfigValues()
			r := newPlanTestReconciler(t, network)
			ensureAllChildren(t, ctx, r, network)

			tc.mutate(t, ctx, r, network)
			ensureAllChildren(t, ctx, r, network)

			for _, name := range childNames(network) {
				got := getChild(t, ctx, r, name).Spec.ConfigValues
				if tc.want == nil {
					g.Expect(got).To(BeEmpty(), "%s must hold no config values", name)
					continue
				}
				g.Expect(got).To(ConsistOf(tc.want), "%s must converge on the network set", name)
			}
		})
	}
}

// configValues sits BESIDE the pre-existing configOverrides; both propagate on
// the same reconcile and neither clobbers the other (spec SC-007).
func TestEnsureSeiNode_ConfigValuesAndOverridesCoexist(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	network.Spec.ConfigOverrides = map[string]string{testOverrideKey: testOverrideVal}
	network.Spec.ConfigValues = networkConfigValues()

	r := newPlanTestReconciler(t, network)
	ensureAllChildren(t, ctx, r, network)

	for _, name := range childNames(network) {
		child := getChild(t, ctx, r, name)
		g.Expect(child.Spec.Overrides).To(HaveKeyWithValue(testOverrideKey, testOverrideVal),
			"%s keeps the dotted-key overrides", name)
		g.Expect(child.Spec.ConfigValues).To(ConsistOf(networkConfigValues()),
			"%s also carries the config values", name)
	}

	// Editing one must not disturb the other.
	network.Spec.ConfigValues[0].Value = testConfigValFalse
	ensureAllChildren(t, ctx, r, network)

	for _, name := range childNames(network) {
		child := getChild(t, ctx, r, name)
		g.Expect(child.Spec.Overrides).To(HaveKeyWithValue(testOverrideKey, testOverrideVal),
			"%s: a config-value edit must not clear the overrides map", name)
		g.Expect(child.Spec.ConfigValues).To(ContainElement(
			seiv1alpha1.ConfigValue{File: testConfigFile, Key: testConfigKey, Value: testConfigValFalse}))
	}
}

// Reconcile stays idempotent with config values set: a second pass over an
// unchanged network must issue NO child Update, so nothing downstream (a config
// re-apply, a seid restart) is triggered by a bare requeue. The slice compare
// has to be semantic for this — a per-field or pointer compare would see drift
// every loop and update forever.
func TestEnsureSeiNode_NoOpWhenConfigValuesUnchanged(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	network.Spec.ConfigValues = networkConfigValues()
	r := newPlanTestReconciler(t, network)
	ensureAllChildren(t, ctx, r, network)

	before := make(map[string]string, network.Spec.Replicas)
	for _, name := range childNames(network) {
		before[name] = getChild(t, ctx, r, name).ResourceVersion
	}

	ensureAllChildren(t, ctx, r, network)

	for _, name := range childNames(network) {
		g.Expect(getChild(t, ctx, r, name).ResourceVersion).To(Equal(before[name]),
			"%s: an unchanged second reconcile must not bump resourceVersion", name)
	}
}

// networkResources is the pool footprint used by the spec.resources tests.
func networkResources(cpu, mem string) *seiv1alpha1.Resources {
	return &seiv1alpha1.Resources{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		},
	}
}

// One field on the network sizes every genesis validator in the pool, so the
// operator does not restate the footprint per replica. generateSeiNode stamps it
// onto each child's spec.resources, where it becomes that node's
// highest-precedence sizing source.
func TestGenerateSeiNode_StampsResources(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.Resources = networkResources("4", "32Gi")

	for _, ordinal := range []int{0, 1, 2} {
		node := generateSeiNode(network, ordinal)

		g.Expect(node.Spec.Resources).NotTo(BeNil(),
			"ordinal %d must carry the pool footprint", ordinal)
		g.Expect(node.Spec.Resources.Requests[corev1.ResourceCPU]).
			To(Equal(resource.MustParse("4")))
		g.Expect(node.Spec.Resources.Requests[corev1.ResourceMemory]).
			To(Equal(resource.MustParse("32Gi")))
	}
}

// An unset spec.resources leaves the child's field nil, so every child stays on
// the app-config override or the per-mode code default. DeepCopy on a nil
// pointer returns nil, which is what makes the unconditional call in
// generateSeiNode safe.
func TestGenerateSeiNode_NoResourcesLeavesChildNil(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	g.Expect(network.Spec.Resources).To(BeNil())

	g.Expect(generateSeiNode(network, 0).Spec.Resources).To(BeNil())
}

// generateSeiNode must not alias the network's Resources into the child.
// Aliasing would be invisible in the single-replica case and corrupt the pool in
// the multi-replica one: every child would share one ResourceList, so a later
// write through any of them would rewrite the others. Mirrors
// TestGenerateSeiNode_OverridesNotAliased.
func TestGenerateSeiNode_ResourcesNotAliased(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.Resources = networkResources("4", "32Gi")

	child := generateSeiNode(network, 0)

	// Mutate the PARENT after generating. An aliased child would follow along.
	network.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("64")
	network.Spec.Resources.Requests["nvidia.com/gpu"] = resource.MustParse("1")

	g.Expect(child.Spec.Resources.Requests[corev1.ResourceCPU]).
		To(Equal(resource.MustParse("4")),
			"an already-generated child must not follow a later parent edit")
	g.Expect(child.Spec.Resources.Requests).NotTo(HaveKey(corev1.ResourceName("nvidia.com/gpu")),
		"a key added to the parent must not appear on an already-generated child")

	// And the reverse direction: writing through the child must not reach the parent.
	child.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("1Gi")
	g.Expect(network.Spec.Resources.Requests[corev1.ResourceMemory]).
		To(Equal(resource.MustParse("32Gi")),
			"writing through the child must not mutate the network spec")
}

// setGenesisCeremonyCondition has no NotApplicable branch: every SeiNetwork
// runs the ceremony (genesis is required).
func TestSetGenesisCeremonyCondition(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*seiv1alpha1.SeiNetwork)
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name: "already complete stays True/Complete (latched)",
			mutate: func(n *seiv1alpha1.SeiNetwork) {
				setCondition(n, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionTrue, "Complete", "ceremony already done")
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: "Complete",
		},
		{
			name: "plan in progress sets False/InProgress",
			mutate: func(n *seiv1alpha1.SeiNetwork) {
				setCondition(n, seiv1alpha1.ConditionPlanInProgress, metav1.ConditionTrue, "Running", "")
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "InProgress",
		},
		{
			name:       "not yet started sets False/NotStarted",
			mutate:     func(n *seiv1alpha1.SeiNetwork) {},
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonNotStarted,
		},
		{
			// Resting state after failPlan: PlanInProgress=False and the
			// genesis condition carries CeremonyFailed. The seed must keep it
			// sticky (not reset to NotStarted) so the failure stays visible
			// until the auto-retry plan starts.
			name: "CeremonyFailed is sticky while no plan is active",
			mutate: func(n *seiv1alpha1.SeiNetwork) {
				setCondition(n, seiv1alpha1.ConditionPlanInProgress, metav1.ConditionFalse, "PlanFailed", "previous attempt failed")
				setCondition(n, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionFalse, "CeremonyFailed", "genesis ceremony plan failed")
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "CeremonyFailed",
		},
		{
			// The auto-retry plan supersedes a prior CeremonyFailed: an active
			// plan moves the condition to InProgress so it tracks the live
			// attempt rather than lying about the stale failure.
			name: "active retry plan supersedes CeremonyFailed with InProgress",
			mutate: func(n *seiv1alpha1.SeiNetwork) {
				setCondition(n, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionFalse, "CeremonyFailed", "genesis ceremony plan failed")
				setCondition(n, seiv1alpha1.ConditionPlanInProgress, metav1.ConditionTrue, "PlanStarted", "retry")
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "InProgress",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			network := newTestNetwork(testNetworkName, testGroupNS)
			tc.mutate(network)

			r := &SeiNetworkReconciler{Recorder: record.NewFakeRecorder(10)}
			r.setGenesisCeremonyCondition(network)

			cond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionGenesisCeremonyComplete)
			g.Expect(cond).NotTo(BeNil(), "ConditionGenesisCeremonyComplete must be set on every reconciled SeiNetwork")
			g.Expect(cond.Status).To(Equal(tc.wantStatus))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}
}

// Peers are controller-owned: the collect-and-set-peers ceremony task patches
// each child's Spec.Peers with the assembled validator set. ensureSeiNode
// emits empty peers at create and MUST NOT clobber those controller writes on
// a subsequent reconcile.
func TestEnsureSeiNode_DoesNotClobberControllerSetPeers(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	r := newPlanTestReconciler(t, network)

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	child := &seiv1alpha1.SeiNode{}
	childKey := types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.Spec.Peers).To(BeEmpty(), "child starts with no peers at create")

	// Simulate the collect-and-set-peers task patching peers onto the child.
	patch := client.MergeFrom(child.DeepCopy())
	child.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"abc@syncer-0.syncer.pacific-1.svc:26656"}}},
	}
	g.Expect(r.Patch(ctx, child, patch)).To(Succeed())

	// A subsequent reconcile must leave the controller-set peers intact.
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.Spec.Peers).To(HaveLen(1),
		"controller-set peers must survive reconcile")
	g.Expect(child.Spec.Peers[0].Static).NotTo(BeNil())
}

// Editing spec.configOverrides on a live network must propagate in-place to
// the existing child.
func TestEnsureSeiNode_PropagatesConfigOverrides(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	r := newPlanTestReconciler(t, network)

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	network.Spec.ConfigOverrides = map[string]string{testOverrideKey: testOverrideVal}
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	child := &seiv1alpha1.SeiNode{}
	childKey := types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.Spec.Overrides).To(HaveKeyWithValue(testOverrideKey, testOverrideVal))
}

// syncPausedToChildren runs unconditionally in Reconcile — before reconcilePlan
// and regardless of plan state — so an unpause propagates to children even
// while a network-level plan (the genesis ceremony) is in progress. This is the
// deadlock guard: the SeiNode controller short-circuits on still-paused
// children, so a resume gated on plan completion would wedge await-nodes-running.
func TestSyncPausedToChildren_IgnoresPlanInProgress(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testNetworkName, testGroupNS)
	network.UID = "net-uid"
	setPlanInProgress(network, "Genesis", "assembling")

	child := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testNode0,
			Namespace: testGroupNS,
			Labels:    map[string]string{seinetworkLabel: testNetworkName},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: testAPIVersion,
				Kind:       testKind,
				Name:       testNetworkName,
				UID:        network.UID,
				Controller: new(true),
			}},
		},
		Spec: seiv1alpha1.SeiNodeSpec{Paused: true},
	}

	r := newPlanTestReconciler(t, network, child)

	// Unpause while PlanInProgress=True — must still reach the child.
	g.Expect(r.syncPausedToChildren(ctx, network, false)).To(Succeed())

	got := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: testNode0, Namespace: testGroupNS}, got)).To(Succeed())
	g.Expect(got.Spec.Paused).To(BeFalse(),
		"unpause must propagate to children even while a plan is in progress")
}

// No-op reconcile path: identical spec across two reconciles must not trigger
// a child Update (no resourceVersion bump).
func TestEnsureSeiNode_NoOpWhenUnchanged(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	r := newPlanTestReconciler(t, network)
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	child := &seiv1alpha1.SeiNode{}
	childKey := types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	rvBefore := child.ResourceVersion

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.ResourceVersion).To(Equal(rvBefore),
		"no-op reconcile must not bump resourceVersion")
}
