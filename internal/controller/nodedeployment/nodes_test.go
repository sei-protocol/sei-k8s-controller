package nodedeployment

import (
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

	r := &SeiNodeDeploymentReconciler{}
	r.detectDeploymentNeeded(group)

	cond := apimeta.FindStatusCondition(group.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("TemplateChanged"))

	g.Expect(group.Status.Rollout).NotTo(BeNil())
	g.Expect(group.Status.Rollout.Strategy).To(Equal(seiv1alpha1.UpdateStrategyInPlace))
	g.Expect(group.Status.Rollout.TargetHash).NotTo(BeEmpty())
	g.Expect(group.Status.Rollout.Nodes).To(HaveLen(3))
	g.Expect(group.Status.Rollout.Nodes[0].Name).To(Equal("archive-rpc-0"))
	g.Expect(group.Status.Rollout.EntrantNodes).To(BeEmpty())
}

func TestDetectDeploymentNeeded_InPlace_AlreadyActive(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.UpdateStrategy = seiv1alpha1.UpdateStrategy{Type: seiv1alpha1.UpdateStrategyInPlace}
	group.Status.TemplateHash = testOldHash
	group.Status.IncumbentNodes = []string{"archive-rpc-0"}

	setCondition(group, seiv1alpha1.ConditionRolloutInProgress, metav1.ConditionTrue,
		"TemplateChanged", "already rolling")

	existingRollout := &seiv1alpha1.RolloutStatus{
		Strategy:   seiv1alpha1.UpdateStrategyInPlace,
		TargetHash: "existing-target",
	}
	group.Status.Rollout = existingRollout

	r := &SeiNodeDeploymentReconciler{}
	r.detectDeploymentNeeded(group)

	g.Expect(group.Status.Rollout).To(Equal(existingRollout))
}

func TestDetectDeploymentNeeded_EmptyType_TreatedAsInPlace(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.UpdateStrategy = seiv1alpha1.UpdateStrategy{Type: ""}
	group.Status.TemplateHash = testOldHash
	group.Status.IncumbentNodes = []string{"archive-rpc-0"}

	r := &SeiNodeDeploymentReconciler{}
	r.detectDeploymentNeeded(group)

	g.Expect(group.Status.Rollout).NotTo(BeNil())
	g.Expect(group.Status.Rollout.Strategy).To(Equal(seiv1alpha1.UpdateStrategyInPlace))

	cond := apimeta.FindStatusCondition(group.Status.Conditions, seiv1alpha1.ConditionRolloutInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
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
