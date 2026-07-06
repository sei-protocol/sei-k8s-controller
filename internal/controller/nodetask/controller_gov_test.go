package nodetask

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	sidecartasks "github.com/sei-protocol/seictl/sidecar/tasks"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// setResultPayload stages a terminal task result carrying a structured result
// payload (the gov completion-contract channel).
func (f *fakeSidecarClient) setResultPayload(id uuid.UUID, status sidecar.TaskResultStatus, errMsg string, payload json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	res := &sidecar.TaskResult{Id: id, Status: status, Type: "test", SubmittedAt: time.Now()}
	if errMsg != "" {
		res.Error = &errMsg
	}
	if len(payload) > 0 {
		p := payload
		res.Result = &p
	}
	f.results[id] = res
}

func newGovUpgradeTask() *seiv1alpha1.SeiNodeTask {
	return &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-govupgrade", Generation: 1},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindGovSoftwareUpgrade,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef:      seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
				RequirePhase: seiv1alpha1.PhaseRunning,
			},
			GovSoftwareUpgrade: &seiv1alpha1.GovSoftwareUpgradePayload{
				ChainID: testChainID, KeyName: testKeyName,
				Title: "t", Description: "d", UpgradeName: "v2", UpgradeHeight: 1000,
				InitialDeposit: "10000000usei", Fees: testFees, Gas: 200000,
			},
		},
	}
}

func readyReasonOf(cr *seiv1alpha1.SeiNodeTask) string {
	for _, c := range cr.Status.Conditions {
		if c.Type == seiv1alpha1.ConditionSeiNodeTaskReady && c.Status == metav1.ConditionTrue {
			return c.Reason
		}
	}
	return ""
}

func failedReasonOf(cr *seiv1alpha1.SeiNodeTask) string {
	for _, c := range cr.Status.Conditions {
		if c.Type == seiv1alpha1.ConditionSeiNodeTaskFailed && c.Status == metav1.ConditionTrue {
			return c.Reason
		}
	}
	return ""
}

func TestReconcile_GovUpgrade_Confirmed(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	fakeSC := newFakeSidecarClient()
	r, c := newReconcilerWithSidecar(t, time.Now(), fakeSC, newGovUpgradeTask(), newRunningNode())

	_, err := r.Reconcile(ctx, req()) // R1 synth
	g.Expect(err).NotTo(HaveOccurred())
	taskID, perr := uuid.Parse(getTask(t, ctx, c).Status.Task.ID)
	g.Expect(perr).NotTo(HaveOccurred())

	_, err = r.Reconcile(ctx, req()) // R2 submit
	g.Expect(err).NotTo(HaveOccurred())

	fakeSC.setResultPayload(taskID, sidecar.Completed, "",
		json.RawMessage(`{"txHash":"ABC","height":10,"proposalId":42,"inclusionStatus":"committed_ok"}`))

	_, err = r.Reconcile(ctx, req()) // R3 poll → confirmed
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))
	g.Expect(readyReasonOf(got)).To(Equal("Confirmed"))
	g.Expect(got.Status.Outputs).NotTo(BeNil())
	g.Expect(got.Status.Outputs.GovSoftwareUpgrade).NotTo(BeNil())
	g.Expect(got.Status.Outputs.GovSoftwareUpgrade.ProposalID).To(Equal(uint64(42)))
	g.Expect(got.Status.Outputs.GovSoftwareUpgrade.TxHash).To(Equal("ABC"))
	g.Expect(got.Status.Outputs.GovSoftwareUpgrade.Height).To(Equal(int64(10)))
}

func TestReconcile_GovUpgrade_CommittedFailed(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	fakeSC := newFakeSidecarClient()
	r, c := newReconcilerWithSidecar(t, time.Now(), fakeSC, newGovUpgradeTask(), newRunningNode())

	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	taskID, _ := uuid.Parse(getTask(t, ctx, c).Status.Task.ID)
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())

	fakeSC.setResultPayload(taskID, sidecar.Failed, "committed but failed: code=11",
		json.RawMessage(`{"txHash":"ABC","code":11,"inclusionStatus":"committed_failed"}`))

	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	g.Expect(failedReasonOf(got)).To(Equal("TxFailed"))
	g.Expect(got.Status.Outputs).NotTo(BeNil())
	g.Expect(got.Status.Outputs.GovSoftwareUpgrade.TxHash).To(Equal("ABC"))
}

