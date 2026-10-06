package planner

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

// Spec 010 (maintenance hold): planner-side coverage. Each test names the
// requirement it covers.

const (
	holdImmediate = seiv1alpha1.MaintenanceHoldImmediate
	holdAfterExit = seiv1alpha1.MaintenanceHoldAfterExit
)

func heldNode(want, have seiv1alpha1.MaintenanceHold) *seiv1alpha1.SeiNode {
	node := withNodeConfig(runningFullNode())
	if want != "" {
		node.Spec.Maintenance = &seiv1alpha1.MaintenanceSpec{Hold: want}
	}
	node.Status.MaintenanceHold = have
	return node
}

func recordedHold(t *testing.T, plan *seiv1alpha1.TaskPlan) seiv1alpha1.MaintenanceHold {
	t.Helper()
	last := plan.Tasks[len(plan.Tasks)-1]
	if last.Type != task.TaskTypeRecordMaintenanceHold {
		t.Fatalf("plan ends with %s, want %s", last.Type, task.TaskTypeRecordMaintenanceHold)
	}
	var p task.RecordMaintenanceHoldParams
	if err := json.Unmarshal(last.Params.Raw, &p); err != nil {
		t.Fatal(err)
	}
	return p.Hold
}

// 010 Req 2.1-2.3, 2.7, 4.1, 4.3 / SC-002: each move between the requested
// hold and the hold in effect builds its own plan, ending with the record.
func TestHoldPlans(t *testing.T) {
	cases := []struct {
		name       string
		want, have seiv1alpha1.MaintenanceHold
		types      []string
	}{
		{"hold now", holdImmediate, "", []string{
			taskTypeMarkNotReady, taskTypeStopSeid, task.TaskTypeRecordMaintenanceHold}},
		{"arm a running seid", holdAfterExit, "", []string{
			taskTypeMarkNotReady, task.TaskTypeRecordMaintenanceHold}},
		{"start a parked seid once", holdAfterExit, holdImmediate, []string{
			task.TaskTypeObserveImage, task.TaskTypeStartSeidOnce, sidecar.TaskTypeAwaitSeidStart,
			taskTypeMarkNotReady, task.TaskTypeRecordMaintenanceHold}},
		{"stop an armed seid now", holdImmediate, holdAfterExit, []string{
			taskTypeMarkNotReady, taskTypeStopSeid, task.TaskTypeRecordMaintenanceHold}},
		{"release a parked seid", "", holdImmediate, []string{
			task.TaskTypeObserveImage, TaskMarkReady, task.TaskTypeRecordMaintenanceHold}},
		{"release an armed seid", "", holdAfterExit, []string{
			task.TaskTypeObserveImage, TaskMarkReady, task.TaskTypeRecordMaintenanceHold}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			node := heldNode(tc.want, tc.have)

			g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
			g.Expect(node.Status.Plan).NotTo(BeNil())
			g.Expect(planTaskTypes(node.Status.Plan)).To(Equal(tc.types))
			g.Expect(recordedHold(t, node.Status.Plan)).To(Equal(tc.want))
			g.Expect(node.Status.Plan.FailedPhase).To(BeEmpty())
		})
	}
}

// 010 Req 2.8: a hold already in effect plans nothing; a pod that lost
// readiness under it is not re-marked ready (Req 2.5).
func TestHoldInEffect_NoPlanNoReapproval(t *testing.T) {
	for _, hold := range []seiv1alpha1.MaintenanceHold{holdImmediate, holdAfterExit} {
		t.Run(string(hold), func(t *testing.T) {
			g := NewWithT(t)
			node := heldNode(hold, hold)
			setSidecarReadyCondition(node, metav1.ConditionFalse, "NotReady", "pod rolled")

			g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
			g.Expect(node.Status.Plan).To(BeNil())
		})
	}
}

