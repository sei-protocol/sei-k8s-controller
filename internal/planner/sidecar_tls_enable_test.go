package planner

import (
	"slices"
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	tlsEnableSecretName = "validator-0-tls"
	tlsImageV1          = "sei:v1.0.0"
	tlsImageV2          = "sei:v2.0.0"
)

func tlsEnableRunningNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: sourceChainID, Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  sourceChainID,
			Image:    tlsImageV1,
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Sidecar:  &seiv1alpha1.SidecarConfig{TLS: &seiv1alpha1.SidecarTLSSpec{SecretName: tlsEnableSecretName}},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase:        seiv1alpha1.PhaseRunning,
			CurrentImage: tlsImageV1,
		},
	}
}

func setTLSSecretReady2(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus) {
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type: seiv1alpha1.ConditionSidecarTLSSecretReady, Status: status, Reason: seiv1alpha1.ReasonTLSSecretReady,
	})
}

// --- sidecarTLSEnableDrift detector ---

func TestSidecarTLSEnableDrift_FiresWhenSpecSetAndStatusEmptyAndReady(t *testing.T) {
	g := NewWithT(t)
	node := tlsEnableRunningNode()
	setTLSSecretReady2(node, metav1.ConditionTrue)
	g.Expect(sidecarTLSEnableDrift(node)).To(BeTrue())
}

func TestSidecarTLSEnableDrift_FalseWhenAlreadyConverged(t *testing.T) {
	g := NewWithT(t)
	node := tlsEnableRunningNode()
	node.Status.CurrentSidecarTLSSecretName = tlsEnableSecretName
	setTLSSecretReady2(node, metav1.ConditionTrue)
	g.Expect(sidecarTLSEnableDrift(node)).To(BeFalse())
}

func TestSidecarTLSEnableDrift_FalseWhenPreflightNotReady(t *testing.T) {
	g := NewWithT(t)
	node := tlsEnableRunningNode()
	setTLSSecretReady2(node, metav1.ConditionFalse)
	g.Expect(sidecarTLSEnableDrift(node)).To(BeFalse(),
		"detector must wait for preflight to validate the Secret before triggering a plan")
}

func TestSidecarTLSEnableDrift_FalseWhenTLSDisabled(t *testing.T) {
	g := NewWithT(t)
	node := tlsEnableRunningNode()
	node.Spec.Sidecar = nil
	g.Expect(sidecarTLSEnableDrift(node)).To(BeFalse())
}

// --- buildRunningPlan composition ---

func TestBuildRunningPlan_TLSEnableOnly(t *testing.T) {
	g := NewWithT(t)
	node := tlsEnableRunningNode()
	setTLSSecretReady2(node, metav1.ConditionTrue)

	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	types := taskTypes(plan)
	g.Expect(types).To(Equal([]string{
		task.TaskTypeApplyRBACProxyConfig,
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveSidecarTLS,
		TaskMarkReady,
	}))
}

func TestBuildRunningPlan_ImageAndTLSEnable_CoCompose(t *testing.T) {
	g := NewWithT(t)
	node := tlsEnableRunningNode()
	node.Spec.Image = tlsImageV2
	setTLSSecretReady2(node, metav1.ConditionTrue)

	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	types := taskTypes(plan)
	g.Expect(types).To(Equal([]string{
		task.TaskTypeApplyRBACProxyConfig,
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveImage,
		task.TaskTypeObserveSidecarTLS,
		TaskMarkReady,
	}))
}

func TestBuildRunningPlan_ImageOnlyDrift_TLSAlreadyConverged(t *testing.T) {
	g := NewWithT(t)
	node := tlsEnableRunningNode()
	node.Status.CurrentSidecarTLSSecretName = tlsEnableSecretName
	node.Spec.Image = tlsImageV2
	setTLSSecretReady2(node, metav1.ConditionTrue)

	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	g.Expect(taskTypes(plan)).To(Equal([]string{
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveImage,
		TaskMarkReady,
	}))
}

// --- ResolvePlan condition lifecycle on TLS enable ---

func TestResolvePlan_TLSEnable_SetsAndClearsCondition(t *testing.T) {
	g := NewWithT(t)
	node := tlsEnableRunningNode()
	setTLSSecretReady2(node, metav1.ConditionTrue)

	err := (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).NotTo(BeNil())

	cond := apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Message).To(ContainSubstring("tls enable"))

	node.Status.Plan.Phase = seiv1alpha1.TaskPlanComplete
	node.Status.CurrentSidecarTLSSecretName = tlsEnableSecretName

	err = (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).To(BeNil())

	cond = apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("UpdateComplete"))
}

// --- classifyPlan labels for the new variants ---

func TestClassifyPlan_TLSEnableOnly(t *testing.T) {
	g := NewWithT(t)
	node := tlsEnableRunningNode()
	setTLSSecretReady2(node, metav1.ConditionTrue)
	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(classifyPlan(plan)).To(Equal("tls-enable"))
}

func TestClassifyPlan_ImageAndTLSEnable(t *testing.T) {
	g := NewWithT(t)
	node := tlsEnableRunningNode()
	node.Spec.Image = tlsImageV2
	setTLSSecretReady2(node, metav1.ConditionTrue)
	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(classifyPlan(plan)).To(Equal("node-update-tls-enable"))
}

// --- init-plan ObserveSidecarTLS insertion ---

func TestBuildBasePlan_InsertsObserveSidecarTLSBeforeSidecarProgressionWhenTLSEnabled(t *testing.T) {
	g := NewWithT(t)
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: sourceChainID, Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  sourceChainID,
			Image:    tlsImageV1,
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Sidecar:  &seiv1alpha1.SidecarConfig{TLS: &seiv1alpha1.SidecarTLSSpec{SecretName: tlsEnableSecretName}},
		},
	}
	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	types := taskTypes(plan)
	obsIdx := slices.Index(types, task.TaskTypeObserveSidecarTLS)
	stsIdx := slices.Index(types, task.TaskTypeApplyStatefulSet)
	configIdx := slices.Index(types, TaskConfigApply)

	g.Expect(obsIdx).To(BeNumerically(">=", 0), "observe-sidecar-tls must appear")
	g.Expect(obsIdx).To(BeNumerically(">", stsIdx), "observe-sidecar-tls must follow ApplyStatefulSet")
	g.Expect(obsIdx).To(BeNumerically("<", configIdx), "observe-sidecar-tls must precede the first sidecar HTTP task (config-apply)")
}

func TestBuildBasePlan_NoObserveSidecarTLSWhenTLSDisabled(t *testing.T) {
	g := NewWithT(t)
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testValidatorName, Namespace: sourceChainID, Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  sourceChainID,
			Image:    tlsImageV1,
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(slices.Index(taskTypes(plan), task.TaskTypeObserveSidecarTLS)).To(Equal(-1),
		"observe-sidecar-tls must NOT appear when TLS is disabled")
}
