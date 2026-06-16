package seinetwork

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

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

func TestGenerateSeiNode_SystemLabelsOverrideUserLabels(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.Template.Metadata = &seiv1alpha1.SeiNodeTemplateMeta{
		Labels: map[string]string{
			groupLabel: "user-attempt-to-override",
		},
	}
	network.Spec.Template.Spec.PodLabels = map[string]string{
		groupLabel: "another-override-attempt",
	}

	node := generateSeiNode(network, 0)

	g.Expect(node.Spec.PodLabels).To(HaveKeyWithValue(groupLabel, testNetworkName))
}

func TestGenerateSeiNode_CopiesSpec(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testNamespace)

	node := generateSeiNode(network, 0)

	g.Expect(node.Name).To(Equal(testNode0))
	g.Expect(node.Namespace).To(Equal(testNamespace))
	g.Expect(node.Spec.ChainID).To(Equal(testNamespace))
	g.Expect(node.Spec.Image).To(Equal("ghcr.io/sei-protocol/seid:v1.0.0"))
	g.Expect(node.Spec.Validator).NotTo(BeNil())
}

// generateSeiNode stamps the per-node genesis ceremony config on the
// validator template. Every SeiNetwork runs the ceremony.
func TestGenerateSeiNode_StampsGenesisCeremony(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.Genesis = seiv1alpha1.GenesisCeremonyConfig{
		ChainID:        "loadtest-1",
		StakingAmount:  "5usei",
		AccountBalance: "1000usei",
	}
	network.Spec.Template.Spec.ChainID = ""

	node := generateSeiNode(network, 2)

	g.Expect(node.Spec.ChainID).To(Equal("loadtest-1"), "chainId falls back to genesis when template leaves it empty")
	g.Expect(node.Spec.Validator).NotTo(BeNil())
	g.Expect(node.Spec.Validator.GenesisCeremony).NotTo(BeNil())
	g.Expect(node.Spec.Validator.GenesisCeremony.ChainID).To(Equal("loadtest-1"))
	g.Expect(node.Spec.Validator.GenesisCeremony.StakingAmount).To(Equal("5usei"))
	g.Expect(node.Spec.Validator.GenesisCeremony.AccountBalance).To(Equal("1000usei"))
	g.Expect(node.Spec.Validator.GenesisCeremony.Index).To(Equal(int32(2)))
}

func TestGenerateSeiNode_Annotations(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.Template.Metadata = &seiv1alpha1.SeiNodeTemplateMeta{
		Annotations: map[string]string{
			"example.com/team": "platform",
		},
	}

	node := generateSeiNode(network, 0)
	g.Expect(node.Annotations).To(HaveKeyWithValue("example.com/team", "platform"))
}

func TestGenerateSeiNode_NoAnnotationsWhenNil(t *testing.T) {
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

	currentHash := templateHash(&network.Spec.Template.Spec)

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

func TestGenerateSeiNode_DeepCopiesTemplate(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testNetworkName, testGroupNS)
	network.Spec.Template.Spec.Overrides = map[string]string{
		"evm.http_port": "8545",
	}

	node := generateSeiNode(network, 0)
	node.Spec.Overrides["modified"] = "true"

	g.Expect(network.Spec.Template.Spec.Overrides).NotTo(HaveKey("modified"),
		"modification to generated SeiNode should not mutate the network template")
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

// Adding a peer source to an existing SeiNetwork must reach the child.
func TestEnsureSeiNode_PropagatesPeersOnUpdate(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	network.Spec.Template.Spec.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{
			Region: "eu-central-1",
			Tags:   map[string]string{"ChainIdentifier": testNamespace, "Component": "state-syncer"},
		}},
	}

	r := newPlanTestReconciler(t, network)

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	child := &seiv1alpha1.SeiNode{}
	childKey := types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}
	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.Spec.Peers).To(HaveLen(1))

	network.Spec.Template.Spec.Peers = append(network.Spec.Template.Spec.Peers,
		seiv1alpha1.PeerSource{Label: &seiv1alpha1.LabelPeerSource{
			Namespace: testNamespace,
			Selector:  map[string]string{"sei.io/chain": testNamespace},
		}})

	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	g.Expect(r.Get(ctx, childKey, child)).To(Succeed())
	g.Expect(child.Spec.Peers).To(HaveLen(2),
		"existing child Spec.Peers must reflect network template additions")
	g.Expect(child.Spec.Peers[1].Label).NotTo(BeNil())
	g.Expect(child.Spec.Peers[1].Label.Selector).To(Equal(map[string]string{"sei.io/chain": testNamespace}))
}

// Removing a peer source from the SeiNetwork must also propagate.
func TestEnsureSeiNode_PropagatesPeerRemoval(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	network.Spec.Template.Spec.Peers = []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"k": "v"}}},
		{Label: &seiv1alpha1.LabelPeerSource{Namespace: testNamespace, Selector: map[string]string{"k": "v"}}},
	}

	r := newPlanTestReconciler(t, network)
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	network.Spec.Template.Spec.Peers = network.Spec.Template.Spec.Peers[:1]
	g.Expect(r.ensureSeiNode(ctx, network, 0)).To(Succeed())

	child := &seiv1alpha1.SeiNode{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: testSyncerOrd0, Namespace: testNamespace}, child)).To(Succeed())
	g.Expect(child.Spec.Peers).To(HaveLen(1))
	g.Expect(child.Spec.Peers[0].EC2Tags).NotTo(BeNil())
	g.Expect(child.Spec.Peers[0].Label).To(BeNil())
}

// No-op reconcile path: identical template across two reconciles must not
// trigger a child Update (no resourceVersion bump).
func TestEnsureSeiNode_PeersNoOpWhenUnchanged(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork("syncer", testNamespace)
	network.Spec.Template.Spec.Peers = []seiv1alpha1.PeerSource{
		{Label: &seiv1alpha1.LabelPeerSource{Namespace: testNamespace, Selector: map[string]string{"k": "v"}}},
	}

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
