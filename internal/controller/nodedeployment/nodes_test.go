package nodedeployment

import (
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func newTestGroup(name, namespace string) *seiv1alpha1.SeiNodeDeployment {
	return &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Replicas: 3,
			Template: seiv1alpha1.SeiNodeTemplate{
				Spec: seiv1alpha1.SeiNodeSpec{
					ChainID: "pacific-1",
					Image:   "ghcr.io/sei-protocol/seid:v1.0.0",
					FullNode: &seiv1alpha1.FullNodeSpec{
						Snapshot: &seiv1alpha1.SnapshotSource{
							S3: &seiv1alpha1.S3SnapshotSource{
								TargetHeight: 100000,
							},
						},
					},
					Sidecar: &seiv1alpha1.SidecarConfig{Port: 7777},
				},
			},
		},
	}
}

func TestGenerateSeiNode_NameAndNamespace(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")

	node := generateSeiNode(group, 0)
	g.Expect(node.Name).To(Equal("archive-rpc-0"))
	g.Expect(node.Namespace).To(Equal("sei"))

	node2 := generateSeiNode(group, 2)
	g.Expect(node2.Name).To(Equal("archive-rpc-2"))
}

func TestGenerateSeiNode_SystemLabels(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")

	node := generateSeiNode(group, 1)

	g.Expect(node.Labels).To(HaveKeyWithValue(groupLabel, "archive-rpc"))
	g.Expect(node.Labels).To(HaveKeyWithValue(groupOrdinalLabel, "1"))
}

func TestGenerateSeiNode_UserLabelsAreMerged(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Template.Metadata = &seiv1alpha1.SeiNodeTemplateMeta{
		Labels: map[string]string{
			"team": "platform",
			"env":  "production",
		},
	}

	node := generateSeiNode(group, 0)

	g.Expect(node.Labels).To(HaveKeyWithValue("team", "platform"))
	g.Expect(node.Labels).To(HaveKeyWithValue("env", "production"))
	g.Expect(node.Labels).To(HaveKeyWithValue(groupLabel, "archive-rpc"))
}

func TestGenerateSeiNode_SystemLabelsOverrideUser(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Template.Metadata = &seiv1alpha1.SeiNodeTemplateMeta{
		Labels: map[string]string{
			groupLabel: "should-be-overridden",
		},
	}

	node := generateSeiNode(group, 0)
	g.Expect(node.Labels).To(HaveKeyWithValue(groupLabel, "archive-rpc"))
}

func TestGenerateSeiNode_InjectsPodLabels(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")

	node := generateSeiNode(group, 0)

	g.Expect(node.Spec.PodLabels).NotTo(BeNil())
	g.Expect(node.Spec.PodLabels).To(HaveKeyWithValue(groupLabel, "archive-rpc"))
}

func TestGenerateSeiNode_PreservesExistingPodLabels(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Template.Spec.PodLabels = map[string]string{
		"existing": "value",
	}

	node := generateSeiNode(group, 0)

	g.Expect(node.Spec.PodLabels).To(HaveKeyWithValue("existing", "value"))
	g.Expect(node.Spec.PodLabels).To(HaveKeyWithValue(groupLabel, "archive-rpc"))
}

func TestGenerateSeiNode_PropagatesTemplateMetadataLabelsToPods(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Template.Metadata = &seiv1alpha1.SeiNodeTemplateMeta{
		Labels: map[string]string{
			"sei.io/chain-id": "pacific-1",
			"sei.io/role":     "archive",
		},
	}

	node := generateSeiNode(group, 0)

	g.Expect(node.Spec.PodLabels).To(HaveKeyWithValue("sei.io/chain-id", "pacific-1"))
	g.Expect(node.Spec.PodLabels).To(HaveKeyWithValue("sei.io/role", "archive"))
	g.Expect(node.Spec.PodLabels).To(HaveKeyWithValue(groupLabel, "archive-rpc"))
}

func TestGenerateSeiNode_PodLabelsOverrideTemplateMetadataLabels(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Template.Metadata = &seiv1alpha1.SeiNodeTemplateMeta{
		Labels: map[string]string{
			"key": "from-metadata",
		},
	}
	group.Spec.Template.Spec.PodLabels = map[string]string{
		"key": "from-pod-labels",
	}

	node := generateSeiNode(group, 0)

	g.Expect(node.Spec.PodLabels).To(HaveKeyWithValue("key", "from-pod-labels"))
}

