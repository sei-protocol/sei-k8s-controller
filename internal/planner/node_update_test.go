package planner

import (
	"encoding/json"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	testImageV2          = "sei:v2.0.0"
	testExternalAddrAtl  = "syncer-0-0-p2p.atlantic-2.harbor.platform.sei.io:26656"
	testSidecarImageV1   = "ghcr.io/sei-protocol/seictl@sha256:1111"
	testSidecarImageV2   = "ghcr.io/sei-protocol/seictl@sha256:2222"
	testSidecarOverrideV = "ghcr.io/sei-protocol/seictl@sha256:3333"
)

func platformWithSidecar(image string) platform.Config {
	return platform.Config{SidecarImage: image}
}

// runningFullNode returns a SeiNode in the Running phase with currentImage matching spec.image.
func runningFullNode() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "full-0", Namespace: "default", Generation: 1},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "atlantic-2",
			Image:    "sei:v1.0.0",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase:        seiv1alpha1.PhaseRunning,
			CurrentImage: "sei:v1.0.0",
		},
	}
}

// planTaskTypes extracts the ordered task type strings from a plan.
func planTaskTypes(plan *seiv1alpha1.TaskPlan) []string {
	types := make([]string, 0, len(plan.Tasks))
	for _, t := range plan.Tasks {
		types = append(types, t.Type)
	}
	return types
}

// configPatchFromPlan finds the config-patch task in a plan and unmarshals
// its params back into a ConfigPatchTask for inspection.
func configPatchFromPlan(t *testing.T, plan *seiv1alpha1.TaskPlan) task.ConfigPatchTask {
	t.Helper()
	g := NewWithT(t)
	for _, pt := range plan.Tasks {
		if pt.Type != TaskConfigPatch {
			continue
		}
		g.Expect(pt.Params).NotTo(BeNil())
		var p task.ConfigPatchTask
		g.Expect(json.Unmarshal(pt.Params.Raw, &p)).To(Succeed())
		return p
	}
	t.Fatal("plan has no config-patch task")
	return task.ConfigPatchTask{}
}

// --- fullNodePlanner running-plan tests ---

func TestFullPlanner_NoDrift_ReturnsNil(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil(), "no plan should be built when there is no image drift")
}

func TestFullPlanner_ImageDrift_UpdateProgression(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2

	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil(), "plan should be built for image drift")

	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive))
	g.Expect(plan.TargetPhase).To(Equal(seiv1alpha1.PhaseRunning))
	// FailedPhase deliberately empty — failure retries on next reconcile.
	g.Expect(string(plan.FailedPhase)).To(BeEmpty())

	want := []string{
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		TaskConfigPatch,
		TaskConfigValidate,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveImage,
		TaskMarkReady,
	}
	g.Expect(planTaskTypes(plan)).To(Equal(want))

	// Tasks start Pending with non-empty IDs and params.
	for _, pt := range plan.Tasks {
		g.Expect(pt.Status).To(Equal(seiv1alpha1.TaskPending), "task %s should start Pending", pt.Type)
		g.Expect(pt.ID).NotTo(BeEmpty(), "task %s should have an ID", pt.Type)
		g.Expect(pt.Params).NotTo(BeNil(), "task %s should have params", pt.Type)
	}
}

// The update plan's config-patch must carry the external-address from
// Spec.ExternalAddress — the publishable-P2P propagation contract.
func TestFullPlanner_ConfigPatchCarriesExternalAddress(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.ExternalAddress = testExternalAddrAtl
	node.Spec.Image = testImageV2

	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	patch := configPatchFromPlan(t, plan)
	g.Expect(patch.Files).To(HaveKey("config.toml"))
	p2p, ok := patch.Files["config.toml"]["p2p"].(map[string]any)
	g.Expect(ok).To(BeTrue(), "config.toml.p2p should be a nested map")
	g.Expect(p2p).To(HaveKeyWithValue("external-address", testExternalAddrAtl))
}