// 010 Req 2.5, 2.6: an image change under a hold rolls the pod but leaves it
// parked: the update plan carries no mark-ready.
func TestHold_UpdatePlanCarriesNoMarkReady(t *testing.T) {
	g := NewWithT(t)
	node := heldNode(holdImmediate, holdImmediate)
	node.Spec.Image = testImageV2

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).NotTo(BeNil())
	types := planTaskTypes(node.Status.Plan)
	g.Expect(types).To(ContainElement(task.TaskTypeObserveImage))
	g.Expect(types).NotTo(ContainElement(TaskMarkReady))
}

// 010 Req 3 / SC-004: a reset on a held node parks instead of releasing, and
// the hold in effect becomes Immediate.
func TestHold_ResetParks(t *testing.T) {
	g := NewWithT(t)
	node := heldNode(holdAfterExit, holdAfterExit)
	node.Spec.DataResetGeneration = 1

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	plan := node.Status.Plan
	g.Expect(isDataResetPlan(plan)).To(BeTrue(), "the reset comes before any hold plan")
	g.Expect(planTaskTypes(plan)).NotTo(ContainElement(TaskMarkReady))
	g.Expect(recordedHold(t, plan)).To(Equal(holdImmediate))
}

// 010 Req 4.2: release with a reset pending runs the full reset plan, which
// ends in mark-ready.
func TestRelease_WithResetPending(t *testing.T) {
	g := NewWithT(t)
	node := heldNode("", holdImmediate)
	node.Spec.DataResetGeneration = 1

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	types := planTaskTypes(node.Status.Plan)
	g.Expect(types[len(types)-1]).To(Equal(TaskMarkReady))
}

// 010 Req 2.9 / User Story 5: a node created with a hold initializes without
// starting seid, and reaches Running with the hold in effect.
func TestHold_InitPlanParks(t *testing.T) {
	for _, mode := range staticModes {
		t.Run(mode.name, func(t *testing.T) {
			g := NewWithT(t)
			node := withNodeConfig(pendingNode(mode.configure))
			node.Spec.Maintenance = &seiv1alpha1.MaintenanceSpec{Hold: holdImmediate}

			g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
			g.Expect(node.Status.Plan).NotTo(BeNil())
			g.Expect(planTaskTypes(node.Status.Plan)).NotTo(ContainElement(TaskMarkReady))
			g.Expect(recordedHold(t, node.Status.Plan)).To(Equal(holdImmediate))
			g.Expect(node.Status.Plan.TargetPhase).To(Equal(seiv1alpha1.PhaseRunning))
		})
	}
}

// 010 Req 2.4 / SC-003: a plan built before the hold cannot release seid; the
// next plan is the hold plan.
func TestHold_StaleUpdatePlanCannotReleaseGate(t *testing.T) {
	g := NewWithT(t)
	s := testScheme(t)
	node := withNodeConfig(runningFullNode())
	node.Spec.Image = testImageV2
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	plan := node.Status.Plan
	completeTasksBefore(plan, len(plan.Tasks)-1)

	node.Spec.Maintenance = &seiv1alpha1.MaintenanceSpec{Hold: holdImmediate}

	mock := &mockSidecarClient{}
	_, err := nodeExecutor(fake.NewClientBuilder().WithScheme(s), s, mock).ExecutePlan(context.Background(), node, plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))
	g.Expect(plan.FailedTaskDetail.Error).To(ContainSubstring("maintenance hold"))
	g.Expect(mock.submitted).To(BeEmpty())

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(isMaintenancePlan(node.Status.Plan)).To(BeTrue())
}

// 010 Req 2.3, 2.4: the start-once step passes the guard under a hold, and the
// plan records AfterExit once mark-not-ready closes the gate again.
func TestHold_StartOncePassesGuard(t *testing.T) {
	g := NewWithT(t)
	s := testScheme(t)
	node := heldNode(holdAfterExit, holdImmediate)
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	plan := node.Status.Plan
	completeTasksBefore(plan, 1) // observe-image done

	mock := &mockSidecarClient{}
	_, err := nodeExecutor(fake.NewClientBuilder().WithScheme(s), s, mock).ExecutePlan(context.Background(), node, plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive), "await-seid-start is polled")
	g.Expect(mock.submitted).NotTo(BeEmpty())
	g.Expect(mock.submitted[0].Type).To(Equal(sidecar.TaskTypeMarkReady), "start-seid-once submits mark-ready")
}