func TestGenerateSeiNode_SystemLabelsOverrideUserLabels(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Template.Metadata = &seiv1alpha1.SeiNodeTemplateMeta{
		Labels: map[string]string{
			groupLabel: "user-attempt-to-override",
		},
	}
	group.Spec.Template.Spec.PodLabels = map[string]string{
		groupLabel: "another-override-attempt",
	}

	node := generateSeiNode(group, 0)

	g.Expect(node.Spec.PodLabels).To(HaveKeyWithValue(groupLabel, "archive-rpc"))
}

func TestGenerateSeiNode_CopiesSpec(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("full-node", "pacific-1")

	node := generateSeiNode(group, 0)

	g.Expect(node.Name).To(Equal("full-node-0"))
	g.Expect(node.Namespace).To(Equal("pacific-1"))
	g.Expect(node.Spec.ChainID).To(Equal("pacific-1"))
	g.Expect(node.Spec.Image).To(Equal("ghcr.io/sei-protocol/seid:v1.0.0"))
	g.Expect(node.Spec.FullNode).NotTo(BeNil())
}

func TestGenerateSeiNode_Annotations(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Template.Metadata = &seiv1alpha1.SeiNodeTemplateMeta{
		Annotations: map[string]string{
			"example.com/team": "platform",
		},
	}

	node := generateSeiNode(group, 0)
	g.Expect(node.Annotations).To(HaveKeyWithValue("example.com/team", "platform"))
}

func TestGenerateSeiNode_NoAnnotationsWhenNil(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")

	node := generateSeiNode(group, 0)
	g.Expect(node.Annotations).To(BeNil())
}

func TestDetectDeploymentNeeded_InPlace_SetsRolloutInProgress(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.UpdateStrategy = seiv1alpha1.UpdateStrategy{Type: seiv1alpha1.UpdateStrategyInPlace}
	group.Status.TemplateHash = testOldHash
	group.Status.IncumbentNodes = []string{"archive-rpc-0", "archive-rpc-1", "archive-rpc-2"}

	r := &SeiNodeDeploymentReconciler{Recorder: record.NewFakeRecorder(10)}
	r.detectDeploymentNeeded(group)

	cond := apimeta.FindStatusCondition(group.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("TemplateChanged"))

	g.Expect(group.Status.Rollout).NotTo(BeNil())
	g.Expect(group.Status.Rollout.TargetHash).NotTo(BeEmpty())
	g.Expect(group.Status.IncumbentNodes).To(ConsistOf("archive-rpc-0", "archive-rpc-1", "archive-rpc-2"))
}

func TestDetectDeploymentNeeded_InPlace_AlreadyActive_SameTarget(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.UpdateStrategy = seiv1alpha1.UpdateStrategy{Type: seiv1alpha1.UpdateStrategyInPlace}
	group.Status.TemplateHash = testOldHash
	group.Status.IncumbentNodes = []string{"archive-rpc-0"}

	currentHash := templateHash(&group.Spec.Template.Spec)

	setCondition(group, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
		"TemplateChanged", "already rolling")

	existingRollout := &seiv1alpha1.RolloutStatus{
		TargetHash: currentHash,
	}
	group.Status.Rollout = existingRollout

	r := &SeiNodeDeploymentReconciler{Recorder: record.NewFakeRecorder(10)}
	r.detectDeploymentNeeded(group)

	g.Expect(group.Status.Rollout).To(Equal(existingRollout))
}

func TestDetectDeploymentNeeded_InPlace_Supersedes_StaleRollout(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.UpdateStrategy = seiv1alpha1.UpdateStrategy{Type: seiv1alpha1.UpdateStrategyInPlace}
	group.Status.TemplateHash = testOldHash
	group.Status.IncumbentNodes = []string{"archive-rpc-0"}

	setCondition(group, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
		"TemplateChanged", "already rolling")

	group.Status.Rollout = &seiv1alpha1.RolloutStatus{
		TargetHash: "stale-hash",
	}
	group.Status.Plan = &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive}

	r := &SeiNodeDeploymentReconciler{Recorder: record.NewFakeRecorder(10)}
	r.detectDeploymentNeeded(group)

	g.Expect(group.Status.Rollout.TargetHash).NotTo(Equal("stale-hash"))
	g.Expect(group.Status.Plan).To(BeNil())
}

