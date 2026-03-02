package node

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-node-controller/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// 11.1 — Full reconcile loop: Initialized → Ready for each bootstrap mode
// ---------------------------------------------------------------------------

func TestIntegrationFullProgressionSnapshotMode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	mock := &mockSidecarClient{status: &StatusResponse{Phase: phaseInitialized}}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	// Phase: Initialized → issues snapshot-restore
	mock.status = &StatusResponse{Phase: phaseInitialized}
	_, err := r.reconcileSidecarProgression(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(taskSnapshotRestore))

	fetchNode := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(phaseInitialized))

	// Phase: TaskRunning — requeues without submitting
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseTaskRunning, CurrentTask: taskSnapshotRestore}
	result, err := r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(bootstrapPollInterval))
	g.Expect(mock.submitted).To(BeEmpty())
	g.Expect(fetchNode().Status.SidecarCurrentTask).To(Equal(taskSnapshotRestore))

	// Phase: TaskComplete (snapshot-restore) → issues discover-peers
	mock.status = &StatusResponse{Phase: phaseTaskComplete, LastTask: taskSnapshotRestore, LastTaskResult: "success"}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(taskDiscoverPeers))
	g.Expect(fetchNode().Status.SidecarLastTask).To(Equal(taskSnapshotRestore))

	// Phase: TaskComplete (discover-peers) → issues config-patch
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseTaskComplete, LastTask: taskDiscoverPeers, LastTaskResult: "success"}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(taskConfigPatch))

	// Phase: TaskComplete (config-patch) → issues mark-ready
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseTaskComplete, LastTask: taskConfigPatch, LastTaskResult: "success"}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(taskMarkReady))

	// Phase: Ready — steady-state polling
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseReady}
	result, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(statusPollInterval))
	g.Expect(mock.submitted).To(BeEmpty())
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(phaseReady))
}

func TestIntegrationFullProgressionPeerSyncMode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := peerSyncNode()
	mock := &mockSidecarClient{status: &StatusResponse{Phase: phaseInitialized}}
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
	g.Expect(mock.submitted[0].Type).To(Equal(taskDiscoverPeers))

	// TaskComplete (discover-peers) → configure-genesis
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseTaskComplete, LastTask: taskDiscoverPeers, LastTaskResult: "success"}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(taskConfigureGenesis))

	// TaskComplete (configure-genesis) → configure-state-sync
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseTaskComplete, LastTask: taskConfigureGenesis, LastTaskResult: "success"}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(taskConfigureStateSync))

	// TaskComplete (configure-state-sync) → config-patch
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseTaskComplete, LastTask: taskConfigureStateSync, LastTaskResult: "success"}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(taskConfigPatch))

	// TaskComplete (config-patch) → mark-ready
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseTaskComplete, LastTask: taskConfigPatch, LastTaskResult: "success"}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(taskMarkReady))

	// Ready — steady-state
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseReady}
	result, err := r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(statusPollInterval))
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(phaseReady))
}

func TestIntegrationFullProgressionGenesisMode(t *testing.T) {
	g := NewGomegaWithT(t)
	node := genesisNode()
	mock := &mockSidecarClient{status: &StatusResponse{Phase: phaseInitialized}}
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
	g.Expect(mock.submitted[0].Type).To(Equal(taskConfigPatch))

	// TaskComplete (config-patch) → mark-ready
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseTaskComplete, LastTask: taskConfigPatch, LastTaskResult: "success"}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(taskMarkReady))

	// Ready — steady-state
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseReady}
	result, err := r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(statusPollInterval))
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(phaseReady))
}

// ---------------------------------------------------------------------------
// 11.2 — Upgrade lifecycle: schedule → pending → UpgradeHalted → patch → prune
// ---------------------------------------------------------------------------