// Two builds of the same spec must produce identical patch payloads
// (plan-construction is deterministic w.r.t. spec).
func TestFullPlanner_ConfigPatchIsDeterministic(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.ExternalAddress = testExternalAddrAtl
	node.Spec.Image = testImageV2

	plan1, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	plan2, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())

	p1 := configPatchFromPlan(t, plan1)
	p2 := configPatchFromPlan(t, plan2)
	g.Expect(p1.Files).To(Equal(p2.Files), "same spec must produce same patch payload")
}

// --- archive and replay planners share the full progression ---

func TestArchivePlanner_ImageDrift_UpdateProgression(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.FullNode = nil
	node.Spec.Archive = &seiv1alpha1.ArchiveSpec{}
	node.Spec.Image = testImageV2

	plan, err := (&archiveNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	g.Expect(planTaskTypes(plan)).To(Equal([]string{
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		TaskConfigPatch,
		TaskConfigValidate,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveImage,
		TaskMarkReady,
	}))
}

// --- sidecar image drift tests ---

// sidecarDriftedNode returns a runningFullNode with CurrentSidecarImage
// populated so the empty-no-drift backfill guard doesn't fire; the test
// then mutates Spec.Sidecar / platform to drive drift.
func sidecarDriftedNode() *seiv1alpha1.SeiNode {
	node := runningFullNode()
	node.Status.CurrentSidecarImage = testSidecarImageV1
	return node
}

// Effective sidecar image = platform default when Spec.Sidecar is nil.
// Bumping the platform default while CurrentSidecarImage stays at v1
// must trigger a drift-routed update plan.
func TestFullPlanner_SidecarDriftFromPlatformDefault_UpdateProgression(t *testing.T) {
	g := NewWithT(t)
	node := sidecarDriftedNode()
	p := platformWithSidecar(testSidecarImageV2)

	plan, err := (&fullNodePlanner{platform: p}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil(), "sidecar drift should trigger update plan")
	g.Expect(planTaskTypes(plan)).To(Equal([]string{
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		TaskConfigPatch,
		TaskConfigValidate,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveImage,
		TaskMarkReady,
	}))
}

// Spec.Sidecar.Image override takes precedence over the platform default.
func TestFullPlanner_SidecarDriftFromOverride_UpdateProgression(t *testing.T) {
	g := NewWithT(t)
	node := sidecarDriftedNode()
	node.Spec.Sidecar = &seiv1alpha1.SidecarConfig{Image: testSidecarOverrideV}
	p := platformWithSidecar(testSidecarImageV1)

	plan, err := (&fullNodePlanner{platform: p}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil(), "sidecar override drift should trigger update plan")
	g.Expect(plan.Tasks).To(HaveLen(7))
}

// Combined drift — single update plan covers both.
func TestFullPlanner_CombinedDrift_SingleUpdatePlan(t *testing.T) {
	g := NewWithT(t)
	node := sidecarDriftedNode()
	node.Spec.Image = testImageV2
	p := platformWithSidecar(testSidecarImageV2)

	plan, err := (&fullNodePlanner{platform: p}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())
	g.Expect(plan.Tasks).To(HaveLen(7), "one plan covers both drifts")

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Message).To(ContainSubstring("seid spec="))
	g.Expect(cond.Message).To(ContainSubstring("sidecar spec="))
}

// Backfill guard: empty CurrentSidecarImage = "not yet observed, no drift."
// Without this, a controller upgrade fleet-rolls every existing SeiNode on
// first reconcile before ObserveImage backfills the field.
func TestFullPlanner_NoCurrentSidecarImage_NoDrift(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	p := platformWithSidecar(testSidecarImageV2)

	plan, err := (&fullNodePlanner{platform: p}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil(), "empty CurrentSidecarImage must NOT trigger drift")
}

