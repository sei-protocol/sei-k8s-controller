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
)

// driveTask submits one task and completes it via the full Reconcile pipeline.
// For fire-and-forget tasks (config-validate, mark-ready, infrastructure tasks),
// the task completes in a single reconcile. For normal sidecar tasks, mock
// results are set and a second reconcile is made.
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

	reconcileOnce(t, g, r, node.Name, node.Namespace)

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
	reconcileOnce(t, g, r, node.Name, node.Namespace)
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

	// First Reconcile: builds plan, transitions to Initializing, completes
	// all synchronous infrastructure tasks, and submits the first sidecar task.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	node = fetch()
	g.Expect(node.Status.Plan).NotTo(BeNil())
	g.Expect(node.Status.Phase).To(Equal(seiv1alpha1.PhaseInitializing))

	// Drive sidecar tasks (infrastructure tasks already completed in first reconcile).
	driveTask(t, g, r, mock, fetch, planner.TaskSnapshotRestore)
	driveTask(t, g, r, mock, fetch, planner.TaskConfigureGenesis)
	driveTask(t, g, r, mock, fetch, planner.TaskConfigApply)
	// Completing configure-state-sync also chain-completes the trailing
	// fire-and-forget tasks (config-validate, mark-ready) and the plan,
	// transitioning the node to Running.
	driveTask(t, g, r, mock, fetch, planner.TaskConfigureStateSync)

	updated := fetch()
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

	// First Reconcile: builds plan, transitions to Initializing, completes
	// all synchronous infrastructure tasks, and submits the first sidecar task.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	node = fetch()
	g.Expect(node.Status.Plan).NotTo(BeNil())

	// Drive sidecar tasks (infrastructure tasks already completed in first reconcile).
	driveTask(t, g, r, mock, fetch, planner.TaskConfigureGenesis)
	// Completing config-apply also chain-completes the trailing fire-and-forget
	// tasks (config-validate, mark-ready) and the plan, transitioning to Running.
	driveTask(t, g, r, mock, fetch, planner.TaskConfigApply)

	updated := fetch()
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

	// First Reconcile: builds plan, transitions to Initializing, completes
	// all synchronous infrastructure tasks, and submits the first sidecar task.
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	node = fetch()
	g.Expect(node.Status.Plan).NotTo(BeNil())

	// Drive snapshot-restore: the first reconcile already submitted it.
	node = fetch()
	ct := planner.CurrentTask(node.Status.Plan)
	g.Expect(ct).NotTo(BeNil())
	g.Expect(ct.Type).To(Equal(planner.TaskSnapshotRestore))
	taskUUID, err := uuid.Parse(ct.ID)
	g.Expect(err).NotTo(HaveOccurred())

	reconcileOnce(t, g, r, node.Name, node.Namespace)

	// Fail the snapshot-restore task.
	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskUUID: completedResult(taskUUID, planner.TaskSnapshotRestore, strPtr("S3 access denied")),
	}
	reconcileOnce(t, g, r, node.Name, node.Namespace)

	updated := fetch()
	g.Expect(updated.Status.Plan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))
	g.Expect(updated.Status.Phase).To(Equal(seiv1alpha1.PhaseFailed))

	// Verify the failed task details.
	failedTask := findPlannedTask(updated.Status.Plan, planner.TaskSnapshotRestore)
	g.Expect(failedTask).NotTo(BeNil())
	g.Expect(failedTask.Status).To(Equal(seiv1alpha1.TaskFailed))
	g.Expect(failedTask.Error).To(Equal("S3 access denied"))

	// Subsequent reconcile is a no-op on failed plans.
	mock.submitted = nil
	reconcileOnce(t, g, r, node.Name, node.Namespace)
	g.Expect(mock.submitted).To(BeEmpty())
}
