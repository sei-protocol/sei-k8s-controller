package planner

import (
	"context"
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

// Spec 009 (declarative data reset): planner-side coverage. Each test names
// the requirement it covers.

func resetPendingNode(spec, handled int64) *seiv1alpha1.SeiNode {
	node := withNodeConfig(runningFullNode())
	node.Spec.DataResetGeneration = spec
	node.Status.DataResetGeneration = handled
	return node
}

func dataResetCondition(node *seiv1alpha1.SeiNode) *metav1.Condition {
	return meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionDataResetInProgress)
}

// 009 Req 2.1, 2.2, 2.3 / SC-003: the reset plan runs in order and records the
// counter it was built for.
func TestDataReset_PlanOrder(t *testing.T) {
	g := NewWithT(t)
	node := resetPendingNode(3, 1)

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	plan := node.Status.Plan
	g.Expect(plan).NotTo(BeNil())
	g.Expect(planTaskTypes(plan)).To(Equal([]string{
		task.TaskTypeObserveImage,
		taskTypeMarkNotReady,
		taskTypeStopSeid,
		sidecar.TaskTypeResetDataKeepSignState,
		task.TaskTypeRecordDataReset,
		TaskMarkReady,
	}))
	g.Expect(plan.FailedPhase).To(BeEmpty(), "a failed reset retries; it never fails the node")
	g.Expect(plan.TargetPhase).To(Equal(seiv1alpha1.PhaseRunning))

	var rec task.RecordDataResetParams
	g.Expect(json.Unmarshal(plan.Tasks[4].Params.Raw, &rec)).To(Succeed())
	g.Expect(rec.Generation).To(Equal(int64(3)))
	g.Expect(plan.Tasks[3].MaxRetries).To(Equal(dataResetMaxRetries))

	cond := dataResetCondition(node)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonResetRunning))
	g.Expect(cond.Message).To(ContainSubstring("dataResetGeneration=3"))
}

// 009 Req 2.1, Req 3.4 / SC-003: a pending reset comes before an image update
// and before a readiness reapproval.
func TestDataReset_PrecedesUpdateAndReapproval(t *testing.T) {
	g := NewWithT(t)
	node := resetPendingNode(1, 0)
	node.Spec.Image = testImageV2
	setSidecarReadyCondition(node, metav1.ConditionFalse, "NotReady", "sidecar lost readiness")

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(isDataResetPlan(node.Status.Plan)).To(BeTrue())
	g.Expect(meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)).To(BeNil(),
		"no update plan was built, so no update is claimed")
}

// 009 Req 2.4 / SC-006: equal counters plan nothing, across any restart.
func TestDataReset_NoPlanWhenHandled(t *testing.T) {
	g := NewWithT(t)
	node := resetPendingNode(2, 2)

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).To(BeNil())
}

// 009 Req 1.5 / SC-002: a node that is not Running takes the spec value as
// handled, so creating a node with a non-zero counter never wipes it.
func TestDataReset_NotRunningAbsorbsCounter(t *testing.T) {
	g := NewWithT(t)
	node := withNodeConfig(pendingNode(func(n *seiv1alpha1.SeiNode) { n.Spec.FullNode = &seiv1alpha1.FullNodeSpec{} }))
	node.Spec.DataResetGeneration = 4

	ResolveDataReset(node)
	g.Expect(node.Status.DataResetGeneration).To(Equal(int64(4)))
	g.Expect(dataResetCondition(node).Reason).To(Equal(seiv1alpha1.ReasonNoResetRequested))

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(node.Status.Plan).NotTo(BeNil())
	g.Expect(planTaskTypes(node.Status.Plan)).NotTo(ContainElement(sidecar.TaskTypeResetDataKeepSignState))
}

// 009 Req 5 / SC-008: the condition moves Pending -> Running -> Complete and
// names the counter value; NotApplicable without nodeConfig.
func TestDataReset_ConditionTransitions(t *testing.T) {
	g := NewWithT(t)

	plain := runningFullNode()
	ResolveDataReset(plain)
	g.Expect(dataResetCondition(plain).Reason).To(Equal(seiv1alpha1.ReasonDataResetNotApplicable))
	g.Expect(dataResetCondition(plain).Status).To(Equal(metav1.ConditionFalse))

	node := resetPendingNode(0, 0)
	ResolveDataReset(node)
	g.Expect(dataResetCondition(node).Reason).To(Equal(seiv1alpha1.ReasonNoResetRequested))

	node.Spec.DataResetGeneration = 1
	ResolveDataReset(node)
	cond := dataResetCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonResetPending))
	g.Expect(cond.Message).To(ContainSubstring("dataResetGeneration=1"))

	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(dataResetCondition(node).Reason).To(Equal(seiv1alpha1.ReasonResetRunning))
	ResolveDataReset(node)
	g.Expect(dataResetCondition(node).Reason).To(Equal(seiv1alpha1.ReasonResetRunning),
		"the resolver leaves an active reset plan's condition alone")

	// The plan completes: record-data-reset stamped the handled counter.
	node.Status.Plan.Phase = seiv1alpha1.TaskPlanComplete
	node.Status.DataResetGeneration = 1
	ResolveDataReset(node)
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	cond = dataResetCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonResetComplete))
	g.Expect(cond.Message).To(ContainSubstring("dataResetGeneration=1"))

	ResolveDataReset(node)
	g.Expect(dataResetCondition(node).Reason).To(Equal(seiv1alpha1.ReasonResetComplete),
		"ResetComplete stays until the next reset")
}

// 009 Req 2.5, Req 5.5: a failed reset reads ResetFailed with the task error,
// keeps it through the retry, and clears it only on success.
func TestDataReset_FailureStaysReadableUntilSuccess(t *testing.T) {
	g := NewWithT(t)
	node := resetPendingNode(1, 0)
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())

	plan := node.Status.Plan
	plan.Phase = seiv1alpha1.TaskPlanFailed
	plan.FailedTaskDetail = &seiv1alpha1.FailedTaskInfo{Type: taskTypeResetData, Error: "seidb (pid 42) running in the pod"}

	ResolveDataReset(node)
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	cond := dataResetCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonResetFailed))
	g.Expect(cond.Message).To(ContainSubstring("seidb (pid 42)"))
	g.Expect(isDataResetPlan(node.Status.Plan)).To(BeTrue(), "the planner builds the reset plan again")
	g.Expect(node.Status.DataResetGeneration).To(Equal(int64(0)), "a failed reset leaves the handled counter")

	ResolveDataReset(node)
	g.Expect(dataResetCondition(node).Reason).To(Equal(seiv1alpha1.ReasonResetFailed),
		"the retry keeps the last failure readable")

	node.Status.Plan.Phase = seiv1alpha1.TaskPlanComplete
	node.Status.DataResetGeneration = 1
	g.Expect((&NodeResolver{}).ResolvePlan(context.Background(), node)).To(Succeed())
	g.Expect(dataResetCondition(node).Reason).To(Equal(seiv1alpha1.ReasonResetComplete))
}