func TestReconcile_GovUpgrade_Pending_ReSubmits(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	fakeSC := newFakeSidecarClient()
	// No spec.timeoutSeconds → unbounded → pending always re-submits.
	r, c := newReconcilerWithSidecar(t, time.Now(), fakeSC, newGovUpgradeTask(), newRunningNode())

	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	taskID, _ := uuid.Parse(getTask(t, ctx, c).Status.Task.ID)
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())

	fakeSC.setResultPayload(taskID, sidecar.Failed, "inclusion undetermined",
		json.RawMessage(`{"txHash":"ABC","inclusionStatus":"pending"}`))

	_, err = r.Reconcile(ctx, req()) // R3: pending → reset to Pending, stay Running
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseRunning))
	g.Expect(got.Status.Task.Status).To(Equal(seiv1alpha1.TaskPending))

	before := fakeSC.submitCount()
	_, err = r.Reconcile(ctx, req()) // R4: re-submits (same task ID → engine re-run)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(fakeSC.submitCount()).To(Equal(before + 1))

	// Convergence: the re-checked tx now commits → Complete/Confirmed.
	fakeSC.setResultPayload(taskID, sidecar.Completed, "",
		json.RawMessage(`{"txHash":"ABC","proposalId":7,"inclusionStatus":"committed_ok"}`))
	_, err = r.Reconcile(ctx, req()) // R5: re-submit then poll → confirmed
	g.Expect(err).NotTo(HaveOccurred())
	got = getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))
	g.Expect(readyReasonOf(got)).To(Equal("Confirmed"))
	g.Expect(got.Status.Outputs.GovSoftwareUpgrade.ProposalID).To(Equal(uint64(7)))
}

// TestGovResultMirrorMatchesWire guards the local govResult mirror against
// drift from seictl's GovTxResult: a field/constant rename there would decode
// to zero here and silently misroute outcomes (the cost of not importing the
// type). This test fails loudly if the wire shape diverges.
func TestGovResultMirrorMatchesWire(t *testing.T) {
	g := NewWithT(t)
	g.Expect(inclusionCommittedOK).To(Equal(sidecartasks.InclusionCommittedOK))
	g.Expect(inclusionCommittedFailed).To(Equal(sidecartasks.InclusionCommittedFailed))
	g.Expect(inclusionPending).To(Equal(sidecartasks.InclusionPending))

	raw, err := json.Marshal(sidecartasks.GovTxResult{
		TxHash: "ABC", Height: 9, ProposalID: 42, Code: 5,
		Codespace: "sdk", RawLog: "boom", InclusionStatus: sidecartasks.InclusionCommittedFailed,
	})
	g.Expect(err).NotTo(HaveOccurred())
	var gr govResult
	g.Expect(json.Unmarshal(raw, &gr)).To(Succeed())
	g.Expect(gr).To(Equal(govResult{
		TxHash: "ABC", Height: 9, ProposalID: 42, Code: 5,
		Codespace: "sdk", RawLog: "boom", InclusionStatus: inclusionCommittedFailed,
	}))
}

// A gov Failed with no result payload (e.g. CheckTx reject) is a generic
// terminal failure, not committed_failed/pending.
func TestReconcile_GovUpgrade_NoResult_GenericFailure(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	fakeSC := newFakeSidecarClient()
	r, c := newReconcilerWithSidecar(t, time.Now(), fakeSC, newGovUpgradeTask(), newRunningNode())

	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	taskID, _ := uuid.Parse(getTask(t, ctx, c).Status.Task.ID)
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())

	fakeSC.setResult(taskID, sidecar.Failed, "checkTx rejected: code=5")

	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	g.Expect(failedReasonOf(got)).To(Equal("TaskFailed"))
}

func TestReconcile_GovVote_Confirmed(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	fakeSC := newFakeSidecarClient()
	r, c := newReconcilerWithSidecar(t, time.Now(), fakeSC, newGovVoteTask(), newRunningNode())

	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	taskID, _ := uuid.Parse(getTask(t, ctx, c).Status.Task.ID)
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())

	fakeSC.setResultPayload(taskID, sidecar.Completed, "",
		json.RawMessage(`{"txHash":"V1","height":3,"inclusionStatus":"committed_ok"}`))

	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))
	g.Expect(readyReasonOf(got)).To(Equal("Confirmed"))
	g.Expect(got.Status.Outputs).NotTo(BeNil())
	g.Expect(got.Status.Outputs.GovVote).NotTo(BeNil())
	g.Expect(got.Status.Outputs.GovVote.TxHash).To(Equal("V1"))
	g.Expect(got.Status.Outputs.GovVote.Height).To(Equal(int64(3)))
}

func TestReconcile_GovUpgrade_PendingTimeout_Fails(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	fakeSC := newFakeSidecarClient()
	clock := time.Now()
	cr := newGovUpgradeTask()
	cr.Spec.TimeoutSeconds = 1
	r, c := newReconcilerWithSidecar(t, clock, fakeSC, cr, newRunningNode())
	r.Now = func() time.Time { return clock }

	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	taskID, _ := uuid.Parse(getTask(t, ctx, c).Status.Task.ID)
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())

	fakeSC.setResultPayload(taskID, sidecar.Failed, "inclusion undetermined",
		json.RawMessage(`{"txHash":"ABC","inclusionStatus":"pending"}`))

	clock = clock.Add(2 * time.Second) // past spec.timeoutSeconds
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	g.Expect(failedReasonOf(got)).To(Equal("InclusionUndetermined"))
	g.Expect(got.Status.Outputs.GovSoftwareUpgrade.TxHash).To(Equal("ABC")) // txHash retained
}
