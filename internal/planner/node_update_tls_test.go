package planner

import (
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	testIssuer    = "sei-internal-ca"
	testIssuerAlt = "sei-internal-ca-rotated"
	testKind      = "ClusterIssuer"
)

func tlsSpecOf(issuer, kind string) *seiv1alpha1.SidecarTLSSpec {
	return &seiv1alpha1.SidecarTLSSpec{IssuerName: issuer, IssuerKind: kind}
}

func tlsStatus() *seiv1alpha1.SidecarTLSStatus {
	return &seiv1alpha1.SidecarTLSStatus{IssuerName: testIssuer, IssuerKind: testKind}
}

func withTLSSpec(node *seiv1alpha1.SeiNode, spec *seiv1alpha1.SidecarTLSSpec) *seiv1alpha1.SeiNode {
	if node.Spec.Sidecar == nil {
		node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{}
	}
	node.Spec.Sidecar.TLS = spec
	return node
}

// --- sidecarTLSDrift matrix ---

func TestSidecarTLSDrift_SpecNil_ReturnsFalse(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	// spec.sidecar.tls == nil, status.currentSidecarTLS == nil
	g.Expect(sidecarTLSDrift(node)).To(BeFalse())
}

func TestSidecarTLSDrift_DisableCase_ReturnsFalse(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	// Deferred per LLD §6: spec=nil, current=set should NOT trigger drift.
	node.Status.CurrentSidecarTLS = tlsStatus()
	g.Expect(sidecarTLSDrift(node)).To(BeFalse(),
		"disable path is deferred — spec.sidecar.tls=nil must not trigger drift")
}

func TestSidecarTLSDrift_EnableCase_ReturnsTrue(t *testing.T) {
	g := NewWithT(t)
	node := withTLSSpec(runningFullNode(), tlsSpecOf(testIssuer, testKind))
	// status.currentSidecarTLS == nil — initial enable
	g.Expect(sidecarTLSDrift(node)).To(BeTrue())
}

func TestSidecarTLSDrift_IssuerSwap_ReturnsTrue(t *testing.T) {
	g := NewWithT(t)
	node := withTLSSpec(runningFullNode(), tlsSpecOf(testIssuerAlt, testKind))
	node.Status.CurrentSidecarTLS = tlsStatus()
	g.Expect(sidecarTLSDrift(node)).To(BeTrue())
}

func TestSidecarTLSDrift_KindSwap_ReturnsTrue(t *testing.T) {
	g := NewWithT(t)
	node := withTLSSpec(runningFullNode(), tlsSpecOf(testIssuer, "Issuer"))
	node.Status.CurrentSidecarTLS = tlsStatus()
	g.Expect(sidecarTLSDrift(node)).To(BeTrue())
}

func TestSidecarTLSDrift_Converged_ReturnsFalse(t *testing.T) {
	g := NewWithT(t)
	node := withTLSSpec(runningFullNode(), tlsSpecOf(testIssuer, testKind))
	node.Status.CurrentSidecarTLS = tlsStatus()
	g.Expect(sidecarTLSDrift(node)).To(BeFalse())
}

// --- buildRunningPlan with TLS drift ---

func TestBuildRunningPlan_TLSDriftOnly_ReturnsTLSTogglePlan(t *testing.T) {
	g := NewWithT(t)
	node := withTLSSpec(runningFullNode(), tlsSpecOf(testIssuer, testKind))

	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive))
	g.Expect(plan.TargetPhase).To(Equal(seiv1alpha1.PhaseRunning))
	g.Expect(string(plan.FailedPhase)).To(BeEmpty())

	g.Expect(planTaskTypes(plan)).To(Equal([]string{
		task.TaskTypeApplySidecarCert,
		task.TaskTypeWaitForSidecarTLSSecret,
		task.TaskTypeApplyRBACProxyConfig,
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveSidecarTLS,
		TaskMarkReady,
	}))

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("TLSToggleStarted"))
	g.Expect(cond.Message).To(ContainSubstring("sidecar.tls drift detected"))
}