// Diagnostic message names which image drifted.
func TestImageDriftMessage_NamesWhichDrifted(t *testing.T) {
	g := NewWithT(t)
	p := platformWithSidecar(testSidecarImageV2)

	seidOnly := sidecarDriftedNode()
	seidOnly.Spec.Image = testImageV2
	g.Expect(imageDriftMessage(seidOnly, platformWithSidecar(testSidecarImageV1))).To(And(
		ContainSubstring("image drift detected"),
		Not(ContainSubstring("sidecar"))))

	sidecarOnly := sidecarDriftedNode()
	g.Expect(imageDriftMessage(sidecarOnly, p)).To(ContainSubstring("sidecar image drift detected"))

	both := sidecarDriftedNode()
	both.Spec.Image = testImageV2
	g.Expect(imageDriftMessage(both, p)).To(And(
		ContainSubstring("seid spec="),
		ContainSubstring("sidecar spec=")))
}

// Replayer shares the full/archive update shape — symmetry test.
func TestReplayerPlanner_ImageDrift_UpdateProgression(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.FullNode = nil
	node.Spec.Replayer = &seiv1alpha1.ReplayerSpec{}
	node.Spec.Image = testImageV2

	plan, err := (&replayerPlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	g.Expect(planTaskTypes(plan)).To(Equal([]string{
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		TaskConfigPatch,
		TaskConfigValidate,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveImage,
		TaskMarkReady,
	}))
}

// Convention: init writes the whole config via TaskConfigApply; a Running
// node's update plan patches via TaskConfigPatch. Never both in one plan.
// Stated as a doc rule on NodePlanner; this test enforces it.
func TestPlannerConvention_InitUsesApply_UpdateUsesPatch(t *testing.T) {
	g := NewWithT(t)

	// Init: empty phase, empty currentImage.
	initNode := runningFullNode()
	initNode.Status.Phase = ""
	initNode.Status.CurrentImage = ""

	initPlan, err := (&fullNodePlanner{}).BuildPlan(initNode)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(initPlan).NotTo(BeNil())
	initTypes := planTaskTypes(initPlan)
	g.Expect(initTypes).To(ContainElement(TaskConfigApply), "init must include config-apply")
	g.Expect(initTypes).NotTo(ContainElement(TaskConfigPatch), "init must NOT include config-patch")

	// Running update: spec.image diverges from status.currentImage.
	updateNode := runningFullNode()
	updateNode.Spec.Image = testImageV2

	updatePlan, err := (&fullNodePlanner{}).BuildPlan(updateNode)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(updatePlan).NotTo(BeNil())
	updateTypes := planTaskTypes(updatePlan)
	g.Expect(updateTypes).To(ContainElement(TaskConfigPatch), "update must include config-patch")
	g.Expect(updateTypes).NotTo(ContainElement(TaskConfigApply), "update must NOT include config-apply")
}

// Validator's update progression prepends the three key-validation gates
// so a missing/malformed secret aborts before any STS mutation.
func TestValidatorPlanner_ImageDrift_PrependsValidationGates(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.FullNode = nil
	node.Spec.Validator = &seiv1alpha1.ValidatorSpec{}
	node.Spec.Image = testImageV2

	plan, err := (&validatorPlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())

	g.Expect(planTaskTypes(plan)).To(Equal([]string{
		task.TaskTypeValidateSigningKey,
		task.TaskTypeValidateNodeKey,
		task.TaskTypeValidateOperatorKeyring,
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		TaskConfigPatch,
		TaskConfigValidate,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveImage,
		TaskMarkReady,
	}))
}

// --- sidecar mark-ready re-apply ---

func setSidecarReady(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason string) {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:    seiv1alpha1.ConditionSidecarReady,
		Status:  status,
		Reason:  reason,
		Message: "test",
	})
}

func TestFullPlanner_SidecarReady_NoPlan(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	setSidecarReady(node, metav1.ConditionTrue, "Ready")

	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil())
}

func TestFullPlanner_SidecarNotReady_ReturnsMarkReadyPlan(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	setSidecarReady(node, metav1.ConditionFalse, "NotReady")

	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive))
	g.Expect(plan.TargetPhase).To(Equal(seiv1alpha1.PhaseRunning))
	g.Expect(string(plan.FailedPhase)).To(BeEmpty())
	g.Expect(planTaskTypes(plan)).To(Equal([]string{TaskMarkReady}))
}