func TestIntegrationUpgradeLifecycle(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	node.Spec.ScheduledUpgrades = []seiv1alpha1.ScheduledUpgrade{
		{Height: 196000000, Image: "sei:v2"},
	}
	sts := testStatefulSet(node)
	mock := &mockSidecarClient{status: &StatusResponse{Phase: phaseReady}}
	r, c := newProgressionReconciler(t, mock, node, sts)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetchNode := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// Sidecar is Ready — controller submits schedule-upgrade.
	_, err := r.reconcileSidecarProgression(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Type).To(Equal(taskScheduleUpgrade))
	g.Expect(mock.submitted[0].Params["height"]).To(Equal(int64(196000000)))
	g.Expect(mock.submitted[0].Params["image"]).To(Equal("sei:v2"))

	// Verify sidecar stays Ready while upgrade is pending.
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(phaseReady))
	g.Expect(fetchNode().Status.SubmittedUpgradeHeights).To(ContainElement(int64(196000000)))

	// Sidecar transitions to UpgradeHalted after block height reached.
	mock.submitted = nil
	mock.status = &StatusResponse{
		Phase:         phaseUpgradeHalted,
		UpgradeHeight: 196000000,
		UpgradeImage:  "sei:v2",
	}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())

	// Verify StatefulSet image was patched.
	updatedSts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, key, updatedSts)).To(Succeed())
	g.Expect(updatedSts.Spec.Template.Spec.Containers[0].Image).To(Equal("sei:v2"))

	// Verify upgrade was pruned from spec and status.
	updated := fetchNode()
	g.Expect(updated.Spec.ScheduledUpgrades).To(BeEmpty())
	g.Expect(updated.Status.SubmittedUpgradeHeights).To(BeEmpty())
}

// ---------------------------------------------------------------------------
// 11.3 — Runtime task phase recovery: update-peers returns to Ready
// ---------------------------------------------------------------------------

func TestIntegrationRuntimeTaskPhaseRecovery(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	mock := &mockSidecarClient{status: &StatusResponse{Phase: phaseInitialized}}
	r, c := newProgressionReconciler(t, mock, node)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetchNode := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// Drive bootstrap to Ready (abbreviated — just set mock to Ready).
	mock.status = &StatusResponse{Phase: phaseReady}
	result, err := r.reconcileSidecarProgression(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(statusPollInterval))
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(phaseReady))

	// Simulate update-peers task completing — sidecar returns to Ready
	// (runtime tasks transition back to Ready, not TaskComplete per Req 6.4).
	mock.submitted = nil
	mock.status = &StatusResponse{
		Phase:          phaseReady,
		LastTask:       taskUpdatePeers,
		LastTaskResult: "success",
	}
	result, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(statusPollInterval))

	// Verify phase is Ready (not TaskComplete).
	g.Expect(fetchNode().Status.SidecarPhase).To(Equal(phaseReady))

	// Verify controller can issue subsequent tasks — another reconcile
	// with Ready phase should work without errors.
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseReady}
	result, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(statusPollInterval))
}

// ---------------------------------------------------------------------------
// 11.4 — Bootstrap failure retry with exponential backoff and Degraded
// ---------------------------------------------------------------------------

func TestIntegrationBootstrapFailureRetry(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	mock := &mockSidecarClient{
		status: &StatusResponse{Phase: phaseTaskComplete, LastTask: taskSnapshotRestore, LastTaskResult: "error"},
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
	mock.status = &StatusResponse{Phase: phaseInitialized}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())

	// Verify retry counter was reset by checking that a new failure
	// starts at 5s backoff again (not continuing from previous count).
	mock.submitted = nil
	mock.status = &StatusResponse{Phase: phaseTaskComplete, LastTask: taskSnapshotRestore, LastTaskResult: "error"}
	result, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(5 * time.Second))
}

// ---------------------------------------------------------------------------
// 11.5 — Upgrade ordering: multiple upgrades fire lowest-height first
// ---------------------------------------------------------------------------