func TestBuildRunningPlan_ImageAndTLSDrift_SingleCombinedPlan(t *testing.T) {
	g := NewWithT(t)
	node := withTLSSpec(runningFullNode(), tlsSpecOf(testIssuer, testKind))
	node.Spec.Image = testImageV2 // also bump image

	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	g.Expect(planTaskTypes(plan)).To(Equal([]string{
		task.TaskTypeApplySidecarCert,
		task.TaskTypeWaitForSidecarTLSSecret,
		task.TaskTypeApplyRBACProxyConfig,
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveImage,
		task.TaskTypeObserveSidecarTLS,
		TaskMarkReady,
	}), "both observers run in one plan; one pod cycle covers both drifts")

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Reason).To(Equal("UpdateAndTLSToggleStarted"))
	g.Expect(cond.Message).To(ContainSubstring("image drift detected"))
	g.Expect(cond.Message).To(ContainSubstring("sidecar.tls drift detected"))
}

func TestBuildRunningPlan_ImageDrift_NoTLSPreTasks(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2 // image drift only, no TLS spec

	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	// Existing image-only behavior preserved.
	g.Expect(planTaskTypes(plan)).To(Equal([]string{
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveImage,
		TaskMarkReady,
	}))

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond.Reason).To(Equal("UpdateStarted"))
}

func TestBuildRunningPlan_TLSConverged_NoPlan(t *testing.T) {
	g := NewWithT(t)
	node := withTLSSpec(runningFullNode(), tlsSpecOf(testIssuer, testKind))
	node.Status.CurrentSidecarTLS = tlsStatus()

	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil(), "no drift → no plan")
}

// --- nodeUpdateReason discrimination ---

func TestNodeUpdateReason_AllCombinations(t *testing.T) {
	g := NewWithT(t)
	g.Expect(nodeUpdateReason(true, false)).To(Equal("UpdateStarted"))
	g.Expect(nodeUpdateReason(false, true)).To(Equal("TLSToggleStarted"))
	g.Expect(nodeUpdateReason(true, true)).To(Equal("UpdateAndTLSToggleStarted"))
}

// --- classifyPlan labels ---

func TestClassifyPlan_TLSToggle(t *testing.T) {
	g := NewWithT(t)
	node := withTLSSpec(runningFullNode(), tlsSpecOf(testIssuer, testKind))
	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(classifyPlan(plan)).To(Equal("tls-toggle"))
}

func TestClassifyPlan_NodeUpdateWithTLS(t *testing.T) {
	g := NewWithT(t)
	node := withTLSSpec(runningFullNode(), tlsSpecOf(testIssuer, testKind))
	node.Spec.Image = testImageV2
	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(classifyPlan(plan)).To(Equal("node-update+tls"))
}

func TestClassifyPlan_NodeUpdateImageOnly(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2
	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(classifyPlan(plan)).To(Equal("node-update"),
		"image-only plan keeps the existing label so dashboards do not break")
}

// --- idempotency on plan rebuild ---

func TestBuildRunningPlan_TLSDrift_UniqueIDs(t *testing.T) {
	g := NewWithT(t)
	node := withTLSSpec(runningFullNode(), tlsSpecOf(testIssuer, testKind))

	plan1, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	plan2, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(plan1.ID).NotTo(Equal(plan2.ID))
	seen := map[string]bool{}
	for _, tsk := range plan1.Tasks {
		g.Expect(seen[tsk.ID]).To(BeFalse(), "duplicate task ID: %s", tsk.ID)
		seen[tsk.ID] = true
	}
}

// --- ResolvePlan condition lifecycle on TLS toggle ---

func TestResolvePlan_TLSToggle_SetsAndClearsCondition(t *testing.T) {
	g := NewWithT(t)
	node := withTLSSpec(runningFullNode(), tlsSpecOf(testIssuer, testKind))

	err := (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).NotTo(BeNil())

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("TLSToggleStarted"))

	// Simulate plan completion and observer stamping CurrentSidecarTLS.
	node.Status.Plan.Phase = seiv1alpha1.TaskPlanComplete
	node.Status.CurrentSidecarTLS = tlsStatus()

	err = (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).To(BeNil(), "completed plan should be cleared")

	cond = meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("UpdateComplete"))
}
