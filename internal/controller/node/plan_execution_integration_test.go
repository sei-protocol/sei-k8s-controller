package node

import (
	"context"
	"testing"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// driveTask simulates one task round: submits on first reconcile,
// then sets a completed result and reconciles again to advance.
func driveTask(
	t *testing.T,
	g Gomega,
	r *SeiNodeReconciler,
	mock *mockSidecarClient,
	fetch func() *seiv1alpha1.SeiNode,
	taskType string,
) {
	t.Helper()

	mock.submitted = nil
	taskID := uuid.New()
	mock.submitID = taskID
	node := fetch()
	planner, err := PlannerForNode(node)
	g.Expect(err).NotTo(HaveOccurred())
	sc := r.buildSidecarClient(node)
	_, err = r.executePlan(context.Background(), node, node.Status.InitPlan, planner, sc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(taskType))

	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskID: completedResult(taskID, taskType, nil),
	}
	node = fetch()
	planner, _ = PlannerForNode(node)
	sc = r.buildSidecarClient(node)
	_, err = r.executePlan(context.Background(), node, node.Status.InitPlan, planner, sc)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestIntegrationFullProgressionSnapshotMode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	planner, _ := PlannerForNode(node)
	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetch := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// Create plan via reconcilePending
	_, err := r.reconcilePending(ctx, node, planner)
	g.Expect(err).NotTo(HaveOccurred())
	node = fetch()
	g.Expect(node.Status.InitPlan).NotTo(BeNil())
	g.Expect(node.Status.Phase).To(Equal(seiv1alpha1.PhasePreInitializing))

	// Drive the first task (snapshot-restore submit)
	taskID := uuid.New()
	mock.submitID = taskID
	planner, _ = PlannerForNode(node)
	sc := r.buildSidecarClient(node)
	_, err = r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted[0].Type).To(Equal(taskSnapshotRestore))

	// Complete snapshot-restore.
	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskID: completedResult(taskID, taskSnapshotRestore, nil),
	}
	updated := fetch()
	planner, _ = PlannerForNode(updated)
	sc = r.buildSidecarClient(updated)
	_, err = r.executePlan(ctx, updated, updated.Status.InitPlan, planner, sc)
	g.Expect(err).NotTo(HaveOccurred())

	// Drive remaining tasks: config-apply, state-sync patch, validate, ready.
	driveTask(t, g, r, mock, fetch, taskConfigApply)
	driveTask(t, g, r, mock, fetch, taskConfigureStateSync)
	driveTask(t, g, r, mock, fetch, taskConfigValidate)
	driveTask(t, g, r, mock, fetch, taskMarkReady)

	// Final plan completion reconcile
	updated = fetch()
	planner, _ = PlannerForNode(updated)
	sc = r.buildSidecarClient(updated)
	_, err = r.executePlan(ctx, updated, updated.Status.InitPlan, planner, sc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fetch().Status.InitPlan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
}

func TestIntegrationFullProgressionGenesisMode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := genesisNode()
	planner, _ := PlannerForNode(node)
	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetch := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// Create plan via reconcilePending
	_, err := r.reconcilePending(ctx, node, planner)
	g.Expect(err).NotTo(HaveOccurred())
	node = fetch()
	g.Expect(node.Status.InitPlan).NotTo(BeNil())

	// Drive the first task (config-apply submit)
	taskID := uuid.New()
	mock.submitID = taskID
	planner, _ = PlannerForNode(node)
	sc := r.buildSidecarClient(node)
	_, err = r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted[0].Type).To(Equal(taskConfigApply))

	// Complete config-apply.
	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskID: completedResult(taskID, taskConfigApply, nil),
	}
	updated := fetch()
	planner, _ = PlannerForNode(updated)
	sc = r.buildSidecarClient(updated)
	_, err = r.executePlan(ctx, updated, updated.Status.InitPlan, planner, sc)
	g.Expect(err).NotTo(HaveOccurred())

	// Drive config-validate and mark-ready.
	driveTask(t, g, r, mock, fetch, taskConfigValidate)
	driveTask(t, g, r, mock, fetch, taskMarkReady)

	// Complete the plan.
	updated = fetch()
	planner, _ = PlannerForNode(updated)
	sc = r.buildSidecarClient(updated)
	_, err = r.executePlan(ctx, updated, updated.Status.InitPlan, planner, sc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fetch().Status.InitPlan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
}

func TestIntegrationTaskFailure_FailsPlan(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	planner, _ := PlannerForNode(node)
	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetch := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// Create plan via reconcilePending
	_, err := r.reconcilePending(ctx, node, planner)
	g.Expect(err).NotTo(HaveOccurred())
	node = fetch()
	g.Expect(node.Status.InitPlan).NotTo(BeNil())

	// Drive the first task (snapshot-restore submit)
	taskID := uuid.New()
	mock.submitID = taskID
	planner, _ = PlannerForNode(node)
	sc := r.buildSidecarClient(node)
	_, err = r.executePlan(ctx, node, node.Status.InitPlan, planner, sc)
	g.Expect(err).NotTo(HaveOccurred())

	// Fail the task.
	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskID: completedResult(taskID, taskSnapshotRestore, strPtr("S3 access denied")),
	}
	updated := fetch()
	planner, _ = PlannerForNode(updated)
	sc = r.buildSidecarClient(updated)
	_, err = r.executePlan(ctx, updated, updated.Status.InitPlan, planner, sc)
	g.Expect(err).NotTo(HaveOccurred())

	updated = fetch()
	g.Expect(updated.Status.InitPlan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))
	g.Expect(updated.Status.InitPlan.Tasks[0].Status).To(Equal(seiv1alpha1.PlannedTaskFailed))
	g.Expect(updated.Status.InitPlan.Tasks[0].Error).To(Equal("S3 access denied"))

	// Subsequent reconciles are no-ops.
	mock.submitted = nil
	planner, _ = PlannerForNode(updated)
	sc = r.buildSidecarClient(updated)
	_, err = r.executePlan(ctx, updated, updated.Status.InitPlan, planner, sc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(BeEmpty())
}
