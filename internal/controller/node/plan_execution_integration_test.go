package node

import (
	"context"
	"testing"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

// driveTask submits one task and completes it. For fire-and-forget tasks
// (config-validate, mark-ready), the task completes in a single ExecutePlan
// call. For normal tasks, mock results are set and a second call is made.
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
	node := fetch()
	ct := planner.CurrentTask(node.Status.Plan)
	g.Expect(ct).NotTo(BeNil(), "expected a current task for %s", taskType)
	g.Expect(ct.Type).To(Equal(taskType), "current task type mismatch")
	taskUUID, err := uuid.Parse(ct.ID)
	g.Expect(err).NotTo(HaveOccurred())

	_, err = r.PlanExecutor.ExecutePlan(context.Background(), node, node.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(taskType))

	// Check if already complete (fire-and-forget tasks complete in one call)
	node = fetch()
	for _, pt := range node.Status.Plan.Tasks {
		if pt.ID == taskUUID.String() && pt.Status == seiv1alpha1.TaskComplete {
			return
		}
	}

	// Task is still running — set mock results and drive to completion
	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskUUID: completedResult(taskUUID, taskType, nil),
	}
	node = fetch()
	_, err = r.PlanExecutor.ExecutePlan(context.Background(), node, node.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestIntegrationFullProgressionSnapshotMode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	p, _ := planner.ForNode(node, testSnapshotRegion)
	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetch := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	_, err := r.reconcilePending(ctx, node, p)
	g.Expect(err).NotTo(HaveOccurred())
	node = fetch()
	g.Expect(node.Status.Plan).NotTo(BeNil())
	g.Expect(node.Status.Phase).To(Equal(seiv1alpha1.PhaseInitializing))

	ct := planner.CurrentTask(node.Status.Plan)
	g.Expect(ct).NotTo(BeNil())
	taskUUID, err := uuid.Parse(ct.ID)
	g.Expect(err).NotTo(HaveOccurred())

	_, err = r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted[0].Type).To(Equal(planner.TaskSnapshotRestore))

	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskUUID: completedResult(taskUUID, planner.TaskSnapshotRestore, nil),
	}
	updated := fetch()
	_, err = r.PlanExecutor.ExecutePlan(ctx, updated, updated.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())

	driveTask(t, g, r, mock, fetch, planner.TaskConfigureGenesis)
	driveTask(t, g, r, mock, fetch, planner.TaskConfigApply)
	driveTask(t, g, r, mock, fetch, planner.TaskConfigureStateSync)
	driveTask(t, g, r, mock, fetch, planner.TaskConfigValidate)
	driveTask(t, g, r, mock, fetch, planner.TaskMarkReady)

	updated = fetch()
	_, err = r.PlanExecutor.ExecutePlan(ctx, updated, updated.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fetch().Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
}

func TestIntegrationFullProgressionGenesisMode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := genesisNode()
	p, _ := planner.ForNode(node, testSnapshotRegion)
	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetch := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	_, err := r.reconcilePending(ctx, node, p)
	g.Expect(err).NotTo(HaveOccurred())
	node = fetch()
	g.Expect(node.Status.Plan).NotTo(BeNil())

	ct := planner.CurrentTask(node.Status.Plan)
	g.Expect(ct).NotTo(BeNil())
	taskUUID, err := uuid.Parse(ct.ID)
	g.Expect(err).NotTo(HaveOccurred())

	_, err = r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted[0].Type).To(Equal(planner.TaskConfigureGenesis))

	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskUUID: completedResult(taskUUID, planner.TaskConfigureGenesis, nil),
	}
	updated := fetch()
	_, err = r.PlanExecutor.ExecutePlan(ctx, updated, updated.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())

	driveTask(t, g, r, mock, fetch, planner.TaskConfigApply)
	driveTask(t, g, r, mock, fetch, planner.TaskConfigValidate)
	driveTask(t, g, r, mock, fetch, planner.TaskMarkReady)

	updated = fetch()
	_, err = r.PlanExecutor.ExecutePlan(ctx, updated, updated.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fetch().Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
}

func TestIntegrationTaskFailure_FailsPlan(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	p, _ := planner.ForNode(node, testSnapshotRegion)
	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetch := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	_, err := r.reconcilePending(ctx, node, p)
	g.Expect(err).NotTo(HaveOccurred())
	node = fetch()
	g.Expect(node.Status.Plan).NotTo(BeNil())

	ct := planner.CurrentTask(node.Status.Plan)
	g.Expect(ct).NotTo(BeNil())
	taskUUID, err := uuid.Parse(ct.ID)
	g.Expect(err).NotTo(HaveOccurred())

	_, err = r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())

	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskUUID: completedResult(taskUUID, planner.TaskSnapshotRestore, strPtr("S3 access denied")),
	}
	updated := fetch()
	_, err = r.PlanExecutor.ExecutePlan(ctx, updated, updated.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())

	updated = fetch()
	g.Expect(updated.Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))
	g.Expect(updated.Status.Plan.Tasks[0].Status).To(Equal(seiv1alpha1.TaskFailed))
	g.Expect(updated.Status.Plan.Tasks[0].Error).To(Equal("S3 access denied"))

	mock.submitted = nil
	_, err = r.PlanExecutor.ExecutePlan(ctx, updated, updated.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(BeEmpty())
}
