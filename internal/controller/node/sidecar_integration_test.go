package node

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	sidecar "github.com/sei-protocol/sei-sidecar-client-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestIntegrationFullProgressionSnapshotMode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	mock := &mockSidecarClient{status: &sidecar.StatusResponse{Status: sidecar.Initializing}}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	// Phase: Initialized → issues snapshot-restore
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing}
	_, err := r.reconcileSidecarProgression(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskSnapshotRestore))

	fetchNode := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(sidecarInitializing))

	// Phase: TaskRunning — requeues without submitting
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Running}
	result, err := r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(bootstrapPollInterval))
	g.Expect(mock.submitted).To(BeEmpty())
	g.Expect(fetchNode().Status.SidecarCurrentTask).To(BeEmpty())

	// Phase: TaskComplete (snapshot-restore) → issues discover-peers
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskSnapshotRestore}}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskDiscoverPeers))
	g.Expect(fetchNode().Status.SidecarLastTask).To(Equal(taskSnapshotRestore))

	// Phase: TaskComplete (discover-peers) → issues config-patch
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskDiscoverPeers}}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskConfigPatch))

	// Phase: TaskComplete (config-patch) → issues mark-ready
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskConfigPatch}}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskMarkReady))

	// Phase: Ready — steady-state polling
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Ready}
	result, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(statusPollInterval))
	g.Expect(mock.submitted).To(BeEmpty())
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(sidecarReady))
}

func TestIntegrationFullProgressionPeerSyncMode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := peerSyncNode()
	mock := &mockSidecarClient{status: &sidecar.StatusResponse{Status: sidecar.Initializing}}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetchNode := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// Initialized → discover-peers
	_, err := r.reconcileSidecarProgression(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskDiscoverPeers))

	// TaskComplete (discover-peers) → configure-genesis
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskDiscoverPeers}}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskConfigureGenesis))

	// TaskComplete (configure-genesis) → configure-state-sync
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskConfigureGenesis}}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskConfigureStateSync))

	// TaskComplete (configure-state-sync) → config-patch
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskConfigureStateSync}}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskConfigPatch))

	// TaskComplete (config-patch) → mark-ready
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskConfigPatch}}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskMarkReady))

	// Ready — steady-state
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Ready}
	result, err := r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(statusPollInterval))
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(sidecarReady))
}

func TestIntegrationFullProgressionGenesisMode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := genesisNode()
	mock := &mockSidecarClient{status: &sidecar.StatusResponse{Status: sidecar.Initializing}}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetchNode := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// Initialized → config-patch
	_, err := r.reconcileSidecarProgression(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskConfigPatch))

	// TaskComplete (config-patch) → mark-ready
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskConfigPatch}}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].TaskType()).To(Equal(taskMarkReady))

	// Ready — steady-state
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Ready}
	result, err := r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(statusPollInterval))
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(sidecarReady))
}

func TestIntegrationRuntimeTaskPhaseRecovery(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	mock := &mockSidecarClient{status: &sidecar.StatusResponse{Status: sidecar.Initializing}}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetchNode := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// Drive bootstrap to Ready (abbreviated — just set mock to Ready).
	mock.status = &sidecar.StatusResponse{Status: sidecar.Ready}
	result, err := r.reconcileSidecarProgression(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(statusPollInterval))
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(sidecarReady))

	// Simulate update-peers task completing — sidecar returns to Ready
	// (runtime tasks transition back to Ready, not TaskComplete per Req 6.4).
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{
		Status:   sidecar.Ready,
		LastTask: &sidecar.TaskResult{Type: taskUpdatePeers},
	}
	result, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(statusPollInterval))

	// Verify phase is Ready (not TaskComplete).
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(sidecarReady))

	// Verify controller can issue subsequent tasks — another reconcile
	// with Ready phase should work without errors.
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Ready}
	result, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(statusPollInterval))
}

func TestIntegrationBootstrapFailureRetry(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskSnapshotRestore, Error: strPtr("failed")}},
	}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetchNode := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// Retry 1 → 5s backoff
	result, err := r.reconcileSidecarProgression(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(5 * time.Second))

	// Retry 2 → 10s backoff
	result, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))

	// Retry 3 → 20s backoff
	result, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(20 * time.Second))

	// Retry 4 → exceeds max (3), sets Degraded condition, stops retrying
	result, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(BeZero())

	degraded := findCondition(fetchNode().Status.Conditions, ConditionTypeDegraded)
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(degraded.Reason).To(Equal(reasonBootstrapTaskFailed))

	// Sidecar restarts → Initialized phase regression resets retry counter
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())

	// Verify retry counter was reset by checking that a new failure
	// starts at 5s backoff again (not continuing from previous count).
	mock.submitted = nil
	mock.status = &sidecar.StatusResponse{Status: sidecar.Initializing, LastTask: &sidecar.TaskResult{Type: taskSnapshotRestore, Error: strPtr("failed")}}
	result, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(5 * time.Second))
}
