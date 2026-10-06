package nodetask

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

func newUnjailTask() *seiv1alpha1.SeiNodeTask {
	return &seiv1alpha1.SeiNodeTask{
		ObjectMeta: metav1.ObjectMeta{Name: testTaskName, Namespace: testNS, UID: "task-uid-unjail", Generation: 1},
		Spec: seiv1alpha1.SeiNodeTaskSpec{
			Kind: seiv1alpha1.SeiNodeTaskKindUnjail,
			Target: seiv1alpha1.SeiNodeTaskTarget{
				NodeRef:      seiv1alpha1.SeiNodeTaskNodeRef{Name: testNodeName},
				RequirePhase: seiv1alpha1.PhaseRunning,
			},
			Unjail: &seiv1alpha1.UnjailPayload{
				ChainID: testChainID,
				KeyName: testKeyName,
				Fees:    testFees,
				Gas:     200000,
			},
		},
	}
}

func TestTaskParamsForKind_Unjail(t *testing.T) {
	g := NewWithT(t)
	taskType, raw, err := taskParamsForKind(newUnjailTask(), nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(taskType).To(Equal(sidecar.TaskTypeUnjail))

	var got sidecar.UnjailTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got).To(Equal(sidecar.UnjailTask{
		ChainID: testChainID,
		KeyName: testKeyName,
		Fees:    testFees,
		Gas:     200000,
	}))
}

// Empty keyName on the CR resolves to the target's operator keyring uid, the
// same way as for GovVote.
func TestTaskParamsForKind_Unjail_DerivesKeyNameFromTarget(t *testing.T) {
	g := NewWithT(t)
	cr := newUnjailTask()
	cr.Spec.Unjail.KeyName = ""

	_, raw, err := taskParamsForKind(cr, newRunningNode())
	g.Expect(err).NotTo(HaveOccurred())
	var got sidecar.UnjailTask
	g.Expect(json.Unmarshal(raw, &got)).To(Succeed())
	g.Expect(got.KeyName).To(Equal(seiv1alpha1.GentxOperatorKeyName))
}

func TestTaskParamsForKind_Unjail_NonValidatorTarget(t *testing.T) {
	g := NewWithT(t)
	target := newRunningNode()
	target.Spec.Validator = nil
	target.Spec.FullNode = &seiv1alpha1.FullNodeSpec{}

	_, _, err := taskParamsForKind(newUnjailTask(), target)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("requires a validator target"))
	g.Expect(task.FailureReason(err)).To(Equal(task.ReasonParamsBuildFailed))
}

func TestReconcile_Unjail_Confirmed(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	fakeSC := newFakeSidecarClient()
	r, c := newReconcilerWithSidecar(t, time.Now(), fakeSC, newUnjailTask(), newRunningNode())

	_, err := r.Reconcile(ctx, req()) // R1 synth
	g.Expect(err).NotTo(HaveOccurred())
	taskID, perr := uuid.Parse(getTask(t, ctx, c).Status.Task.ID)
	g.Expect(perr).NotTo(HaveOccurred())
	_, err = r.Reconcile(ctx, req()) // R2 submit
	g.Expect(err).NotTo(HaveOccurred())

	fakeSC.setResultPayload(taskID, sidecar.Completed, "",
		json.RawMessage(`{"txHash":"U1","height":7,"inclusionStatus":"committed_ok"}`))

	_, err = r.Reconcile(ctx, req()) // R3 poll → confirmed
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseComplete))
	g.Expect(readyReasonOf(got)).To(Equal("Confirmed"))
	g.Expect(got.Status.Outputs).NotTo(BeNil())
	g.Expect(got.Status.Outputs.Unjail).To(Equal(&seiv1alpha1.UnjailOutputs{TxHash: "U1", Height: 7}))
}

// A committed-but-failed unjail latches TxFailed and still surfaces the tx hash.
func TestReconcile_Unjail_CommittedFailed_KeepsTxHash(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	fakeSC := newFakeSidecarClient()
	r, c := newReconcilerWithSidecar(t, time.Now(), fakeSC, newUnjailTask(), newRunningNode())

	_, err := r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	taskID, _ := uuid.Parse(getTask(t, ctx, c).Status.Task.ID)
	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())

	fakeSC.setResultPayload(taskID, sidecar.Failed, "tx U2 committed but failed: code=5",
		json.RawMessage(`{"txHash":"U2","height":8,"code":5,"inclusionStatus":"committed_failed"}`))

	_, err = r.Reconcile(ctx, req())
	g.Expect(err).NotTo(HaveOccurred())
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	g.Expect(failedReasonOf(got)).To(Equal("TxFailed"))
	g.Expect(got.Status.Outputs.Unjail).To(Equal(&seiv1alpha1.UnjailOutputs{TxHash: "U2", Height: 8}))
}

// A non-validator target fails at param synthesis and submits nothing.
func TestReconcile_Unjail_NonValidatorTarget_Fails(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	fakeSC := newFakeSidecarClient()
	target := newRunningNode()
	target.Spec.Validator = nil
	target.Spec.FullNode = &seiv1alpha1.FullNodeSpec{}
	r, c := newReconcilerWithSidecar(t, time.Now(), fakeSC, newUnjailTask(), target)

	for range 3 {
		_, err := r.Reconcile(ctx, req())
		g.Expect(err).NotTo(HaveOccurred())
	}
	got := getTask(t, ctx, c)
	g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskPhaseFailed))
	g.Expect(failedReasonOf(got)).To(Equal(task.ReasonParamsBuildFailed))
	g.Expect(fakeSC.submitCount()).To(Equal(0))
}