// 010 Req 5 / SC-005: the condition follows the requested hold and the hold in
// effect.
func TestResolveMaintenance(t *testing.T) {
	cases := []struct {
		name       string
		nodeConfig bool
		want, have seiv1alpha1.MaintenanceHold
		status     metav1.ConditionStatus
		reason     string
	}{
		{"no nodeConfig", false, "", "", metav1.ConditionFalse, seiv1alpha1.ReasonMaintenanceNotApplicable},
		{"not held", true, "", "", metav1.ConditionFalse, seiv1alpha1.ReasonNotHeld},
		{"hold requested", true, holdImmediate, "", metav1.ConditionTrue, seiv1alpha1.ReasonHoldPending},
		{"held", true, holdImmediate, holdImmediate, metav1.ConditionTrue, seiv1alpha1.ReasonHeld},
		{"armed", true, holdAfterExit, holdAfterExit, metav1.ConditionTrue, seiv1alpha1.ReasonArmed},
		{"start once pending", true, holdAfterExit, holdImmediate, metav1.ConditionTrue, seiv1alpha1.ReasonHoldPending},
		{"release pending", true, "", holdImmediate, metav1.ConditionTrue, seiv1alpha1.ReasonHeld},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			node := heldNode(tc.want, tc.have)
			if !tc.nodeConfig {
				node.Spec.NodeConfig = nil
			}
			ResolveMaintenance(node)
			cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionMaintenanceInProgress)
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond.Status).To(Equal(tc.status))
			g.Expect(cond.Reason).To(Equal(tc.reason))
		})
	}
}

// Review finding: a hold set after the init plan was built must not fail the
// node. The init plan's mark-ready passes, the node reaches Running, and the
// next plan is the hold plan.
func TestHold_SetMidInitDoesNotFailNode(t *testing.T) {
	g := NewWithT(t)
	s := testScheme(t)
	node := withNodeConfig(pendingNode(func(n *seiv1alpha1.SeiNode) { n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{} }))
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	plan := node.Status.Plan
	g.Expect(plan.FailedPhase).To(Equal(seiv1alpha1.PhaseFailed), "an init plan failure is terminal, which is why this matters")
	completeTasksBefore(plan, len(plan.Tasks)-1)

	node.Spec.Maintenance = &seiv1alpha1.MaintenanceSpec{Hold: holdImmediate}

	mock := &mockSidecarClient{}
	_, err := nodeExecutor(fake.NewClientBuilder().WithScheme(s), s, mock).ExecutePlan(context.Background(), node, plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
	g.Expect(node.Status.Phase).To(Equal(seiv1alpha1.PhaseRunning))

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(isMaintenancePlan(node.Status.Plan)).To(BeTrue())
}

// seidroid #595 blocker 1: the operator changes AfterExit back to Immediate
// while the start-once plan runs. The start-once step refuses, nothing starts
// seid, and with the gate still closed the planner has nothing left to do.
func TestHold_StartOnceRefusedAfterFlipToImmediate(t *testing.T) {
	g := NewWithT(t)
	s := testScheme(t)
	node := heldNode(holdAfterExit, holdImmediate)
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	plan := node.Status.Plan
	completeTasksBefore(plan, 1) // observe-image done

	node.Spec.Maintenance.Hold = holdImmediate

	mock := &mockSidecarClient{}
	_, err := nodeExecutor(fake.NewClientBuilder().WithScheme(s), s, mock).ExecutePlan(context.Background(), node, plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))
	g.Expect(plan.FailedTaskDetail.Type).To(Equal(task.TaskTypeStartSeidOnce))
	g.Expect(mock.submitted).To(BeEmpty(), "start-seid-once must not reach the sidecar")

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).To(BeNil(), "the gate never opened, so the Immediate hold still holds")
}

