package node

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
)

// newWorkflowReconciler builds a reconciler with the workflow field index and
// both status subresources registered on a fake client.
func newWorkflowReconciler(t *testing.T, objs ...client.Object) (*SeiNodeReconciler, client.Client) {
	t.Helper()
	s := newNodeTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&seiv1alpha1.SeiNode{}, &seiv1alpha1.SeiNodeTaskWorkflow{}).
		WithIndex(&seiv1alpha1.SeiNodeTaskWorkflow{}, workflowTargetNodeIndex, func(o client.Object) []string {
			wf := o.(*seiv1alpha1.SeiNodeTaskWorkflow)
			return []string{wf.Spec.Target.NodeRef.Name}
		}).
		Build()
	return &SeiNodeReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(100),
		Platform: platformtest.Config(),
	}, c
}

func idleRunningNode(name string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  pacific1ChainID,
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase:                seiv1alpha1.PhaseRunning,
			ResolvedStateSyncers: []string{"a:26657", "b:26657"},
		},
	}
}

func workflowFor(name, target string, created time.Time) *seiv1alpha1.SeiNodeTaskWorkflow {
	return &seiv1alpha1.SeiNodeTaskWorkflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         testNamespace,
			UID:               types.UID(name + "-uid"),
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: seiv1alpha1.SeiNodeTaskWorkflowSpec{
			Kind:      seiv1alpha1.SeiNodeTaskWorkflowKindStateSync,
			Target:    seiv1alpha1.SeiNodeTaskTarget{NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: target}},
			StateSync: &seiv1alpha1.StateSyncWorkflow{},
		},
	}
}

// callReconcileWorkflow fetches a fresh node (for a valid resourceVersion) and
// invokes the workflow state machine directly.
func callReconcileWorkflow(t *testing.T, r *SeiNodeReconciler, c client.Client, name string) (*seiv1alpha1.SeiNode, bool, bool) {
	t.Helper()
	node := &seiv1alpha1.SeiNode{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, node); err != nil {
		t.Fatalf("get node: %v", err)
	}
	before := node.DeepCopy()
	base := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
	suppress, _, handled, err := r.reconcileWorkflow(context.Background(), node, before, base)
	if err != nil {
		t.Fatalf("reconcileWorkflow: %v", err)
	}
	return node, suppress, handled
}

func getWorkflow(t *testing.T, c client.Client, name string) *seiv1alpha1.SeiNodeTaskWorkflow {
	t.Helper()
	wf := &seiv1alpha1.SeiNodeTaskWorkflow{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: name}, wf); err != nil {
		t.Fatalf("get workflow %s: %v", name, err)
	}
	return wf
}

func TestReconcileWorkflow_AdoptsOldestPending(t *testing.T) {
	g := NewWithT(t)
	node := idleRunningNode("rpc-0")
	wf := workflowFor("ss-0", "rpc-0", time.Now())
	r, c := newWorkflowReconciler(t, node, wf)

	got, suppress, handled := callReconcileWorkflow(t, r, c, "rpc-0")

	g.Expect(handled).To(BeTrue(), "adopting reconcile is self-contained")
	g.Expect(suppress).To(BeTrue(), "the node is occupied once adopted")
	g.Expect(got.Status.AdoptedWorkflow).NotTo(BeNil())
	g.Expect(got.Status.AdoptedWorkflow.Name).To(Equal("ss-0"))
	g.Expect(got.Status.AdoptedWorkflow.UID).To(Equal(types.UID("ss-0-uid")))

	wip := apimeta.FindStatusCondition(got.Status.Conditions, seiv1alpha1.ConditionWorkflowInProgress)
	g.Expect(wip).NotTo(BeNil())
	g.Expect(wip.Status).To(Equal(metav1.ConditionTrue))

	adopted := getWorkflow(t, c, "ss-0")
	g.Expect(adopted.Finalizers).To(ContainElement(seiv1alpha1.SeiNodeTaskWorkflowFinalizer))
	g.Expect(adopted.Status.Plan).NotTo(BeNil())
	g.Expect(adopted.Status.Plan.Tasks).To(HaveLen(5)) // config-patch omitted (no Migration in fixture); mark-ready is terminal
	g.Expect(adopted.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskWorkflowPhaseRunning))
}