// Defense in depth: the CRD enum rejects empty Type at the apiserver,
// but in-memory specs (tests, controller-internal copies after a CRD
// downgrade-then-upgrade) might omit it. The migration handler logs a
// warning and treats it as InPlace rather than panicking on an empty
// strategy string.
func TestDetectDeploymentNeeded_EmptyType_TreatedAsInPlace(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.UpdateStrategy = seiv1alpha1.UpdateStrategy{Type: ""}
	group.Status.TemplateHash = testOldHash
	group.Status.IncumbentNodes = []string{"archive-rpc-0"}

	r := &SeiNodeDeploymentReconciler{Recorder: record.NewFakeRecorder(10)}
	r.detectDeploymentNeeded(group)

	g.Expect(group.Status.Rollout).NotTo(BeNil())

	cond := apimeta.FindStatusCondition(group.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
}

// Regression armor for the empty-incumbents guard in detectDeploymentNeeded.
// If populateIncumbentNodes finds zero children (stale owner refs after a
// manual edit, etc.), a rollout with empty node lists would create a plan
// whose tasks all complete as no-ops and re-fire indefinitely.
func TestDetectDeploymentNeeded_NoIncumbentNodes_NoRollout(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.UpdateStrategy = seiv1alpha1.UpdateStrategy{Type: seiv1alpha1.UpdateStrategyInPlace}
	group.Status.TemplateHash = testOldHash
	group.Status.IncumbentNodes = nil

	r := &SeiNodeDeploymentReconciler{Recorder: record.NewFakeRecorder(10)}
	r.detectDeploymentNeeded(group)

	g.Expect(group.Status.Rollout).To(BeNil())
	cond := apimeta.FindStatusCondition(group.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(cond).To(BeNil())
}

func TestGenerateSeiNode_DeepCopiesTemplate(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Template.Spec.Overrides = map[string]string{
		"evm.http_port": "8545",
	}

	node := generateSeiNode(group, 0)
	node.Spec.Overrides["modified"] = "true"

	g.Expect(group.Spec.Template.Spec.Overrides).NotTo(HaveKey("modified"),
		"modification to generated SeiNode should not mutate the group template")
}

func TestSetGenesisCeremonyCondition(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*seiv1alpha1.SeiNodeDeployment)
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		{
			name:       "no genesis spec sets False/NotApplicable",
			mutate:     func(g *seiv1alpha1.SeiNodeDeployment) { g.Spec.Genesis = nil },
			wantStatus: metav1.ConditionFalse,
			wantReason: "NotApplicable",
		},
		{
			name: "already complete stays True/Complete (latched)",
			mutate: func(g *seiv1alpha1.SeiNodeDeployment) {
				setCondition(g, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionTrue, "Complete", "ceremony already done")
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: "Complete",
		},
		{
			name: "latched True survives operator clearing spec.genesis",
			mutate: func(g *seiv1alpha1.SeiNodeDeployment) {
				setCondition(g, seiv1alpha1.ConditionGenesisCeremonyComplete, metav1.ConditionTrue, "Complete", "ceremony already done")
				g.Spec.Genesis = nil
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: "Complete",
		},
		{
			name: "plan in progress sets False/InProgress",
			mutate: func(g *seiv1alpha1.SeiNodeDeployment) {
				setCondition(g, seiv1alpha1.ConditionPlanInProgress, metav1.ConditionTrue, "Running", "")
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: "InProgress",
		},
		{
			name:       "genesis configured but not started sets False/NotStarted",
			mutate:     func(g *seiv1alpha1.SeiNodeDeployment) {},
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonNotStarted,
		},
		{
			// Post-failPlan: PlanInProgress=False, no latched Complete.
			// The next reconcile must drop back to NotStarted so retries
			// can proceed.
			name: "plan failed returns to False/NotStarted",
			mutate: func(g *seiv1alpha1.SeiNodeDeployment) {
				setCondition(g, seiv1alpha1.ConditionPlanInProgress, metav1.ConditionFalse, "PlanFailed", "previous attempt failed")
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: ReasonNotStarted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			group := newTestGroup("archive-rpc", "sei")
			group.Spec.Genesis = &seiv1alpha1.GenesisCeremonyConfig{ChainID: "pacific-1"}
			tc.mutate(group)

			r := &SeiNodeDeploymentReconciler{Recorder: record.NewFakeRecorder(10)}
			r.setGenesisCeremonyCondition(group)

			cond := apimeta.FindStatusCondition(group.Status.Conditions, seiv1alpha1.ConditionGenesisCeremonyComplete)
			g.Expect(cond).NotTo(BeNil(), "ConditionGenesisCeremonyComplete must be set on every reconciled SND")
			g.Expect(cond.Status).To(Equal(tc.wantStatus))
			g.Expect(cond.Reason).To(Equal(tc.wantReason))
		})
	}
}
