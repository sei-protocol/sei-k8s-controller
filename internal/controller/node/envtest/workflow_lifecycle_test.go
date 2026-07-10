//go:build envtest

package envtest_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	nodecontroller "github.com/sei-protocol/sei-k8s-controller/internal/controller/node"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// fakeSidecar is a controllable task.SidecarClient: every submitted task
// completes, unless its type is in failTypes (fails terminally) or stallTypes
// (stays Running so the workflow parks adopted for finalizer/pause tests).
type fakeSidecar struct {
	mu         sync.Mutex
	idToType   map[uuid.UUID]string
	failTypes  map[string]bool
	stallTypes map[string]bool
}

func newFakeSidecar(failTypes ...string) *fakeSidecar {
	f := &fakeSidecar{idToType: map[uuid.UUID]string{}, failTypes: map[string]bool{}, stallTypes: map[string]bool{}}
	for _, ft := range failTypes {
		f.failTypes[ft] = true
	}
	return f
}

func (f *fakeSidecar) stallAt(taskType string) *fakeSidecar {
	f.stallTypes[taskType] = true
	return f
}

func (f *fakeSidecar) SubmitTask(_ context.Context, req sidecar.TaskRequest) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Id != nil {
		f.idToType[*req.Id] = string(req.Type)
		return *req.Id, nil
	}
	return uuid.New(), nil
}

func (f *fakeSidecar) GetTask(_ context.Context, id uuid.UUID) (*sidecar.TaskResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	typ := f.idToType[id]
	if f.stallTypes[typ] {
		// Neither Completed nor Failed: the execution stays Running.
		return &sidecar.TaskResult{Id: id, Type: typ, Status: "running"}, nil
	}
	if f.failTypes[typ] {
		msg := "injected failure"
		return &sidecar.TaskResult{Id: id, Type: typ, Status: sidecar.Failed, Error: &msg}, nil
	}
	return &sidecar.TaskResult{Id: id, Type: typ, Status: sidecar.Completed}, nil
}

func (f *fakeSidecar) Healthz(context.Context) (bool, error)     { return true, nil }
func (f *fakeSidecar) GetNodeID(context.Context) (string, error) { return "fake-node-id", nil }

// startNodeManager spins up a manager running the SeiNode controller with the
// fake sidecar, cancelled on test cleanup. Returns the manager's client.
func startNodeManager(t *testing.T, fake *fakeSidecar) client.Client {
	cli, _ := startNodeManagerC(t, fake)
	return cli
}

// startNodeManagerC also returns a cancel func so a test can stop the manager
// mid-run (controller-restart simulation). Cleanup cancels it regardless.
func startNodeManagerC(t *testing.T, fake *fakeSidecar) (client.Client, context.CancelFunc) {
	t.Helper()
	g := NewWithT(t)

	skipNameValidation := true
	mgr, err := ctrl.NewManager(testCfg, ctrl.Options{
		Scheme:                 testCli.Scheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
		// Each subtest starts a fresh manager in the same process; skip the
		// global controller-name uniqueness check that would otherwise reject
		// the second "seinode" controller.
		Controller: ctrlconfig.Controller{SkipNameValidation: &skipNameValidation},
	})
	g.Expect(err).NotTo(HaveOccurred())

	sc := func(_ *seiv1alpha1.SeiNode) (task.SidecarClient, error) { return fake, nil }
	cfgFor := func(node *seiv1alpha1.SeiNode) task.ExecutionConfig {
		return task.ExecutionConfig{
			BuildSidecarClient: func() (task.SidecarClient, error) { return fake, nil },
			KubeClient:         mgr.GetClient(),
			APIReader:          mgr.GetAPIReader(),
			Scheme:             mgr.GetScheme(),
			Resource:           node,
			Platform:           platformtest.Config(),
		}
	}
	r := &nodecontroller.SeiNodeReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("seinode-test"),
		Platform: platformtest.Config(),
		Planner:  &planner.NodeResolver{BuildSidecarClient: sc, Platform: platformtest.Config()},
		PlanExecutor: &planner.Executor[*seiv1alpha1.SeiNode]{
			ConfigFor: func(_ context.Context, node *seiv1alpha1.SeiNode) task.ExecutionConfig { return cfgFor(node) },
		},
		WorkflowConfigFor: func(_ context.Context, node *seiv1alpha1.SeiNode, _ *seiv1alpha1.SeiNodeTaskWorkflow) task.ExecutionConfig {
			return cfgFor(node)
		},
	}
	g.Expect(r.SetupWithManager(mgr)).To(Succeed())

	ctx, cancel := context.WithCancel(testCtx)
	go func() { _ = mgr.Start(ctx) }()
	g.Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())
	t.Cleanup(cancel)
	return mgr.GetClient(), cancel
}