func TestReconcileWorkflow_SecondWorkflowQueues(t *testing.T) {
	g := NewWithT(t)
	node := idleRunningNode("rpc-0")
	older := workflowFor("ss-older", "rpc-0", time.Now().Add(-time.Hour))
	newer := workflowFor("ss-newer", "rpc-0", time.Now())
	r, c := newWorkflowReconciler(t, node, newer, older)

	got, _, _ := callReconcileWorkflow(t, r, c, "rpc-0")

	// First-wins by creationTimestamp: the older workflow is adopted.
	g.Expect(got.Status.AdoptedWorkflow.Name).To(Equal("ss-older"))

	queued := getWorkflow(t, c, "ss-newer")
	g.Expect(queued.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskWorkflowPhasePending))
	cond := apimeta.FindStatusCondition(queued.Status.Conditions, seiv1alpha1.ConditionSeiNodeTaskWorkflowAdopted)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonWorkflowQueued))
	g.Expect(queued.Finalizers).NotTo(ContainElement(seiv1alpha1.SeiNodeTaskWorkflowFinalizer))
}

func TestReconcileWorkflow_RefusesValidatorTarget(t *testing.T) {
	g := NewWithT(t)
	node := idleRunningNode("val-0")
	node.Spec.FullNode = nil
	node.Spec.Validator = &seiv1alpha1.ValidatorSpec{}
	wf := workflowFor("ss-0", "val-0", time.Now())
	r, c := newWorkflowReconciler(t, node, wf)

	got, _, handled := callReconcileWorkflow(t, r, c, "val-0")

	g.Expect(handled).To(BeFalse())
	g.Expect(got.Status.AdoptedWorkflow).To(BeNil(), "a validator never adopts")
	// A validator target fails the workflow terminally so kubectl wait resolves.
	refused := getWorkflow(t, c, "ss-0")
	g.Expect(refused.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskWorkflowPhaseFailed))
	cond := apimeta.FindStatusCondition(refused.Status.Conditions, seiv1alpha1.ConditionSeiNodeTaskWorkflowFailed)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonWorkflowTargetRejected))
}

func TestReconcileWorkflow_RefusesPausedTarget(t *testing.T) {
	g := NewWithT(t)
	node := idleRunningNode("rpc-0")
	node.Spec.Paused = true
	wf := workflowFor("ss-0", "rpc-0", time.Now())
	r, c := newWorkflowReconciler(t, node, wf)

	got, _, _ := callReconcileWorkflow(t, r, c, "rpc-0")

	g.Expect(got.Status.AdoptedWorkflow).To(BeNil(), "a paused node never adopts")
	refused := getWorkflow(t, c, "ss-0")
	cond := apimeta.FindStatusCondition(refused.Status.Conditions, seiv1alpha1.ConditionSeiNodeTaskWorkflowAdopted)
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonWorkflowTargetNotReady))
}

func TestReconcileWorkflow_StalePointerClearedByUID(t *testing.T) {
	g := NewWithT(t)
	node := idleRunningNode("rpc-0")
	// Pointer names a workflow that exists by name but with a different UID
	// (deleted-and-recreated) — the UID guard must not mistake it.
	node.Status.AdoptedWorkflow = &seiv1alpha1.AdoptedWorkflowRef{
		Name: "ss-0", UID: types.UID("stale-uid"), AdoptedAt: metav1.Now(),
	}
	wf := workflowFor("ss-0", "rpc-0", time.Now()) // UID "ss-0-uid"
	r, c := newWorkflowReconciler(t, node, wf)

	got, suppress, handled := callReconcileWorkflow(t, r, c, "rpc-0")

	g.Expect(handled).To(BeFalse())
	g.Expect(suppress).To(BeFalse())
	g.Expect(got.Status.AdoptedWorkflow).To(BeNil(), "stale-UID pointer is cleared")
}