func TestIntegrationUpgradeOrdering(t *testing.T) {
	g := NewGomegaWithT(t)
	node := snapshotNode()
	node.Spec.ScheduledUpgrades = []seiv1alpha1.ScheduledUpgrade{
		{Height: 300, Image: "sei:v3"},
		{Height: 100, Image: "sei:v1"},
		{Height: 200, Image: "sei:v2"},
	}
	sts := testStatefulSet(node)
	mock := &mockSidecarClient{status: &StatusResponse{Phase: phaseReady}}
	r, c := newProgressionReconciler(t, mock, node, sts)
	ctx := context.Background()
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}

	fetchNode := func() *seiv1alpha1.SeiNode {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(c.Get(ctx, key, n)).To(Succeed())
		return n
	}

	// Round 1: controller submits lowest-height upgrade (100) first.
	_, err := r.reconcileSidecarProgression(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(mock.submitted).To(HaveLen(1))
	g.Expect(mock.submitted[0].Params["height"]).To(Equal(int64(100)))
	g.Expect(mock.submitted[0].Params["image"]).To(Equal("sei:v1"))

	// Submit remaining upgrades in subsequent reconciles.
	for range 2 {
		mock.submitted = nil
		_, err = r.reconcileSidecarProgression(ctx, fetchNode())
		g.Expect(err).NotTo(HaveOccurred())
	}

	// All three should now be submitted.
	submitted := fetchNode().Status.SubmittedUpgradeHeights
	g.Expect(submitted).To(ConsistOf(int64(100), int64(200), int64(300)))

	// Sidecar fires lowest-height upgrade (100) → UpgradeHalted.
	mock.submitted = nil
	mock.status = &StatusResponse{
		Phase:         phaseUpgradeHalted,
		UpgradeHeight: 100,
		UpgradeImage:  "sei:v1",
	}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())

	// Verify image patched to v1 and height 100 pruned.
	updatedSts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, key, updatedSts)).To(Succeed())
	g.Expect(updatedSts.Spec.Template.Spec.Containers[0].Image).To(Equal("sei:v1"))

	updated := fetchNode()
	g.Expect(updated.Spec.ScheduledUpgrades).To(HaveLen(2))
	for _, u := range updated.Spec.ScheduledUpgrades {
		g.Expect(u.Height).NotTo(Equal(int64(100)))
	}

	// Simulate pod restart — sidecar re-bootstraps, reaches Ready again.
	mock.status = &StatusResponse{Phase: phaseReady}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())

	// Sidecar fires next upgrade (200) → UpgradeHalted.
	mock.submitted = nil
	mock.status = &StatusResponse{
		Phase:         phaseUpgradeHalted,
		UpgradeHeight: 200,
		UpgradeImage:  "sei:v2",
	}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())

	// Verify image patched to v2 and height 200 pruned.
	g.Expect(c.Get(ctx, key, updatedSts)).To(Succeed())
	g.Expect(updatedSts.Spec.Template.Spec.Containers[0].Image).To(Equal("sei:v2"))

	updated = fetchNode()
	g.Expect(updated.Spec.ScheduledUpgrades).To(HaveLen(1))
	g.Expect(updated.Spec.ScheduledUpgrades[0].Height).To(Equal(int64(300)))

	// Simulate another pod restart → Ready → fires last upgrade (300).
	mock.status = &StatusResponse{Phase: phaseReady}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())

	mock.submitted = nil
	mock.status = &StatusResponse{
		Phase:         phaseUpgradeHalted,
		UpgradeHeight: 300,
		UpgradeImage:  "sei:v3",
	}
	_, err = r.reconcileSidecarProgression(ctx, fetchNode())
	g.Expect(err).NotTo(HaveOccurred())

	// Verify final state: image v3, all upgrades pruned.
	g.Expect(c.Get(ctx, key, updatedSts)).To(Succeed())
	g.Expect(updatedSts.Spec.Template.Spec.Containers[0].Image).To(Equal("sei:v3"))

	updated = fetchNode()
	g.Expect(updated.Spec.ScheduledUpgrades).To(BeEmpty())
	g.Expect(updated.Status.SubmittedUpgradeHeights).To(BeEmpty())
}