// bringNodeToIdle waits for init to reach Running, then settles the node to
// steady state. envtest runs no StatefulSet controller, so observe-image never
// completes and the post-init NodeUpdate plan (currentImage drift) would spin
// forever — masking the adoption path under test. Stamping currentImage and
// clearing the plan stands in for a completed rollout; the image-rollout
// machinery has its own coverage. Retries on the optimistic-lock conflict with
// the running reconciler until the node is durably Running + idle.
func bringNodeToIdle(t *testing.T, cli client.Client, name string) {
	t.Helper()
	eventuallyRunning(t, cli, name)
	NewWithT(t).Eventually(func(g Gomega) {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(cli.Get(testCtx, types.NamespacedName{Namespace: "default", Name: name}, n)).To(Succeed())
		n.Status.CurrentImage = n.Spec.Image
		n.Status.Plan = nil
		g.Expect(cli.Status().Update(testCtx, n)).To(Succeed())

		got := getNode(g, cli, name)
		g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.PhaseRunning))
		g.Expect(got.Status.CurrentImage).To(Equal(got.Spec.Image))
		g.Expect(got.Status.Plan).To(BeNil())
	}).WithTimeout(60 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
}

func lifecycleNode(name string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "sei-test",
			Image:    "ghcr.io/sei-protocol/seid:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Sidecar:  &seiv1alpha1.SidecarConfig{Port: 7777},
		},
	}
}

func lifecycleWorkflow(name, target string) *seiv1alpha1.SeiNodeTaskWorkflow {
	return &seiv1alpha1.SeiNodeTaskWorkflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeTaskWorkflowSpec{
			Kind:   seiv1alpha1.SeiNodeTaskWorkflowKindStateSync,
			Target: seiv1alpha1.SeiNodeTaskTarget{NodeRef: seiv1alpha1.SeiNodeTaskNodeRef{Name: target}},
			StateSync: &seiv1alpha1.StateSyncWorkflow{
				RpcServers: []string{"a:26657", "b:26657"},
			},
		},
	}
}

func getNode(g Gomega, cli client.Client, name string) *seiv1alpha1.SeiNode {
	n := &seiv1alpha1.SeiNode{}
	g.Expect(cli.Get(testCtx, types.NamespacedName{Namespace: "default", Name: name}, n)).To(Succeed())
	return n
}

func getLifecycleWorkflow(g Gomega, cli client.Client, name string) *seiv1alpha1.SeiNodeTaskWorkflow {
	w := &seiv1alpha1.SeiNodeTaskWorkflow{}
	g.Expect(cli.Get(testCtx, types.NamespacedName{Namespace: "default", Name: name}, w)).To(Succeed())
	return w
}

// eventuallyRunning waits for the init path to drive the node to Running.
func eventuallyRunning(t *testing.T, cli client.Client, name string) {
	t.Helper()
	NewWithT(t).Eventually(func(g Gomega) {
		g.Expect(getNode(g, cli, name).Status.Phase).To(Equal(seiv1alpha1.PhaseRunning))
	}).WithTimeout(90 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
}

func TestWorkflowLifecycle_AdoptAndComplete(t *testing.T) {
	g := NewWithT(t)
	fake := newFakeSidecar()
	cli := startNodeManager(t, fake)

	node := lifecycleNode("lc-complete")
	g.Expect(cli.Create(testCtx, node)).To(Succeed())
	t.Cleanup(func() { _ = cli.Delete(testCtx, node) })
	bringNodeToIdle(t, cli, "lc-complete")

	wf := lifecycleWorkflow("wf-complete", "lc-complete")
	g.Expect(cli.Create(testCtx, wf)).To(Succeed())

	// Adoption observable (stable once set): a plan is compiled and persisted on
	// the workflow, gated by the finalizer. (No-execution-on-adopting-reconcile
	// is asserted synchronously in the reconcileWorkflow unit tests.)
	g.Eventually(func(g Gomega) {
		g.Expect(getLifecycleWorkflow(g, cli, "wf-complete").Status.Plan).NotTo(BeNil())
	}).WithTimeout(30 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())

	// Drive-to-terminal: fake completes each task -> workflow Complete, Ready
	// latched, node pointer cleared, WorkflowInProgress back to False, finalizer
	// reaped so a GitOps delete would not hang.
	g.Eventually(func(g Gomega) {
		w := getLifecycleWorkflow(g, cli, "wf-complete")
		g.Expect(w.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskWorkflowPhaseComplete))
		ready := apimeta.FindStatusCondition(w.Status.Conditions, seiv1alpha1.ConditionSeiNodeTaskWorkflowReady)
		g.Expect(ready).NotTo(BeNil())
		g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(w.Finalizers).NotTo(ContainElement(seiv1alpha1.SeiNodeTaskWorkflowFinalizer))

		n := getNode(g, cli, "lc-complete")
		g.Expect(n.Status.AdoptedWorkflow).To(BeNil())
		wip := apimeta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionWorkflowInProgress)
		g.Expect(wip).NotTo(BeNil())
		g.Expect(wip.Status).To(Equal(metav1.ConditionFalse))
	}).WithTimeout(90 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
}