// seidroid #595 blocker 2: a hold in effect whose gate the sidecar reports
// open (a start-once plan failed after mark-ready) is rebuilt, so the gate
// closes again even though the requested and in-effect holds agree.
func TestHold_OpenGateUnderHoldIsClosed(t *testing.T) {
	cases := []struct {
		name       string
		want, have seiv1alpha1.MaintenanceHold
		types      []string
		records    seiv1alpha1.MaintenanceHold
	}{
		{"Immediate in effect", holdImmediate, holdImmediate,
			[]string{taskTypeMarkNotReady, taskTypeStopSeid, task.TaskTypeRecordMaintenanceHold}, holdImmediate},
		{"AfterExit in effect", holdAfterExit, holdAfterExit,
			[]string{taskTypeMarkNotReady, task.TaskTypeRecordMaintenanceHold}, holdAfterExit},
		// seidroid #595 follow-up: a start-once plan failed after opening the
		// gate. The Immediate hold in effect is applied again first; start-once
		// is retried on the next plan, with the gate closed.
		{"start-once failed, gate open", holdAfterExit, holdImmediate,
			[]string{taskTypeMarkNotReady, taskTypeStopSeid, task.TaskTypeRecordMaintenanceHold}, holdImmediate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			node := heldNode(tc.want, tc.have)
			setSidecarReadyCondition(node, metav1.ConditionTrue, "Ready", "sidecar returned 200")

			g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
			g.Expect(node.Status.Plan).NotTo(BeNil())
			g.Expect(planTaskTypes(node.Status.Plan)).To(Equal(tc.types))
			g.Expect(recordedHold(t, node.Status.Plan)).To(Equal(tc.records))
		})
	}
}

// seidroid #595: the condition reads HoldPending, never Held or Armed, while a
// hold plan runs or while the gate is open under a hold.
func TestResolveMaintenance_NotHeldWhileChanging(t *testing.T) {
	g := NewWithT(t)
	node := heldNode(holdAfterExit, holdImmediate)
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	node.Spec.Maintenance.Hold = holdImmediate // want == have, but a plan runs

	ResolveMaintenance(node)
	cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionMaintenanceInProgress)
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonHoldPending))

	node.Status.Plan = nil
	setSidecarReadyCondition(node, metav1.ConditionTrue, "Ready", "sidecar returned 200")
	ResolveMaintenance(node)
	cond = meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionMaintenanceInProgress)
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonHoldPending))
	g.Expect(cond.Message).To(ContainSubstring("gate is open"))
}

// seidroid #595 nit: a held init plan keeps its "init" metric label.
func TestClassifyPlan_HeldInitIsInit(t *testing.T) {
	g := NewWithT(t)
	node := withNodeConfig(pendingNode(func(n *seiv1alpha1.SeiNode) { n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{} }))
	node.Spec.Maintenance = &seiv1alpha1.MaintenanceSpec{Hold: holdImmediate}
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(classifyPlan(node.Status.Plan)).To(Equal("init"))
}

// 009 Req 5.9, 010 Req 3: a hold that arrives while a reset runs makes the
// guard refuse the reset plan's final mark-ready. The wipe ran and was
// recorded, so the reset reads ResetComplete with the start deferred, and the
// next plan is the hold plan.
func TestHold_ArrivesMidReset(t *testing.T) {
	g := NewWithT(t)
	s := testScheme(t)
	node := heldNode("", "")
	node.Spec.DataResetGeneration = 1
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	plan := node.Status.Plan
	g.Expect(isDataResetPlan(plan)).To(BeTrue())
	completeTasksBefore(plan, 4)

	node.Spec.Maintenance = &seiv1alpha1.MaintenanceSpec{Hold: holdImmediate}

	mock := &mockSidecarClient{}
	_, err := nodeExecutor(fake.NewClientBuilder().WithScheme(s), s, mock).ExecutePlan(context.Background(), node, plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))
	g.Expect(node.Status.DataResetGeneration).To(Equal(int64(1)))

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	cond := dataResetCondition(node)
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonResetComplete))
	g.Expect(cond.Message).To(ContainSubstring("maintenance hold"))
	g.Expect(isMaintenancePlan(node.Status.Plan)).To(BeTrue())
}