func TestFullPlanner_SidecarUnknown_NoPlan(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	setSidecarReady(node, metav1.ConditionUnknown, "Unreachable")

	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil(), "Unknown should not trigger a plan — re-probe next tick")
}

func TestFullPlanner_ImageDriftWinsOverSidecar(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2
	setSidecarReady(node, metav1.ConditionFalse, "NotReady")

	plan, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())
	g.Expect(plan.Tasks).To(HaveLen(7), "should be full update plan, not one-task mark-ready")
	g.Expect(planTaskTypes(plan)).To(Equal([]string{
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		TaskConfigPatch,
		TaskConfigValidate,
		task.TaskTypeReplacePod,
		task.TaskTypeObserveImage,
		TaskMarkReady,
	}))
}

func TestBuildMarkReadyPlan_FreshIDEveryCall(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()

	p1, err := buildMarkReadyPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	p2, err := buildMarkReadyPlan(node)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(p1.ID).NotTo(Equal(p2.ID))
	g.Expect(p1.Tasks[0].ID).NotTo(Equal(p2.Tasks[0].ID))
}

func TestSidecarNeedsReapproval(t *testing.T) {
	g := NewWithT(t)

	node := runningFullNode()
	g.Expect(sidecarNeedsReapproval(node)).To(BeFalse())

	setSidecarReady(node, metav1.ConditionTrue, "Ready")
	g.Expect(sidecarNeedsReapproval(node)).To(BeFalse())

	setSidecarReady(node, metav1.ConditionUnknown, "Unreachable")
	g.Expect(sidecarNeedsReapproval(node)).To(BeFalse())

	setSidecarReady(node, metav1.ConditionFalse, "SomethingElse")
	g.Expect(sidecarNeedsReapproval(node)).To(BeFalse())

	setSidecarReady(node, metav1.ConditionFalse, "NotReady")
	g.Expect(sidecarNeedsReapproval(node)).To(BeTrue())
}

func TestBuildRunningPlan_UniqueIDs(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2

	plan1, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	plan2, err := (&fullNodePlanner{}).BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(plan1.ID).NotTo(Equal(plan2.ID), "separate plan builds should have unique IDs")

	seen := map[string]bool{}
	for _, tsk := range plan1.Tasks {
		g.Expect(seen[tsk.ID]).To(BeFalse(), "duplicate task ID: %s", tsk.ID)
		seen[tsk.ID] = true
	}
}

// --- ResolvePlan condition tests ---

func TestResolvePlan_NodeUpdate_SetsCondition(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2

	err := (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).NotTo(BeNil(), "plan should be created")

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil(), "NodeUpdateInProgress condition should be set")
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal("UpdateStarted"))
	g.Expect(cond.Message).To(ContainSubstring("image drift detected"))
}

func TestResolvePlan_CompletedPlan_ClearsCondition(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2

	err := (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).NotTo(BeNil())

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

	node.Status.Plan.Phase = seiv1alpha1.TaskPlanComplete
	node.Status.CurrentImage = testImageV2

	err = (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())

	cond = meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil(), "condition should still exist but be False")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("UpdateComplete"))
	g.Expect(node.Status.Plan).To(BeNil(), "completed plan should be cleared")
}

func TestResolvePlan_FailedPlan_ClearsCondition(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2

	err := (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).NotTo(BeNil())

	failIdx := 0
	node.Status.Plan.Phase = seiv1alpha1.TaskPlanFailed
	node.Status.Plan.FailedTaskIndex = &failIdx
	node.Status.Plan.FailedTaskDetail = &seiv1alpha1.FailedTaskInfo{
		Type:  task.TaskTypeApplyStatefulSet,
		ID:    node.Status.Plan.Tasks[0].ID,
		Error: "apply error",
	}

	err = (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(node.Status.Plan).NotTo(BeNil(), "new plan should be built because drift persists")
	g.Expect(node.Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive))

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "condition should be True because a new retry plan was built")
}