func TestWorkflowLifecycle_FailurePath(t *testing.T) {
	g := NewWithT(t)
	// reset-data fails terminally: the node must stay held, phase untouched.
	fake := newFakeSidecar(sidecar.TaskTypeResetData)
	cli := startNodeManager(t, fake)

	node := lifecycleNode("lc-fail")
	g.Expect(cli.Create(testCtx, node)).To(Succeed())
	t.Cleanup(func() { _ = cli.Delete(testCtx, node) })
	bringNodeToIdle(t, cli, "lc-fail")

	wf := lifecycleWorkflow("wf-fail", "lc-fail")
	g.Expect(cli.Create(testCtx, wf)).To(Succeed())
	t.Cleanup(func() { forceDelete(cli, wf) })

	g.Eventually(func(g Gomega) {
		w := getLifecycleWorkflow(g, cli, "wf-fail")
		g.Expect(w.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskWorkflowPhaseFailed))
		failed := apimeta.FindStatusCondition(w.Status.Conditions, seiv1alpha1.ConditionSeiNodeTaskWorkflowFailed)
		g.Expect(failed).NotTo(BeNil())
		g.Expect(failed.Status).To(Equal(metav1.ConditionTrue))

		n := getNode(g, cli, "lc-fail")
		// Parked held: pointer RETAINED, node phase untouched, WIP=FailedHeld.
		g.Expect(n.Status.AdoptedWorkflow).NotTo(BeNil())
		g.Expect(n.Status.Phase).To(Equal(seiv1alpha1.PhaseRunning))
		wip := apimeta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionWorkflowInProgress)
		g.Expect(wip.Status).To(Equal(metav1.ConditionTrue))
		g.Expect(wip.Reason).To(Equal(seiv1alpha1.ReasonWorkflowFailedHeld))
	}).WithTimeout(90 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
}

