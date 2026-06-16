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
			// Post-failPlan: PlanInProgress=False, no latched Complete.
			// The next reconcile must drop back to NotStarted so retries
			// can proceed.
			name: "plan failed returns to False/NotStarted",
			mutate: func(n *seiv1alpha1.SeiNetwork) {
				setCondition(n, seiv1alpha1.ConditionPlanInProgress, metav1.ConditionFalse, "PlanFailed", "previous attempt failed")
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonNotStarted,
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
			Labels:    map[string]string{groupLabel: testNetworkName},
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
