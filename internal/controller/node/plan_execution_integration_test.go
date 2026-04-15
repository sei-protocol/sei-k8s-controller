package node

import (
	"context"
	"testing"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// driveTask submits one task and completes it. For fire-and-forget tasks
// (config-validate, mark-ready, infrastructure tasks), the task completes
// in a single ExecutePlan call. For normal tasks, mock results are set and
// a second call is made.
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

	// Check if already complete (fire-and-forget tasks complete in one call)
	node = fetch()
	for _, pt := range node.Status.Plan.Tasks {
		if pt.ID == taskUUID.String() && pt.Status == seiv1alpha1.TaskComplete {
			return
		}
	}

	// Task is still running — set mock results and drive to completion
	if len(mock.submitted) > 0 {
		mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
			taskUUID: completedResult(taskUUID, taskType, nil),
		}
	}
	node = fetch()
	_, err = r.PlanExecutor.ExecutePlan(context.Background(), node, node.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())
}

// reconcileOnce runs a single Reconcile call for the given node.
func reconcileOnce(t *testing.T, g Gomega, r *SeiNodeReconciler, name, namespace string) {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
	_, err := r.Reconcile(context.Background(), req)
	g.Expect(err).NotTo(HaveOccurred())
}

func TestIntegrationFullProgressionSnapshotMode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetch := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// First Reconcile: ResolvePlan builds the plan and transitions to Initializing.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	node = fetch()
	g.Expect(node.Status.Plan).NotTo(BeNil())
	g.Expect(node.Status.Phase).To(Equal(seiv1alpha1.PhaseInitializing))

	// Drive infrastructure tasks (fire-and-forget, complete in one call each).
	driveTask(t, g, r, mock, fetch, task.TaskTypeEnsureDataPVC)
	driveTask(t, g, r, mock, fetch, task.TaskTypeApplyStatefulSet)
	driveTask(t, g, r, mock, fetch, task.TaskTypeApplyService)

	// Drive sidecar tasks.
	driveTask(t, g, r, mock, fetch, planner.TaskSnapshotRestore)
	driveTask(t, g, r, mock, fetch, planner.TaskConfigureGenesis)
	driveTask(t, g, r, mock, fetch, planner.TaskConfigApply)
	driveTask(t, g, r, mock, fetch, planner.TaskConfigureStateSync)
	driveTask(t, g, r, mock, fetch, planner.TaskConfigValidate)
	driveTask(t, g, r, mock, fetch, planner.TaskMarkReady)

	// Final ExecutePlan marks the plan complete and transitions to Running.
	updated := fetch()
	_, err := r.PlanExecutor.ExecutePlan(ctx, updated, updated.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())
	updated = fetch()
	g.Expect(updated.Status.Phase).To(Equal(seiv1alpha1.PhaseRunning))
}

func TestIntegrationFullProgressionGenesisMode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := genesisNode()
	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetch := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// First Reconcile: ResolvePlan builds the plan and transitions to Initializing.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	node = fetch()
	g.Expect(node.Status.Plan).NotTo(BeNil())

	// Drive infrastructure tasks.
	driveTask(t, g, r, mock, fetch, task.TaskTypeEnsureDataPVC)
	driveTask(t, g, r, mock, fetch, task.TaskTypeApplyStatefulSet)
	driveTask(t, g, r, mock, fetch, task.TaskTypeApplyService)

	// Drive sidecar tasks.
	driveTask(t, g, r, mock, fetch, planner.TaskConfigureGenesis)
	driveTask(t, g, r, mock, fetch, planner.TaskConfigApply)
	driveTask(t, g, r, mock, fetch, planner.TaskConfigValidate)
	driveTask(t, g, r, mock, fetch, planner.TaskMarkReady)

	// Final ExecutePlan marks the plan complete and transitions to Running.
	updated := fetch()
	_, err := r.PlanExecutor.ExecutePlan(ctx, updated, updated.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())
	updated = fetch()
	g.Expect(updated.Status.Phase).To(Equal(seiv1alpha1.PhaseRunning))
}

func TestIntegrationTaskFailure_FailsPlan(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	mock := &mockSidecarClient{}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetch := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// First Reconcile: builds plan and transitions to Initializing.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	node = fetch()
	g.Expect(node.Status.Plan).NotTo(BeNil())

	// Drive infrastructure tasks past the fire-and-forget phase.
	driveTask(t, g, r, mock, fetch, task.TaskTypeEnsureDataPVC)
	driveTask(t, g, r, mock, fetch, task.TaskTypeApplyStatefulSet)
	driveTask(t, g, r, mock, fetch, task.TaskTypeApplyService)

	// Drive snapshot-restore: submit it.
	node = fetch()
	ct := planner.CurrentTask(node.Status.Plan)
	g.Expect(ct).NotTo(BeNil())
	g.Expect(ct.Type).To(Equal(planner.TaskSnapshotRestore))
	taskUUID, err := uuid.Parse(ct.ID)
	g.Expect(err).NotTo(HaveOccurred())

	_, err = r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())

	// Fail the snapshot-restore task.
	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskUUID: completedResult(taskUUID, planner.TaskSnapshotRestore, strPtr("S3 access denied")),
	}
	updated := fetch()
	_, err = r.PlanExecutor.ExecutePlan(ctx, updated, updated.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())

	updated = fetch()
	g.Expect(updated.Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))
	g.Expect(updated.Status.Phase).To(Equal(seiv1alpha1.PhaseFailed))

	// Verify the failed task details.
	failedTask := findPlannedTask(updated.Status.Plan, planner.TaskSnapshotRestore)
	g.Expect(failedTask).NotTo(BeNil())
	g.Expect(failedTask.Status).To(Equal(seiv1alpha1.TaskFailed))
	g.Expect(failedTask.Error).To(Equal("S3 access denied"))

	// Subsequent ExecutePlan is a no-op on failed plans.
	mock.submitted = nil
	_, err = r.PlanExecutor.ExecutePlan(ctx, updated, updated.Status.Plan)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(BeEmpty())
}