func TestWorkflowLifecycle_ReadoptByUIDAfterRestart(t *testing.T) {
	g := NewWithT(t)
	fake := newFakeSidecar().stallAt(sidecar.TaskTypeAwaitCondition)
	cli, cancel := startNodeManagerC(t, fake)

	node := lifecycleNode("lc-restart")
	g.Expect(cli.Create(testCtx, node)).To(Succeed())
	t.Cleanup(func() { _ = cli.Delete(testCtx, node) })
	bringNodeToIdle(t, cli, "lc-restart")

	wf := lifecycleWorkflow("wf-restart", "lc-restart")
	g.Expect(cli.Create(testCtx, wf)).To(Succeed())
	t.Cleanup(func() { forceDelete(cli, wf) })

	var adoptedUID types.UID
	g.Eventually(func(g Gomega) {
		n := getNode(g, cli, "lc-restart")
		g.Expect(n.Status.AdoptedWorkflow).NotTo(BeNil())
		adoptedUID = n.Status.AdoptedWorkflow.UID
	}).WithTimeout(60 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

	// Simulate a controller restart: stop this manager, start a fresh one.
	cancel()
	cli2 := startNodeManager(t, fake)

	// The fresh instance resumes the SAME workflow by UID — no re-adoption of a
	// different object, pointer stable.
	g.Consistently(func(g Gomega) {
		n := getNode(g, cli2, "lc-restart")
		g.Expect(n.Status.AdoptedWorkflow).NotTo(BeNil())
		g.Expect(n.Status.AdoptedWorkflow.UID).To(Equal(adoptedUID))
	}).WithTimeout(15 * time.Second).WithPolling(1 * time.Second).Should(Succeed())
}

func TestWorkflowLifecycle_QueueThenSequential(t *testing.T) {
	g := NewWithT(t)
	fake := newFakeSidecar()
	cli := startNodeManager(t, fake)

	node := lifecycleNode("lc-queue")
	g.Expect(cli.Create(testCtx, node)).To(Succeed())
	t.Cleanup(func() { _ = cli.Delete(testCtx, node) })
	bringNodeToIdle(t, cli, "lc-queue")

	first := lifecycleWorkflow("wf-first", "lc-queue")
	first.CreationTimestamp = metav1.Time{} // let the apiserver stamp it
	g.Expect(cli.Create(testCtx, first)).To(Succeed())
	t.Cleanup(func() { forceDelete(cli, first) })
	// Ensure a strict ordering: create the second a beat later.
	time.Sleep(1100 * time.Millisecond)
	second := lifecycleWorkflow("wf-second", "lc-queue")
	g.Expect(cli.Create(testCtx, second)).To(Succeed())
	t.Cleanup(func() { forceDelete(cli, second) })

	// The older workflow completes; the newer stays Pending (queued), then is
	// adopted and completes once the first is done.
	g.Eventually(func(g Gomega) {
		g.Expect(getLifecycleWorkflow(g, cli, "wf-first").Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskWorkflowPhaseComplete))
	}).WithTimeout(90 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

	g.Eventually(func(g Gomega) {
		g.Expect(getLifecycleWorkflow(g, cli, "wf-second").Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskWorkflowPhaseComplete))
	}).WithTimeout(90 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
}

func TestWorkflowLifecycle_FinalizerBlocksDeleteThenForce(t *testing.T) {
	g := NewWithT(t)
	fake := newFakeSidecar().stallAt(sidecar.TaskTypeAwaitCondition)
	cli := startNodeManager(t, fake)

	node := lifecycleNode("lc-final")
	g.Expect(cli.Create(testCtx, node)).To(Succeed())
	t.Cleanup(func() { _ = cli.Delete(testCtx, node) })
	bringNodeToIdle(t, cli, "lc-final")

	wf := lifecycleWorkflow("wf-final", "lc-final")
	g.Expect(cli.Create(testCtx, wf)).To(Succeed())
	t.Cleanup(func() { forceDelete(cli, wf) })

	g.Eventually(func(g Gomega) {
		g.Expect(getLifecycleWorkflow(g, cli, "wf-final").Finalizers).To(ContainElement(seiv1alpha1.SeiNodeTaskWorkflowFinalizer))
	}).WithTimeout(60 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

	// Delete without the force annotation: the finalizer holds (data-safety
	// verification fails closed), so the object lingers Terminating.
	g.Expect(cli.Delete(testCtx, getLifecycleWorkflow(g, cli, "wf-final"))).To(Succeed())
	g.Eventually(func(g Gomega) {
		w := getLifecycleWorkflow(g, cli, "wf-final")
		g.Expect(w.DeletionTimestamp).NotTo(BeNil())
		g.Expect(w.Finalizers).To(ContainElement(seiv1alpha1.SeiNodeTaskWorkflowFinalizer))
	}).WithTimeout(15 * time.Second).WithPolling(300 * time.Millisecond).Should(Succeed())
	// And it stays blocked (does not get released without the annotation).
	g.Consistently(func(g Gomega) {
		g.Expect(getLifecycleWorkflow(g, cli, "wf-final").Finalizers).To(ContainElement(seiv1alpha1.SeiNodeTaskWorkflowFinalizer))
	}).WithTimeout(8 * time.Second).WithPolling(1 * time.Second).Should(Succeed())

	// Set the force annotation: the finalizer is released and the object is GC'd.
	w := getLifecycleWorkflow(g, cli, "wf-final")
	patch := client.MergeFrom(w.DeepCopy())
	w.Annotations = map[string]string{seiv1alpha1.WorkflowForceDeleteAnnotation: "true"}
	g.Expect(cli.Patch(testCtx, w, patch)).To(Succeed())

	g.Eventually(func(g Gomega) {
		err := cli.Get(testCtx, types.NamespacedName{Namespace: "default", Name: "wf-final"}, &seiv1alpha1.SeiNodeTaskWorkflow{})
		g.Expect(err).To(HaveOccurred()) // NotFound: finalizer released, object gone
	}).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
}

func TestWorkflowLifecycle_RefusesValidatorTarget(t *testing.T) {
	g := NewWithT(t)
	fake := newFakeSidecar()
	cli := startNodeManager(t, fake)

	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "lc-val", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:   "sei-test",
			Image:     "ghcr.io/sei-protocol/seid:latest",
			Validator: &seiv1alpha1.ValidatorSpec{},
			Sidecar:   &seiv1alpha1.SidecarConfig{Port: 7777},
		},
	}
	g.Expect(cli.Create(testCtx, node)).To(Succeed())
	t.Cleanup(func() { _ = cli.Delete(testCtx, node) })
	// Force the validator node to a Running/idle steady state so the reconcile
	// reaches the workflow refusal (not gated behind init).
	NewWithT(t).Eventually(func(g Gomega) {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(cli.Get(testCtx, types.NamespacedName{Namespace: "default", Name: "lc-val"}, n)).To(Succeed())
		n.Status.Phase = seiv1alpha1.PhaseRunning
		n.Status.CurrentImage = n.Spec.Image
		n.Status.Plan = nil
		g.Expect(cli.Status().Update(testCtx, n)).To(Succeed())
		got := getNode(g, cli, "lc-val")
		g.Expect(got.Status.Phase).To(Equal(seiv1alpha1.PhaseRunning))
		g.Expect(got.Status.Plan).To(BeNil())
	}).WithTimeout(60 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

	wf := lifecycleWorkflow("wf-val", "lc-val")
	g.Expect(cli.Create(testCtx, wf)).To(Succeed())
	t.Cleanup(func() { forceDelete(cli, wf) })

	g.Eventually(func(g Gomega) {
		w := getLifecycleWorkflow(g, cli, "wf-val")
		g.Expect(w.Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskWorkflowPhaseFailed))
		failed := apimeta.FindStatusCondition(w.Status.Conditions, seiv1alpha1.ConditionSeiNodeTaskWorkflowFailed)
		g.Expect(failed).NotTo(BeNil())
		g.Expect(failed.Reason).To(Equal(seiv1alpha1.ReasonWorkflowTargetRejected))
		g.Expect(getNode(g, cli, "lc-val").Status.AdoptedWorkflow).To(BeNil())
	}).WithTimeout(60 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
}

func TestWorkflowLifecycle_PauseFreezesAdoption(t *testing.T) {
	g := NewWithT(t)
	fake := newFakeSidecar()
	cli := startNodeManager(t, fake)

	node := lifecycleNode("lc-pause")
	g.Expect(cli.Create(testCtx, node)).To(Succeed())
	t.Cleanup(func() { _ = cli.Delete(testCtx, node) })
	bringNodeToIdle(t, cli, "lc-pause")

	// Pause the node (spec source of truth): the reconcile freezes before the
	// workflow logic.
	NewWithT(t).Eventually(func(g Gomega) {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(cli.Get(testCtx, types.NamespacedName{Namespace: "default", Name: "lc-pause"}, n)).To(Succeed())
		n.Spec.Paused = true
		g.Expect(cli.Update(testCtx, n)).To(Succeed())
	}).WithTimeout(30 * time.Second).Should(Succeed())

	wf := lifecycleWorkflow("wf-pause", "lc-pause")
	g.Expect(cli.Create(testCtx, wf)).To(Succeed())
	t.Cleanup(func() { forceDelete(cli, wf) })

	// While paused, the workflow is never adopted.
	g.Consistently(func(g Gomega) {
		g.Expect(getNode(g, cli, "lc-pause").Status.AdoptedWorkflow).To(BeNil())
	}).WithTimeout(12 * time.Second).WithPolling(1 * time.Second).Should(Succeed())

	// Unpause: adoption resumes and the workflow completes.
	NewWithT(t).Eventually(func(g Gomega) {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(cli.Get(testCtx, types.NamespacedName{Namespace: "default", Name: "lc-pause"}, n)).To(Succeed())
		n.Spec.Paused = false
		g.Expect(cli.Update(testCtx, n)).To(Succeed())
	}).WithTimeout(30 * time.Second).Should(Succeed())

	g.Eventually(func(g Gomega) {
		g.Expect(getLifecycleWorkflow(g, cli, "wf-pause").Status.Phase).To(Equal(seiv1alpha1.SeiNodeTaskWorkflowPhaseComplete))
	}).WithTimeout(90 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
}

// forceDelete removes the workflow finalizer (if any) and deletes the object,
// so a stalled/held test workflow does not wedge on cleanup.
func forceDelete(cli client.Client, wf *seiv1alpha1.SeiNodeTaskWorkflow) {
	cur := &seiv1alpha1.SeiNodeTaskWorkflow{}
	if err := cli.Get(testCtx, types.NamespacedName{Namespace: wf.Namespace, Name: wf.Name}, cur); err != nil {
		return
	}
	if len(cur.Finalizers) > 0 {
		patch := client.MergeFrom(cur.DeepCopy())
		cur.Finalizers = nil
		_ = cli.Patch(testCtx, cur, patch)
	}
	_ = cli.Delete(testCtx, cur)
}

// TestWorkflowLifecycle_PauseFreezesForceDelete documents the accepted freeze
// semantics: spec.paused early-returns the reconcile BEFORE the workflow
// finalizer runs, so even a force-delete of an adopted workflow does not release
// the hold until the target is unpaused. Unpause first, then the force delete
// takes effect.
func TestWorkflowLifecycle_PauseFreezesForceDelete(t *testing.T) {
	g := NewWithT(t)
	fake := newFakeSidecar().stallAt(sidecar.TaskTypeAwaitCondition)
	cli := startNodeManager(t, fake)

	node := lifecycleNode("lc-pausedel")
	g.Expect(cli.Create(testCtx, node)).To(Succeed())
	t.Cleanup(func() { _ = cli.Delete(testCtx, node) })
	bringNodeToIdle(t, cli, "lc-pausedel")

	wf := lifecycleWorkflow("wf-pausedel", "lc-pausedel")
	g.Expect(cli.Create(testCtx, wf)).To(Succeed())
	t.Cleanup(func() { unpause(cli, "lc-pausedel"); forceDelete(cli, wf) })

	// Adopted + stalled: the finalizer is on and the workflow is executing.
	g.Eventually(func(g Gomega) {
		g.Expect(getLifecycleWorkflow(g, cli, "wf-pausedel").Finalizers).To(ContainElement(seiv1alpha1.SeiNodeTaskWorkflowFinalizer))
	}).WithTimeout(60 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

	// Pause the target, then force-delete the workflow.
	setPaused(t, cli, "lc-pausedel", true)
	cur := getLifecycleWorkflow(g, cli, "wf-pausedel")
	patch := client.MergeFrom(cur.DeepCopy())
	cur.Annotations = map[string]string{seiv1alpha1.WorkflowForceDeleteAnnotation: "true"}
	g.Expect(cli.Patch(testCtx, cur, patch)).To(Succeed())
	g.Expect(cli.Delete(testCtx, getLifecycleWorkflow(g, cli, "wf-pausedel"))).To(Succeed())

	// Frozen: even with the force annotation, the finalizer holds while paused.
	g.Consistently(func(g Gomega) {
		g.Expect(getLifecycleWorkflow(g, cli, "wf-pausedel").Finalizers).To(ContainElement(seiv1alpha1.SeiNodeTaskWorkflowFinalizer))
	}).WithTimeout(10 * time.Second).WithPolling(1 * time.Second).Should(Succeed())

	// Unpause: the force delete now takes effect and the object is GC'd.
	setPaused(t, cli, "lc-pausedel", false)
	g.Eventually(func(g Gomega) {
		err := cli.Get(testCtx, types.NamespacedName{Namespace: "default", Name: "wf-pausedel"}, &seiv1alpha1.SeiNodeTaskWorkflow{})
		g.Expect(err).To(HaveOccurred())
	}).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
}

func setPaused(t *testing.T, cli client.Client, name string, paused bool) {
	t.Helper()
	NewWithT(t).Eventually(func(g Gomega) {
		n := &seiv1alpha1.SeiNode{}
		g.Expect(cli.Get(testCtx, types.NamespacedName{Namespace: "default", Name: name}, n)).To(Succeed())
		n.Spec.Paused = paused
		g.Expect(cli.Update(testCtx, n)).To(Succeed())
	}).WithTimeout(30 * time.Second).Should(Succeed())
}

func unpause(cli client.Client, name string) {
	n := &seiv1alpha1.SeiNode{}
	if err := cli.Get(testCtx, types.NamespacedName{Namespace: "default", Name: name}, n); err != nil {
		return
	}
	if n.Spec.Paused {
		n.Spec.Paused = false
		_ = cli.Update(testCtx, n)
	}
}