func TestResolvePlan_ResumesActivePlan(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2

	existingPlan := &seiv1alpha1.TaskPlan{
		ID:    "existing-plan-123",
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{
			{Type: task.TaskTypeApplyStatefulSet, ID: "task-1", Status: seiv1alpha1.TaskPending},
		},
	}
	node.Status.Plan = existingPlan

	err := (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan.ID).To(Equal("existing-plan-123"),
		"active plan should be resumed, not replaced")
}

func TestResolvePlan_CompletedNonUpdatePlan_DoesNotClearCondition(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:   seiv1alpha1.ConditionNodeUpdateInProgress,
		Status: metav1.ConditionFalse,
		Reason: "UpdateComplete",
	})

	node.Status.Plan = &seiv1alpha1.TaskPlan{
		ID:    "non-update-plan",
		Phase: seiv1alpha1.TaskPlanComplete,
		Tasks: []seiv1alpha1.PlannedTask{
			{Type: task.TaskTypeApplyStatefulSet, ID: "t-1", Status: seiv1alpha1.TaskComplete},
		},
	}

	err := (&NodeResolver{}).ResolvePlan(t.Context(), node)
	g.Expect(err).NotTo(HaveOccurred())

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("UpdateComplete"),
		"reason should not be overwritten by a non-update plan completion")
}

// --- handleTerminalPlan tests (unchanged) ---

func TestHandleTerminalPlan_CompletedWithUpdateCondition(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:   seiv1alpha1.ConditionNodeUpdateInProgress,
		Status: metav1.ConditionTrue,
		Reason: "UpdateStarted",
	})
	node.Status.Plan = &seiv1alpha1.TaskPlan{
		ID:    "completed-plan",
		Phase: seiv1alpha1.TaskPlanComplete,
	}

	handleTerminalPlan(t.Context(), node)

	g.Expect(node.Status.Plan).To(BeNil())
	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("UpdateComplete"))
}

func TestHandleTerminalPlan_FailedWithUpdateCondition(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:   seiv1alpha1.ConditionNodeUpdateInProgress,
		Status: metav1.ConditionTrue,
		Reason: "UpdateStarted",
	})
	node.Status.Plan = &seiv1alpha1.TaskPlan{
		ID:    "failed-plan",
		Phase: seiv1alpha1.TaskPlanFailed,
		FailedTaskDetail: &seiv1alpha1.FailedTaskInfo{
			Type:  task.TaskTypeObserveImage,
			Error: "timeout waiting for rollout",
		},
	}

	handleTerminalPlan(t.Context(), node)

	g.Expect(node.Status.Plan).To(BeNil())
	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("UpdateFailed"))
	g.Expect(cond.Message).To(ContainSubstring("timeout waiting for rollout"))
}

func TestHandleTerminalPlan_NilPlan(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.Plan = nil
	handleTerminalPlan(t.Context(), node)
	g.Expect(node.Status.Plan).To(BeNil())
}

func TestHandleTerminalPlan_ActivePlan_NoOp(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Status.Plan = &seiv1alpha1.TaskPlan{
		ID:    "active-plan",
		Phase: seiv1alpha1.TaskPlanActive,
	}
	handleTerminalPlan(t.Context(), node)
	g.Expect(node.Status.Plan).NotTo(BeNil(), "active plan should not be cleared")
	g.Expect(node.Status.Plan.ID).To(Equal("active-plan"))
}

func TestPlanFailureMessage_WithDetail(t *testing.T) {
	g := NewWithT(t)
	plan := &seiv1alpha1.TaskPlan{
		FailedTaskDetail: &seiv1alpha1.FailedTaskInfo{
			Type:  task.TaskTypeObserveImage,
			Error: "StatefulSet not ready",
		},
	}
	g.Expect(planFailureMessage(plan)).To(Equal(
		fmt.Sprintf("task %s: StatefulSet not ready", task.TaskTypeObserveImage)))
}

func TestPlanFailureMessage_NoDetail(t *testing.T) {
	g := NewWithT(t)
	plan := &seiv1alpha1.TaskPlan{}
	g.Expect(planFailureMessage(plan)).To(Equal("unknown"))
}
