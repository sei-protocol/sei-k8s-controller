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
	_, err := r.reconcileSidecarProgression(context.Background(), fetch())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskType))

	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskID: completedResult(taskID, taskType, nil),
	}
	_, err = r.reconcileSidecarProgression(context.Background(), fetch())
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

	// First reconcile creates the plan and submits snapshot-restore.
	taskID := uuid.New()
	mock.submitID = taskID
	_, err := r.reconcileSidecarProgression(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fetch().Status.InitPlan).NotTo(BeNil())
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskSnapshotRestore))

	// Complete snapshot-restore.
	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskID: completedResult(taskID, taskSnapshotRestore, nil),
	}
	_, err = r.reconcileSidecarProgression(ctx, fetch())
	g.Expect(err).NotTo(HaveOccurred())

	// Drive remaining tasks: config-apply, state-sync patch, validate, ready.
	driveTask(t, g, r, mock, fetch, taskConfigApply)
	driveTask(t, g, r, mock, fetch, taskConfigureStateSync)
	driveTask(t, g, r, mock, fetch, taskConfigValidate)
	driveTask(t, g, r, mock, fetch, taskMarkReady)

	// One more reconcile to transition from "all tasks done" to plan Complete.
	_, err = r.reconcileSidecarProgression(ctx, fetch())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fetch().Status.InitPlan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
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

	// First reconcile creates plan and submits config-apply.
	taskID := uuid.New()
	mock.submitID = taskID
	_, err := r.reconcileSidecarProgression(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskConfigApply))

	// Complete config-apply.
	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskID: completedResult(taskID, taskConfigApply, nil),
	}
	_, err = r.reconcileSidecarProgression(ctx, fetch())
	g.Expect(err).NotTo(HaveOccurred())

	// Drive config-validate and mark-ready.
	driveTask(t, g, r, mock, fetch, taskConfigValidate)
	driveTask(t, g, r, mock, fetch, taskMarkReady)

	// Complete the plan.
	_, err = r.reconcileSidecarProgression(ctx, fetch())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fetch().Status.InitPlan.Phase).To(Equal(seiv1alpha1.TaskPlanComplete))
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

	// First reconcile creates plan and submits config-apply.
	taskID := uuid.New()
	mock.submitID = taskID
	_, err := r.reconcileSidecarProgression(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())

	// Fail the task.
	mock.taskResults = map[uuid.UUID]*sidecar.TaskResult{
		taskID: completedResult(taskID, taskConfigApply, strPtr("S3 access denied")),
	}
	_, err = r.reconcileSidecarProgression(ctx, fetch())
	g.Expect(err).NotTo(HaveOccurred())

	updated := fetch()
	g.Expect(updated.Status.InitPlan.Phase).To(Equal(seiv1alpha1.TaskPlanFailed))
	g.Expect(updated.Status.InitPlan.Tasks[0].Status).To(Equal(seiv1alpha1.PlannedTaskFailed))
	g.Expect(updated.Status.InitPlan.Tasks[0].Error).To(Equal("S3 access denied"))

	// Subsequent reconciles are no-ops.
	mock.submitted = nil
	_, err = r.reconcileSidecarProgression(ctx, fetch())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(BeEmpty())
}
