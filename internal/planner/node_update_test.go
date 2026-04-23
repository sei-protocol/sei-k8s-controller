package planner

import (
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const testImageV2 = "sei:v2.0.0"

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

// --- buildRunningPlan tests ---

func TestBuildRunningPlan_NoDrift_ReturnsNil(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	// spec.image == status.currentImage — no drift
	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil(), "no plan should be built when there is no image drift")
}

func TestBuildRunningPlan_ImageDrift_ReturnsNodeUpdatePlan(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2 // drift: spec != status.currentImage

	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil(), "plan should be built for image drift")

	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive))
	g.Expect(plan.TargetPhase).To(Equal(seiv1alpha1.PhaseRunning))
	// FailedPhase should be empty — failure retries rather than transitioning out of Running.
	g.Expect(string(plan.FailedPhase)).To(BeEmpty())

	got := planTaskTypes(plan)
	want := []string{
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
		task.TaskTypeObserveImage,
		TaskMarkReady,
	}
	g.Expect(got).To(Equal(want), "NodeUpdate plan should have exactly these tasks in order")

	// All tasks should start Pending with non-empty IDs and params.
	for _, pt := range plan.Tasks {
		g.Expect(pt.Status).To(Equal(seiv1alpha1.TaskPending), "task %s should start Pending", pt.Type)
		g.Expect(pt.ID).NotTo(BeEmpty(), "task %s should have an ID", pt.Type)
		g.Expect(pt.Params).NotTo(BeNil(), "task %s should have params", pt.Type)
	}
}

// --- ResolvePlan condition tests ---

func TestResolvePlan_NodeUpdate_SetsCondition(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2 // drift triggers NodeUpdate plan

	err := ResolvePlan(t.Context(), node, nil)
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

	// Build a NodeUpdate plan and simulate completion.
	err := ResolvePlan(t.Context(), node, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan).NotTo(BeNil())

	// Verify the condition was set.
	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))

	// Mark the plan completed.
	node.Status.Plan.Phase = seiv1alpha1.TaskPlanComplete
	// Also converge currentImage so a new plan is not built.
	node.Status.CurrentImage = testImageV2

	// ResolvePlan should clear the completed plan and the condition.
	err = ResolvePlan(t.Context(), node, nil)
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

	// Build a NodeUpdate plan and simulate failure.
	err := ResolvePlan(t.Context(), node, nil)
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

	// ResolvePlan should clear the failed plan. Since drift still exists,
	// it immediately builds a new NodeUpdate plan and sets the condition
	// back to True. This is correct — automatic retry on failure.
	err = ResolvePlan(t.Context(), node, nil)
	g.Expect(err).NotTo(HaveOccurred())

	// A new plan was built because drift still exists.
	g.Expect(node.Status.Plan).NotTo(BeNil(), "new plan should be built because drift persists")
	g.Expect(node.Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive))

	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue), "condition should be True because a new retry plan was built")
}

func TestResolvePlan_ResumesActivePlan(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2 // drift exists

	existingPlan := &seiv1alpha1.TaskPlan{
		ID:    "existing-plan-123",
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{
			{Type: task.TaskTypeApplyStatefulSet, ID: "task-1", Status: seiv1alpha1.TaskPending},
		},
	}
	node.Status.Plan = existingPlan

	err := ResolvePlan(t.Context(), node, nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.Plan.ID).To(Equal("existing-plan-123"),
		"active plan should be resumed, not replaced")
}

// --- Additional edge cases ---

func TestResolvePlan_CompletedNonUpdatePlan_DoesNotClearCondition(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	// Manually set the condition (as if a previous NodeUpdate plan set it).
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:   seiv1alpha1.ConditionNodeUpdateInProgress,
		Status: metav1.ConditionFalse,
		Reason: "UpdateComplete",
	})

	// Create a completed plan without observe-image (not a NodeUpdate plan).
	node.Status.Plan = &seiv1alpha1.TaskPlan{
		ID:    "non-update-plan",
		Phase: seiv1alpha1.TaskPlanComplete,
		Tasks: []seiv1alpha1.PlannedTask{
			{Type: task.TaskTypeApplyStatefulSet, ID: "t-1", Status: seiv1alpha1.TaskComplete},
		},
	}

	err := ResolvePlan(t.Context(), node, nil)
	g.Expect(err).NotTo(HaveOccurred())

	// The condition should remain unchanged (already False from before).
	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal("UpdateComplete"),
		"reason should not be overwritten by a non-update plan completion")
}

func TestBuildRunningPlan_UniqueIDs(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2

	plan1, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())

	plan2, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(plan1.ID).NotTo(Equal(plan2.ID), "separate plan builds should have unique IDs")

	// Task IDs within a single plan should all be unique.
	seen := map[string]bool{}
	for _, tsk := range plan1.Tasks {
		g.Expect(seen[tsk.ID]).To(BeFalse(), "duplicate task ID: %s", tsk.ID)
		seen[tsk.ID] = true
	}
}

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
	// Should be a no-op — no panic.
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

// --- sidecar mark-ready re-apply tests ---

func setSidecarReady(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason string) {
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:    seiv1alpha1.ConditionSidecarReady,
		Status:  status,
		Reason:  reason,
		Message: "test",
	})
}

func TestBuildRunningPlan_SidecarReady_NoPlan(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	setSidecarReady(node, metav1.ConditionTrue, "Ready")

	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil())
}

func TestBuildRunningPlan_SidecarNotReady_ReturnsMarkReadyPlan(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	setSidecarReady(node, metav1.ConditionFalse, "NotReady")

	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive))
	g.Expect(plan.TargetPhase).To(Equal(seiv1alpha1.PhaseRunning))
	g.Expect(string(plan.FailedPhase)).To(BeEmpty())
	g.Expect(planTaskTypes(plan)).To(Equal([]string{TaskMarkReady}))
}

func TestBuildRunningPlan_SidecarUnknown_NoPlan(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	setSidecarReady(node, metav1.ConditionUnknown, "Unreachable")

	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).To(BeNil(), "Unknown should not trigger a plan — re-probe next tick")
}

func TestBuildRunningPlan_ImageDriftWinsOverSidecar(t *testing.T) {
	g := NewWithT(t)
	node := runningFullNode()
	node.Spec.Image = testImageV2 // image drift
	setSidecarReady(node, metav1.ConditionFalse, "NotReady")

	plan, err := buildRunningPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan).NotTo(BeNil())
	// Image update plan ends with MarkReady, which also resolves the sidecar.
	g.Expect(len(plan.Tasks)).To(Equal(4), "should be full node-update plan, not one-task mark-ready")
	g.Expect(planTaskTypes(plan)).To(Equal([]string{
		task.TaskTypeApplyStatefulSet,
		task.TaskTypeApplyService,
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

	// missing condition
	node := runningFullNode()
	g.Expect(sidecarNeedsReapproval(node)).To(BeFalse())

	// True
	setSidecarReady(node, metav1.ConditionTrue, "Ready")
	g.Expect(sidecarNeedsReapproval(node)).To(BeFalse())

	// Unknown
	setSidecarReady(node, metav1.ConditionUnknown, "Unreachable")
	g.Expect(sidecarNeedsReapproval(node)).To(BeFalse())

	// False + wrong reason
	setSidecarReady(node, metav1.ConditionFalse, "SomethingElse")
	g.Expect(sidecarNeedsReapproval(node)).To(BeFalse())

	// False + NotReady
	setSidecarReady(node, metav1.ConditionFalse, "NotReady")
	g.Expect(sidecarNeedsReapproval(node)).To(BeTrue())
}
