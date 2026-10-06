package planner

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

// Spec 009 Requirement 3 (the start guard), exercised through the executor.

// completeTasksBefore marks every task before index idx Complete, as if the
// plan had advanced that far on earlier reconciles.
func completeTasksBefore(plan *seiv1alpha1.TaskPlan, idx int) {
	for i := range idx {
		plan.Tasks[i].Status = seiv1alpha1.TaskComplete
	}
}

// 009 Req 3.1, 3.2, User Story 4 / SC-004: an update plan built before the
// reset commit reaches mark-ready after the pod rolled onto the new config. The
// guard fails the task, nothing reaches the sidecar, and the next plan is the
// reset plan.
func TestDataReset_StaleUpdatePlanCannotReleaseGate(t *testing.T) {
	g := NewWithT(t)
	s := testScheme(t)
	node := withNodeConfig(runningFullNode())
	node.Spec.Image = testImageV2
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	plan := node.Status.Plan
	g.Expect(plan.Tasks[len(plan.Tasks)-1].Type).To(Equal(TaskMarkReady))
	completeTasksBefore(plan, len(plan.Tasks)-1)

	// The reset commit lands while the update plan waits at mark-ready.
	node.Spec.DataResetGeneration = 1

	mock := &mockSidecarClient{}
	_, err := nodeExecutor(fake.NewClientBuilder().WithScheme(s), s, mock).ExecutePlan(context.Background(), node, plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))
	g.Expect(plan.FailedTaskDetail).NotTo(BeNil())
	g.Expect(plan.FailedTaskDetail.Type).To(Equal(TaskMarkReady))
	g.Expect(plan.FailedTaskDetail.Error).To(ContainSubstring("start guard"))
	g.Expect(mock.submitted).To(BeEmpty(), "the guard refuses before anything reaches the sidecar")

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(isDataResetPlan(node.Status.Plan)).To(BeTrue())
}

// 009 Req 2.2, 2.3: inside the reset plan, record-data-reset stamps the
// counter before mark-ready, so the guard lets the plan's own mark-ready pass.
func TestDataReset_PlanReleasesGateAfterRecording(t *testing.T) {
	g := NewWithT(t)
	s := testScheme(t)
	node := resetPendingNode(1, 0)
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	plan := node.Status.Plan
	completeTasksBefore(plan, 4) // observe-image .. reset-data done

	mock := &mockSidecarClient{}
	_, err := nodeExecutor(fake.NewClientBuilder().WithScheme(s), s, mock).ExecutePlan(context.Background(), node, plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
	g.Expect(node.Status.DataResetGeneration).To(Equal(int64(1)))
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(sidecar.TaskTypeMarkReady))
}

// 009 edge case: the counter rises again while a reset runs. The plan records
// the value it was built for, the guard still sees a pending reset, and the
// planner runs one more reset.
func TestDataReset_CounterRisesDuringReset(t *testing.T) {
	g := NewWithT(t)
	s := testScheme(t)
	node := resetPendingNode(1, 0)
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	plan := node.Status.Plan
	completeTasksBefore(plan, 4)

	node.Spec.DataResetGeneration = 2

	mock := &mockSidecarClient{}
	_, err := nodeExecutor(fake.NewClientBuilder().WithScheme(s), s, mock).ExecutePlan(context.Background(), node, plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(node.Status.DataResetGeneration).To(Equal(int64(1)))
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))
	g.Expect(mock.submitted).To(BeEmpty())

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(isDataResetPlan(node.Status.Plan)).To(BeTrue())
}
