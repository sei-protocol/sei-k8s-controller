package seinetwork

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	network.Generation = 7

	node := generateSeiNode(network, 1)
	g.Expect(node.Labels).To(HaveKeyWithValue(groupLabel, testNetworkName))
	g.Expect(node.Labels).To(HaveKeyWithValue(groupOrdinalLabel, "1"))
	g.Expect(node.Labels).To(HaveKeyWithValue(revisionLabel, "7"))
	g.Expect(node.Labels).To(HaveKeyWithValue(chainLabel, testNamespace))
}

// The reserved group/ordinal/revision pod labels are controller-owned and
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

func TestDetectDeploymentNeeded_InPlace_SetsRolloutInProgress(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Status.TemplateHash = testOldHash
	network.Status.IncumbentNodes = []string{testNode0, "genesis-net-1", "genesis-net-2"}

	r := &SeiNetworkReconciler{Recorder: record.NewFakeRecorder(10)}
	r.detectDeploymentNeeded(network)

	cond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("TemplateChanged"))

	g.Expect(network.Status.Rollout).NotTo(BeNil())
	g.Expect(network.Status.Rollout.TargetHash).NotTo(BeEmpty())
}

func TestDetectDeploymentNeeded_InPlace_AlreadyActive_SameTarget(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Status.TemplateHash = testOldHash
	network.Status.IncumbentNodes = []string{testNode0}

	currentHash := templateHash(&network.Spec)

	setCondition(network, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
		"TemplateChanged", "already rolling")

	existingRollout := &seiv1alpha1.RolloutStatus{TargetHash: currentHash}
	network.Status.Rollout = existingRollout

	r := &SeiNetworkReconciler{Recorder: record.NewFakeRecorder(10)}
	r.detectDeploymentNeeded(network)

	g.Expect(network.Status.Rollout).To(Equal(existingRollout))
}

func TestDetectDeploymentNeeded_InPlace_Supersedes_StaleRollout(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Status.TemplateHash = testOldHash
	network.Status.IncumbentNodes = []string{testNode0}

	setCondition(network, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
		"TemplateChanged", "already rolling")

	network.Status.Rollout = &seiv1alpha1.RolloutStatus{TargetHash: "stale-hash"}
	network.Status.Plan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive}

	r := &SeiNetworkReconciler{Recorder: record.NewFakeRecorder(10)}
	r.detectDeploymentNeeded(network)

	g.Expect(network.Status.Rollout.TargetHash).NotTo(Equal("stale-hash"))
	g.Expect(network.Status.Plan).To(BeNil())
}

// Regression armor for the empty-incumbents guard in detectDeploymentNeeded.
func TestDetectDeploymentNeeded_NoIncumbentNodes_NoRollout(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Status.TemplateHash = testOldHash
	network.Status.IncumbentNodes = nil

	r := &SeiNetworkReconciler{Recorder: record.NewFakeRecorder(10)}
	r.detectDeploymentNeeded(network)

	g.Expect(network.Status.Rollout).To(BeNil())
	cond := apimeta.FindStatusCondition(network.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(cond).To(BeNil())
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