func TestReapCompletedWorkflowFinalizers(t *testing.T) {
	g := NewWithT(t)
	node := idleRunningNode("rpc-0")

	done := workflowFor("wf-done", "rpc-0", time.Now())
	done.Finalizers = []string{seiv1alpha1.SeiNodeTaskWorkflowFinalizer}
	done.Status.Phase = seiv1alpha1.SeiNodeTaskWorkflowPhaseComplete

	failed := workflowFor("wf-failed", "rpc-0", time.Now())
	failed.Finalizers = []string{seiv1alpha1.SeiNodeTaskWorkflowFinalizer}
	failed.Status.Phase = seiv1alpha1.SeiNodeTaskWorkflowPhaseFailed

	r, c := newWorkflowReconciler(t, node, done, failed)
	g.Expect(r.reapCompletedWorkflowFinalizers(context.Background(), node)).To(Succeed())

	// Complete workflow: finalizer reaped so a GitOps prune isn't wedged.
	g.Expect(getWorkflow(t, c, "wf-done").Finalizers).NotTo(ContainElement(seiv1alpha1.SeiNodeTaskWorkflowFinalizer))
	// Failed workflow: still held (finalizer retained) — releases only through
	// the finalizer's data-safety check.
	g.Expect(getWorkflow(t, c, "wf-failed").Finalizers).To(ContainElement(seiv1alpha1.SeiNodeTaskWorkflowFinalizer))
}

func TestReleaseWorkflowFinalizersForNode(t *testing.T) {
	g := NewWithT(t)
	node := idleRunningNode("rpc-0")

	// A parked (Failed, held) workflow must be released when its node is deleted
	// — a gone node has no seid to hold, so the gate would otherwise deadlock.
	held := workflowFor("wf-held", "rpc-0", time.Now())
	held.Finalizers = []string{seiv1alpha1.SeiNodeTaskWorkflowFinalizer}
	held.Status.Phase = seiv1alpha1.SeiNodeTaskWorkflowPhaseFailed

	r, c := newWorkflowReconciler(t, node, held)
	g.Expect(r.releaseWorkflowFinalizersForNode(context.Background(), node)).To(Succeed())
	g.Expect(getWorkflow(t, c, "wf-held").Finalizers).NotTo(ContainElement(seiv1alpha1.SeiNodeTaskWorkflowFinalizer))
}

func TestAdoptedWorkflowParkedFailed(t *testing.T) {
	g := NewWithT(t)
	node := idleRunningNode("rpc-0")

	// No condition yet: not parked (nothing adopted).
	g.Expect(adoptedWorkflowParkedFailed(node)).To(BeFalse())

	// Actively executing hold: NOT parked-failed, so the STS stays frozen.
	setWorkflowInProgress(node, metav1.ConditionTrue, seiv1alpha1.ReasonWorkflowRunning, "driving")
	g.Expect(adoptedWorkflowParkedFailed(node)).To(BeFalse())

	// Parked-Failed hold: releases the STS skip so sidecar hotfixes / impostor
	// recovery aren't suspended indefinitely.
	setWorkflowInProgress(node, metav1.ConditionTrue, seiv1alpha1.ReasonWorkflowFailedHeld, "failed; held")
	g.Expect(adoptedWorkflowParkedFailed(node)).To(BeTrue())
}

func TestReconcileWorkflow_ReadoptsByUIDAfterRestart(t *testing.T) {
	g := NewWithT(t)
	node := idleRunningNode("rpc-0")
	wf := workflowFor("ss-0", "rpc-0", time.Now())
	// Simulate a restart mid-adoption: pointer persisted, plan flush lost.
	node.Status.AdoptedWorkflow = &seiv1alpha1.AdoptedWorkflowRef{
		Name: "ss-0", UID: "ss-0-uid", AdoptedAt: metav1.Now(),
	}
	r, c := newWorkflowReconciler(t, node, wf)

	_, suppress, _ := callReconcileWorkflow(t, r, c, "rpc-0")

	g.Expect(suppress).To(BeTrue(), "the node stays occupied by the re-resolved workflow")
	// The interrupted plan is rebuilt and persisted deterministically.
	resumed := getWorkflow(t, c, "ss-0")
	g.Expect(resumed.Status.Plan).NotTo(BeNil())
	g.Expect(resumed.Status.Plan.Tasks).To(HaveLen(5))
	// The deletion gate is (re-)ensured on the drive step, not just at adoption:
	// an adoption interrupted before the finalizer add must not resume into
	// destructive execution ungated (guards the 1a regression).
	g.Expect(resumed.Finalizers).To(ContainElement(seiv1alpha1.SeiNodeTaskWorkflowFinalizer))
}
