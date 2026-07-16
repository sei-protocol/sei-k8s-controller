package node

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	// workflowTargetNodeIndex indexes SeiNodeTaskWorkflows by their target node
	// name so the node reconcile can list the workflows aimed at it in O(1).
	workflowTargetNodeIndex = "spec.target.nodeRef.name"

	// defaultWorkflowRequirePhaseTimeout bounds how long a workflow waits for
	// its target to reach spec.target.requirePhase before failing terminally.
	// Mirrors nodetask's requirePhase default.
	defaultWorkflowRequirePhaseTimeout = 5 * time.Minute

	// workflowTargetWaitInterval is the requeue cadence while a workflow waits
	// for its target to reach the required phase.
	workflowTargetWaitInterval = 5 * time.Second
)

// reconcileWorkflow is the workflow half of the node's two-headed state
// machine. It returns:
//
//   - suppress: a workflow occupies this node (driving or parked-held); the
//     caller must skip drift planning, execution, and sidecar reapproval.
//   - result: the requeue cadence to use when suppress is true.
//   - handled: reconcileWorkflow performed its own node-first status patch and
//     the caller must return (result, nil) immediately (the adopting reconcile).
//
// It mutates node.Status in-memory for the non-handled cases so the
// WorkflowInProgress condition and any pointer clear ride the caller's single
// node patch.
func (r *SeiNodeReconciler) reconcileWorkflow(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	before *seiv1alpha1.SeiNode,
	statusBase client.Patch,
) (suppress bool, result ctrl.Result, handled bool, err error) {
	if node.Status.AdoptedWorkflow != nil {
		// An adoption pointer is only ever stamped on a Running node;
		// driveAdoptedWorkflow manages the WorkflowInProgress condition in every
		// branch, so no separate seed is needed here.
		return r.driveAdoptedWorkflow(ctx, node)
	}

	// No adoption pointer: only a Running node can adopt a workflow or carry
	// meaningful workflow status. Skip workflow reconciliation entirely for a
	// pre-Running node so it never adds a status write to the node's init /
	// genesis-ceremony reconciles — that extra write widens the optimistic-lock
	// window against a parent SeiNetwork's child-pause propagation and can slow
	// a child to Running, and a non-Running node has no workflow to adopt or
	// seed anyway. WorkflowInProgress is seeded False/NoWorkflow on the first
	// Running reconcile (always-present thereafter — the phase where a workflow
	// can exist).
	if node.Status.Phase != seiv1alpha1.PhaseRunning {
		return false, ctrl.Result{}, false, nil
	}
	seedWorkflowInProgress(node)
	return r.maybeAdoptWorkflow(ctx, node, before, statusBase)
}

// driveAdoptedWorkflow resolves the adopted workflow by UID and advances,
// finalizes, releases, or parks it. It never patches the node's own status —
// node mutations ride the caller's flush (except deletion/release helpers,
// which manage the workflow object directly).
func (r *SeiNodeReconciler) driveAdoptedWorkflow(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
) (suppress bool, result ctrl.Result, handled bool, err error) {
	ptr := node.Status.AdoptedWorkflow
	wf := &seiv1alpha1.SeiNodeTaskWorkflow{}
	getErr := r.Get(ctx, types.NamespacedName{Namespace: node.Namespace, Name: ptr.Name}, wf)
	if apierrors.IsNotFound(getErr) || (getErr == nil && wf.UID != ptr.UID) {
		// The adopted workflow is gone or was replaced by a same-named object.
		// Clear the pointer and resume drift.
		node.Status.AdoptedWorkflow = nil
		setWorkflowInProgress(node, metav1.ConditionFalse, seiv1alpha1.ReasonNoWorkflow,
			"adopted workflow no longer present")
		return false, ctrl.Result{}, false, nil
	}
	if getErr != nil {
		return true, ctrl.Result{}, false, getErr
	}

	if !wf.DeletionTimestamp.IsZero() {
		return r.finalizeWorkflow(ctx, node, wf)
	}

	// Terminal handling. Success releases (finalizer off, pointer clear);
	// failure parks the node held (finalizer + pointer retained) until an
	// operator re-runs or deletes the workflow.
	if wf.Status.Plan != nil {
		switch wf.Status.Plan.Phase {
		case seiv1alpha1.TaskPlanComplete:
			if err := r.releaseCompletedWorkflow(ctx, node, wf); err != nil {
				return true, ctrl.Result{}, false, err
			}
			return false, ctrl.Result{}, false, nil
		case seiv1alpha1.TaskPlanFailed:
			setWorkflowInProgress(node, metav1.ConditionTrue, seiv1alpha1.ReasonWorkflowFailedHeld,
				"workflow failed; node held pending re-run or deletion")
			return true, ctrl.Result{RequeueAfter: statusPollInterval}, false, nil
		}
	}

	setWorkflowInProgress(node, metav1.ConditionTrue, seiv1alpha1.ReasonWorkflowRunning,
		fmt.Sprintf("driving workflow %s", wf.Name))

	// Ensure the deletion gate on EVERY drive step, not just at adoption: an
	// interrupted adoption (crash or optimistic-lock conflict between the
	// pointer commit and the finalizer add) must not resume into destructive
	// execution ungated. Idempotent.
	if err := r.ensureWorkflowFinalizer(ctx, wf); err != nil {
		return true, ctrl.Result{}, false, err
	}

	// Atomic creation: a pointer with no persisted plan means adoption was
	// interrupted before the plan flush. Rebuild and persist, no execution.
	if wf.Status.Plan == nil {
		if err := r.buildAndPersistWorkflowPlan(ctx, node, wf); err != nil {
			return true, ctrl.Result{}, false, err
		}
		return true, planner.ResultRequeueImmediate, false, nil
	}

	return r.executeWorkflow(ctx, node, wf)
}

// executeWorkflow drives one step of the workflow's plan via the generic
// executor, mirrors terminal state into the workflow's phase/conditions, and
// patches the workflow status under its own optimistic lock. On completion it
// releases the node (finalizer off, pointer clear); on failure it parks held.
func (r *SeiNodeReconciler) executeWorkflow(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	wf *seiv1alpha1.SeiNodeTaskWorkflow,
) (suppress bool, result ctrl.Result, handled bool, err error) {
	wfBefore := wf.DeepCopy()
	wfBase := client.MergeFromWithOptions(wfBefore, client.MergeFromWithOptimisticLock{})

	wf.Status.ObservedGeneration = wf.Generation
	if wf.Status.Phase == "" || wf.Status.Phase == seiv1alpha1.SeiNodeTaskWorkflowPhasePending {
		wf.Status.Phase = seiv1alpha1.SeiNodeTaskWorkflowPhaseRunning
	}

	exec := &planner.Executor[*seiv1alpha1.SeiNodeTaskWorkflow]{
		ConfigFor: func(ctx context.Context, w *seiv1alpha1.SeiNodeTaskWorkflow) task.ExecutionConfig {
			return r.WorkflowConfigFor(ctx, node, w)
		},
	}
	result, execErr := exec.ExecutePlan(ctx, wf, wf.Status.Plan)
	if execErr != nil {
		// Transient executor errors carry a requeue in result and are logged by
		// the executor. Keep the workflow-status flush and requeue rather than
		// bubbling a hard error (which would skip the node condition flush).
		log.FromContext(ctx).Error(execErr, "workflow plan execution error", "workflow", wf.Name)
	}

	var terminalPhase seiv1alpha1.TaskPlanPhase
	if wf.Status.Plan != nil {
		switch wf.Status.Plan.Phase {
		case seiv1alpha1.TaskPlanComplete:
			terminalPhase = seiv1alpha1.TaskPlanComplete
			wf.Status.Phase = seiv1alpha1.SeiNodeTaskWorkflowPhaseComplete
			setWorkflowCondition(wf, seiv1alpha1.ConditionSeiNodeTaskWorkflowReady,
				metav1.ConditionTrue, "WorkflowComplete", "recipe completed")
		case seiv1alpha1.TaskPlanFailed:
			terminalPhase = seiv1alpha1.TaskPlanFailed
			wf.Status.Phase = seiv1alpha1.SeiNodeTaskWorkflowPhaseFailed
			setWorkflowCondition(wf, seiv1alpha1.ConditionSeiNodeTaskWorkflowFailed,
				metav1.ConditionTrue, "WorkflowFailed", planFailureDetail(wf.Status.Plan))
		}
	}

	if !apiequality.Semantic.DeepEqual(wfBefore.Status, wf.Status) {
		if err := r.Status().Patch(ctx, wf, wfBase); err != nil {
			return true, ctrl.Result{}, false, fmt.Errorf("patching workflow status: %w", err)
		}
	}

	switch terminalPhase {
	case seiv1alpha1.TaskPlanComplete:
		r.Recorder.Eventf(wf, corev1.EventTypeNormal, "WorkflowComplete",
			"workflow %s completed on node %s", wf.Name, node.Name)
		// Workflow write (Complete) is now durable; release the node (finalizer
		// off, pointer clear). A crash between the two converges: the terminal
		// check at the top of the next reconcile re-runs releaseCompletedWorkflow.
		if err := r.releaseCompletedWorkflow(ctx, node, wf); err != nil {
			return true, ctrl.Result{}, false, err
		}
		return false, ctrl.Result{}, false, nil
	case seiv1alpha1.TaskPlanFailed:
		r.Recorder.Eventf(wf, corev1.EventTypeWarning, "WorkflowFailed",
			"workflow %s failed on node %s; node held", wf.Name, node.Name)
		setWorkflowInProgress(node, metav1.ConditionTrue, seiv1alpha1.ReasonWorkflowFailedHeld,
			"workflow failed; node held pending re-run or deletion")
		return true, ctrl.Result{RequeueAfter: statusPollInterval}, false, nil
	}
	return true, result, false, nil
}

// maybeAdoptWorkflow runs on a node with no adoption pointer. It reaps
// completed-workflow finalizers, seeds candidate status (even when the node is
// non-idle), enforces refusals/requirePhase, and adopts the oldest eligible
// pending workflow node-first.
func (r *SeiNodeReconciler) maybeAdoptWorkflow(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	before *seiv1alpha1.SeiNode,
	statusBase client.Patch,
) (suppress bool, result ctrl.Result, handled bool, err error) {
	// GC safety net: strip the finalizer from completed workflows targeting
	// this node so a GitOps prune isn't wedged Terminating forever.
	if err := r.reapCompletedWorkflowFinalizers(ctx, node); err != nil {
		return false, ctrl.Result{}, false, err
	}

	candidates, err := r.pendingWorkflowsForNode(ctx, node)
	if err != nil {
		return false, ctrl.Result{}, false, err
	}
	if len(candidates) == 0 {
		return false, ctrl.Result{}, false, nil
	}

	// Only a full/RPC node is an eligible target; every other role is structurally
	// ineligible (CEL cannot see the target). Fail terminally so
	// `kubectl wait --for=condition=Failed` resolves rather than parking Pending
	// forever. See ineligibleWorkflowRole for the per-role rationale.
	if role := ineligibleWorkflowRole(node); role != "" {
		for i := range candidates {
			r.failWorkflow(ctx, &candidates[i], seiv1alpha1.ReasonWorkflowTargetRejected,
				fmt.Sprintf("%s target is ineligible; workflows target full/RPC nodes only", role))
		}
		return false, ctrl.Result{}, false, nil
	}

	winner := &candidates[0]
	// Seed queued status on the losers regardless of whether the node is idle,
	// so a workflow created mid-image-roll shows Pending + conditions.
	for i := 1; i < len(candidates); i++ {
		r.seedWorkflowAdoption(ctx, &candidates[i], metav1.ConditionFalse,
			seiv1alpha1.ReasonWorkflowQueued, "another workflow is queued ahead on this node")
	}

	// Refuse a paused target. Aligned on spec.Paused (the source of truth); the
	// Reconcile freeze on spec.Paused normally returns before we get here, so
	// this is belt-and-braces for a future caller.
	if node.Spec.Paused {
		r.seedWorkflowAdoption(ctx, winner, metav1.ConditionFalse,
			seiv1alpha1.ReasonWorkflowTargetNotReady, "target is paused")
		return false, ctrl.Result{}, false, nil
	}

	// Non-idle: the node has an active drift plan (e.g. an image roll). Seed the
	// winner waiting; adoption waits for the node to return to steady state.
	if node.Status.Plan != nil {
		r.seedWorkflowAdoption(ctx, winner, metav1.ConditionFalse,
			seiv1alpha1.ReasonWorkflowTargetNotReady, "target has an active plan")
		return false, ctrl.Result{}, false, nil
	}

	// Honor spec.target.requirePhase (default Running): the node must be in the
	// required phase to adopt. On timeout the workflow fails terminally.
	if res, gated := r.gateOnRequirePhase(ctx, winner, node); gated {
		return false, res, false, nil
	}

	// Compile the plan before committing adoption — pure, no side effects — so
	// an un-compilable recipe (fail-closed witness floor, validation) fails the
	// workflow without ever holding the node.
	plan, planErr := buildWorkflowPlan(node, winner)
	if planErr != nil {
		r.failWorkflow(ctx, winner, seiv1alpha1.ReasonWorkflowPlanBuildFailed, planErr.Error())
		return false, ctrl.Result{}, false, nil
	}

	// Node-first commit: stamp and flush the pointer before persisting the
	// plan, so a restart re-adopts deterministically by UID.
	node.Status.AdoptedWorkflow = &seiv1alpha1.AdoptedWorkflowRef{
		Name:      winner.Name,
		UID:       winner.UID,
		AdoptedAt: metav1.Now(),
	}
	setWorkflowInProgress(node, metav1.ConditionTrue, seiv1alpha1.ReasonWorkflowRunning,
		fmt.Sprintf("adopted workflow %s", winner.Name))
	if !apiequality.Semantic.DeepEqual(before.Status, node.Status) {
		if err := r.Status().Patch(ctx, node, statusBase); err != nil {
			return false, ctrl.Result{}, false, fmt.Errorf("committing adoption pointer: %w", err)
		}
	}

	// Gate deletion before any destructive step runs (also ensured on every
	// drive step; this is the immediate add).
	if err := r.ensureWorkflowFinalizer(ctx, winner); err != nil {
		return true, ctrl.Result{}, false, err
	}
	if err := r.persistWorkflowPlan(ctx, winner, plan); err != nil {
		return true, ctrl.Result{}, false, err
	}
	r.Recorder.Eventf(winner, corev1.EventTypeNormal, "WorkflowAdopted", "adopted by node %s", node.Name)

	// Atomic creation: no execution on the adopting reconcile.
	return true, planner.ResultRequeueImmediate, true, nil
}

// gateOnRequirePhase enforces spec.target.requirePhase before adoption. It
// returns (requeueResult, gated): gated=true means the node is not yet in the
// required phase (still waiting, or the workflow was just failed on timeout) —
// the caller must not adopt. gated=false means the phase is met; proceed.
func (r *SeiNodeReconciler) gateOnRequirePhase(
	ctx context.Context,
	wf *seiv1alpha1.SeiNodeTaskWorkflow,
	node *seiv1alpha1.SeiNode,
) (result ctrl.Result, gated bool) {
	requirePhase := wf.Spec.Target.RequirePhase
	if requirePhase == "" {
		requirePhase = seiv1alpha1.PhaseRunning
	}
	if node.Status.Phase == requirePhase {
		if wf.Status.TargetFirstObservedAt != nil {
			if err := r.patchWorkflowStatus(ctx, wf, func(w *seiv1alpha1.SeiNodeTaskWorkflow) {
				w.Status.TargetFirstObservedAt = nil
			}); err != nil {
				log.FromContext(ctx).Error(err, "clearing workflow target-observed timestamp", "workflow", wf.Name)
			}
		}
		return ctrl.Result{}, false
	}

	now := metav1.Now()
	timeout := defaultWorkflowRequirePhaseTimeout
	if wf.Spec.Target.RequirePhaseTimeout != nil {
		timeout = wf.Spec.Target.RequirePhaseTimeout.Duration
	}
	if wf.Status.TargetFirstObservedAt != nil && now.Sub(wf.Status.TargetFirstObservedAt.Time) > timeout {
		r.failWorkflow(ctx, wf, seiv1alpha1.ReasonWorkflowTargetPhaseTimeout,
			fmt.Sprintf("target %s did not reach phase %s within %s (current: %s)",
				node.Name, requirePhase, timeout, node.Status.Phase))
		return ctrl.Result{}, true
	}

	if err := r.patchWorkflowStatus(ctx, wf, func(w *seiv1alpha1.SeiNodeTaskWorkflow) {
		if w.Status.TargetFirstObservedAt == nil {
			t := now
			w.Status.TargetFirstObservedAt = &t
		}
		if w.Status.Phase == "" {
			w.Status.Phase = seiv1alpha1.SeiNodeTaskWorkflowPhasePending
		}
		setWorkflowCondition(w, seiv1alpha1.ConditionSeiNodeTaskWorkflowAdopted, metav1.ConditionFalse,
			seiv1alpha1.ReasonWorkflowTargetNotReady,
			fmt.Sprintf("waiting for target %s to reach %s (current: %s)", node.Name, requirePhase, node.Status.Phase))
		seedWorkflowTerminalConditions(w)
	}); err != nil {
		log.FromContext(ctx).Error(err, "seeding workflow requirePhase-wait status", "workflow", wf.Name)
	}
	return ctrl.Result{RequeueAfter: workflowTargetWaitInterval}, true
}

// finalizeWorkflow handles a workflow whose deletion was requested. It lifts
// the hold only when the data state is verified safe (or the force annotation
// is set), then clears the node pointer and removes the finalizer.
func (r *SeiNodeReconciler) finalizeWorkflow(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	wf *seiv1alpha1.SeiNodeTaskWorkflow,
) (suppress bool, result ctrl.Result, handled bool, err error) {
	if !controllerutil.ContainsFinalizer(wf, seiv1alpha1.SeiNodeTaskWorkflowFinalizer) {
		node.Status.AdoptedWorkflow = nil
		setWorkflowInProgress(node, metav1.ConditionFalse, seiv1alpha1.ReasonNoWorkflow,
			"adopted workflow deleted")
		return false, ctrl.Result{}, false, nil
	}

	forced := wf.Annotations[seiv1alpha1.WorkflowForceDeleteAnnotation] != ""
	safe, reason := r.verifyWorkflowDataSafe(ctx, node)
	if !forced && !safe {
		// Fail closed: keep the node held until the operator sets the force
		// annotation or re-runs the workflow. Every failure direction lands held.
		setWorkflowInProgress(node, metav1.ConditionTrue, seiv1alpha1.ReasonWorkflowFailedHeld,
			fmt.Sprintf("deletion held: %s", reason))
		r.Recorder.Eventf(wf, corev1.EventTypeWarning, "WorkflowDeleteHeld",
			"refusing to release node %s: %s (set %s to force)", node.Name, reason,
			seiv1alpha1.WorkflowForceDeleteAnnotation)
		return true, ctrl.Result{RequeueAfter: statusPollInterval}, false, nil
	}

	node.Status.AdoptedWorkflow = nil
	setWorkflowInProgress(node, metav1.ConditionFalse, seiv1alpha1.ReasonNoWorkflow,
		"workflow deleted; hold released")
	if err := r.patchWorkflowFinalizer(ctx, wf, false); err != nil {
		return true, ctrl.Result{}, false, err
	}
	if forced {
		r.Recorder.Eventf(wf, corev1.EventTypeWarning, "WorkflowForceDeleted",
			"force-deleted; node %s hold released without data verification", node.Name)
	}
	return false, ctrl.Result{}, false, nil
}

// releaseCompletedWorkflow removes the deletion gate and clears the node
// pointer after a workflow completes. Ordered workflow-write-then-pointer-clear:
// the finalizer removal is durable before the pointer clear (which rides the
// caller's flush), and either crash order converges (the reap pass and the
// terminal-Complete check both re-run this).
func (r *SeiNodeReconciler) releaseCompletedWorkflow(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	wf *seiv1alpha1.SeiNodeTaskWorkflow,
) error {
	if err := r.patchWorkflowFinalizer(ctx, wf, false); err != nil {
		return err
	}
	node.Status.AdoptedWorkflow = nil
	setWorkflowInProgress(node, metav1.ConditionFalse, seiv1alpha1.ReasonNoWorkflow, "workflow completed")
	return nil
}

// verifyWorkflowDataSafe reports whether the target's data directory is in a
// safe state to release the hold: either fully wiped (a clean state-sync boot)
// or untouched since adoption. A wiped data/ contains exactly one file — a
// fresh empty priv_validator_state.json — which counts as wiped, not touched.
//
// The data-state query is not yet available from the sidecar, so this fails
// closed: deleting an adopted workflow requires the force annotation. This
// matches the fail-closed invariant (every failure direction lands on held).
func (r *SeiNodeReconciler) verifyWorkflowDataSafe(_ context.Context, _ *seiv1alpha1.SeiNode) (safe bool, reason string) {
	return false, "data-state verification unavailable"
}

// ensureWorkflowFinalizer idempotently adds the deletion-gate finalizer.
func (r *SeiNodeReconciler) ensureWorkflowFinalizer(ctx context.Context, wf *seiv1alpha1.SeiNodeTaskWorkflow) error {
	return r.patchWorkflowFinalizer(ctx, wf, true)
}

// patchWorkflowFinalizer adds or removes the deletion-gate finalizer via a
// merge Patch (not Update): a full-object Update round-trips spec.target /
// spec.stateSync in a normalized form that the strict-equality immutability
// CEL rules reject as a spec change. A metadata-only Patch leaves the spec
// bytes untouched, so the transition rules see self==oldSelf and pass.
func (r *SeiNodeReconciler) patchWorkflowFinalizer(ctx context.Context, wf *seiv1alpha1.SeiNodeTaskWorkflow, add bool) error {
	patch := client.MergeFrom(wf.DeepCopy())
	changed := false
	if add {
		changed = controllerutil.AddFinalizer(wf, seiv1alpha1.SeiNodeTaskWorkflowFinalizer)
	} else {
		changed = controllerutil.RemoveFinalizer(wf, seiv1alpha1.SeiNodeTaskWorkflowFinalizer)
	}
	if !changed {
		return nil
	}
	if err := r.Patch(ctx, wf, patch); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("patching workflow finalizer (add=%t): %w", add, err)
	}
	return nil
}

// reapCompletedWorkflowFinalizers strips the finalizer from Complete workflows
// targeting this node. Failed workflows are left alone — they still hold the
// node and release only through finalizeWorkflow's safety check.
func (r *SeiNodeReconciler) reapCompletedWorkflowFinalizers(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	list := &seiv1alpha1.SeiNodeTaskWorkflowList{}
	if err := r.List(ctx, list,
		client.InNamespace(node.Namespace),
		client.MatchingFields{workflowTargetNodeIndex: node.Name},
	); err != nil {
		return err
	}
	for i := range list.Items {
		wf := &list.Items[i]
		if wf.Status.Phase != seiv1alpha1.SeiNodeTaskWorkflowPhaseComplete {
			continue
		}
		if err := r.patchWorkflowFinalizer(ctx, wf, false); err != nil && !apierrors.IsConflict(err) {
			return fmt.Errorf("reaping finalizer from completed workflow %s: %w", wf.Name, err)
		}
	}
	return nil
}

// releaseWorkflowFinalizersForNode strips the finalizer from every workflow
// targeting a node being deleted. A gone node has no seid to hold, so the
// deletion gate would otherwise deadlock the workflows permanently.
func (r *SeiNodeReconciler) releaseWorkflowFinalizersForNode(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	list := &seiv1alpha1.SeiNodeTaskWorkflowList{}
	if err := r.List(ctx, list,
		client.InNamespace(node.Namespace),
		client.MatchingFields{workflowTargetNodeIndex: node.Name},
	); err != nil {
		return err
	}
	for i := range list.Items {
		wf := &list.Items[i]
		if err := r.patchWorkflowFinalizer(ctx, wf, false); err != nil && !apierrors.IsConflict(err) {
			return fmt.Errorf("releasing finalizer from workflow %s on node delete: %w", wf.Name, err)
		}
	}
	return nil
}

// pendingWorkflowsForNode lists non-terminal, non-deleting workflows targeting
// the node, sorted by (creationTimestamp, name) for a stable first-wins order.
func (r *SeiNodeReconciler) pendingWorkflowsForNode(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
) ([]seiv1alpha1.SeiNodeTaskWorkflow, error) {
	list := &seiv1alpha1.SeiNodeTaskWorkflowList{}
	if err := r.List(ctx, list,
		client.InNamespace(node.Namespace),
		client.MatchingFields{workflowTargetNodeIndex: node.Name},
	); err != nil {
		return nil, err
	}
	out := make([]seiv1alpha1.SeiNodeTaskWorkflow, 0, len(list.Items))
	for i := range list.Items {
		w := list.Items[i]
		if !w.DeletionTimestamp.IsZero() {
			continue
		}
		if w.Status.Phase == seiv1alpha1.SeiNodeTaskWorkflowPhaseComplete ||
			w.Status.Phase == seiv1alpha1.SeiNodeTaskWorkflowPhaseFailed {
			continue
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].CreationTimestamp, out[j].CreationTimestamp
		if ti.Equal(&tj) {
			return out[i].Name < out[j].Name
		}
		return ti.Before(&tj)
	})
	return out, nil
}

// buildWorkflowPlan compiles a workflow recipe against the resolved target.
func buildWorkflowPlan(node *seiv1alpha1.SeiNode, wf *seiv1alpha1.SeiNodeTaskWorkflow) (*seiv1alpha1.TaskPlan, error) {
	wp, err := planner.WorkflowPlannerFor(wf)
	if err != nil {
		return nil, err
	}
	if err := wp.Validate(node, wf); err != nil {
		return nil, err
	}
	return wp.BuildPlan(node, wf)
}

// buildAndPersistWorkflowPlan rebuilds and persists a plan for a workflow whose
// pointer is set but plan flush was interrupted.
func (r *SeiNodeReconciler) buildAndPersistWorkflowPlan(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	wf *seiv1alpha1.SeiNodeTaskWorkflow,
) error {
	plan, err := buildWorkflowPlan(node, wf)
	if err != nil {
		r.failWorkflow(ctx, wf, seiv1alpha1.ReasonWorkflowPlanBuildFailed, err.Error())
		return nil
	}
	return r.persistWorkflowPlan(ctx, wf, plan)
}

// persistWorkflowPlan writes the compiled plan and Running phase to the
// workflow status under its own optimistic lock.
func (r *SeiNodeReconciler) persistWorkflowPlan(
	ctx context.Context,
	wf *seiv1alpha1.SeiNodeTaskWorkflow,
	plan *seiv1alpha1.TaskPlan,
) error {
	return r.patchWorkflowStatus(ctx, wf, func(w *seiv1alpha1.SeiNodeTaskWorkflow) {
		w.Status.Plan = plan
		w.Status.Phase = seiv1alpha1.SeiNodeTaskWorkflowPhaseRunning
		w.Status.TargetFirstObservedAt = nil
		setWorkflowCondition(w, seiv1alpha1.ConditionSeiNodeTaskWorkflowAdopted,
			metav1.ConditionTrue, seiv1alpha1.ReasonWorkflowAdopted, "plan compiled and persisted")
		seedWorkflowTerminalConditions(w)
	})
}

// ineligibleWorkflowRole returns the target's role name when it may not be a
// state-sync / giga-migration workflow target, or "" when it is eligible. Only a
// full/RPC node (spec.fullNode, which absorbs the RPC role) is eligible; every
// other role is refused: validators and archive nodes block-sync and never restore
// from a snapshot, and a replayer is an ephemeral restore workload a re-bootstrap
// recipe would destroy. It is an allowlist by design — a node mode added later is
// refused until explicitly permitted here, rather than silently becoming eligible.
// The role is read from the typed spec sub-struct, the authoritative source
// deriveRole classifies from, so the check holds even when a CR's derived
// sei.io/role label is absent or stale.
func ineligibleWorkflowRole(node *seiv1alpha1.SeiNode) string {
	if node.Spec.FullNode != nil {
		return "" // full/RPC node — the only eligible target
	}
	switch {
	case node.Spec.Validator != nil:
		return "validator"
	case node.Spec.Archive != nil:
		return "archive"
	case node.Spec.Replayer != nil:
		return "replayer"
	default:
		return "non-full" // unset/unknown mode — refused by the allowlist
	}
}

// failWorkflow marks a workflow terminally Failed. It does not touch the node.
func (r *SeiNodeReconciler) failWorkflow(ctx context.Context, wf *seiv1alpha1.SeiNodeTaskWorkflow, reason, msg string) {
	if wf.Status.Phase == seiv1alpha1.SeiNodeTaskWorkflowPhaseFailed {
		return
	}
	if err := r.patchWorkflowStatus(ctx, wf, func(w *seiv1alpha1.SeiNodeTaskWorkflow) {
		w.Status.Phase = seiv1alpha1.SeiNodeTaskWorkflowPhaseFailed
		setWorkflowCondition(w, seiv1alpha1.ConditionSeiNodeTaskWorkflowFailed, metav1.ConditionTrue, reason, msg)
		seedWorkflowTerminalConditions(w)
	}); err != nil {
		log.FromContext(ctx).Error(err, "marking workflow failed", "workflow", wf.Name)
		return
	}
	r.Recorder.Event(wf, corev1.EventTypeWarning, reason, msg)
}

// seedWorkflowAdoption stamps a not-yet-adopted workflow's Pending phase and
// Adopted condition (transition-only, so steady state is a no-op).
func (r *SeiNodeReconciler) seedWorkflowAdoption(
	ctx context.Context,
	wf *seiv1alpha1.SeiNodeTaskWorkflow,
	status metav1.ConditionStatus,
	reason, msg string,
) {
	if err := r.patchWorkflowStatus(ctx, wf, func(w *seiv1alpha1.SeiNodeTaskWorkflow) {
		if w.Status.Phase == "" {
			w.Status.Phase = seiv1alpha1.SeiNodeTaskWorkflowPhasePending
		}
		setWorkflowCondition(w, seiv1alpha1.ConditionSeiNodeTaskWorkflowAdopted, status, reason, msg)
		seedWorkflowTerminalConditions(w)
	}); err != nil {
		log.FromContext(ctx).Error(err, "seeding workflow adoption status", "workflow", wf.Name)
	}
}

// patchWorkflowStatus applies mutate and, if the status changed, flushes it
// under an optimistic lock. ObservedGeneration is always stamped.
func (r *SeiNodeReconciler) patchWorkflowStatus(
	ctx context.Context,
	wf *seiv1alpha1.SeiNodeTaskWorkflow,
	mutate func(*seiv1alpha1.SeiNodeTaskWorkflow),
) error {
	wfBefore := wf.DeepCopy()
	wfBase := client.MergeFromWithOptions(wfBefore, client.MergeFromWithOptimisticLock{})
	wf.Status.ObservedGeneration = wf.Generation
	mutate(wf)
	if apiequality.Semantic.DeepEqual(wfBefore.Status, wf.Status) {
		return nil
	}
	if err := r.Status().Patch(ctx, wf, wfBase); err != nil {
		return fmt.Errorf("patching workflow status: %w", err)
	}
	return nil
}

// seedWorkflowTerminalConditions seeds the latch pair False for discoverability.
func seedWorkflowTerminalConditions(wf *seiv1alpha1.SeiNodeTaskWorkflow) {
	if apimeta.FindStatusCondition(wf.Status.Conditions, seiv1alpha1.ConditionSeiNodeTaskWorkflowReady) == nil {
		setWorkflowCondition(wf, seiv1alpha1.ConditionSeiNodeTaskWorkflowReady,
			metav1.ConditionFalse, "NotComplete", "workflow has not completed")
	}
	if apimeta.FindStatusCondition(wf.Status.Conditions, seiv1alpha1.ConditionSeiNodeTaskWorkflowFailed) == nil {
		setWorkflowCondition(wf, seiv1alpha1.ConditionSeiNodeTaskWorkflowFailed,
			metav1.ConditionFalse, "NotFailed", "workflow has not failed")
	}
}

// setWorkflowCondition upserts an always-present condition on the workflow,
// stamping ObservedGeneration per the repo discipline.
func setWorkflowCondition(wf *seiv1alpha1.SeiNodeTaskWorkflow, condType string, status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&wf.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: wf.Generation,
	})
}

// seedWorkflowInProgress ensures the always-present WorkflowInProgress
// condition exists, seeded False/NoWorkflow on first reconcile.
func seedWorkflowInProgress(node *seiv1alpha1.SeiNode) {
	if apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionWorkflowInProgress) == nil {
		setWorkflowInProgress(node, metav1.ConditionFalse, seiv1alpha1.ReasonNoWorkflow, "no workflow adopted")
	}
}

func setWorkflowInProgress(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, msg string) {
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionWorkflowInProgress,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: node.Generation,
	})
}

// adoptedWorkflowIsExecuting reports whether an adopted workflow is actively
// occupying the node — driving execution rather than parked after failure. The
// node holds initial-STS apply off while this is true: an actively-executing
// workflow may have seid gated with its data directory mid-wipe, and
// reconcileStatefulSet's unconditional SSA would push a spec.image template
// change immediately, defeating the plan-driven drift suppression (which gates
// only replace-pod, not the direct apply). A parked-Failed hold
// (adoptedWorkflowParkedFailed) releases the skip so sidecar hotfixes and
// impostor-STS recovery aren't suspended indefinitely — the readiness gate keeps
// seid held, and replacing a parked node's pod is in the safe interrupt class.
func adoptedWorkflowIsExecuting(node *seiv1alpha1.SeiNode) bool {
	return node.Status.AdoptedWorkflow != nil && !adoptedWorkflowParkedFailed(node)
}

// adoptedWorkflowParkedFailed reports whether the adopted workflow has failed
// and the node is parked held (WorkflowInProgress=True/WorkflowFailedHeld) —
// distinct from an actively-executing hold (reason WorkflowRunning). Read from
// the node's own condition (set on the prior reconcile), so no extra Get: a
// parked-Failed state is stable across reconciles, so the one-reconcile lag is
// immaterial.
func adoptedWorkflowParkedFailed(node *seiv1alpha1.SeiNode) bool {
	cond := apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionWorkflowInProgress)
	return cond != nil && cond.Status == metav1.ConditionTrue && cond.Reason == seiv1alpha1.ReasonWorkflowFailedHeld
}

func planFailureDetail(plan *seiv1alpha1.TaskPlan) string {
	if plan != nil && plan.FailedTaskDetail != nil {
		return fmt.Sprintf("task %s: %s", plan.FailedTaskDetail.Type, plan.FailedTaskDetail.Error)
	}
	return "workflow plan failed"
}

// workflowTargetHandler wakes the target node when a workflow it may adopt is
// created, updated, or deleted. The mirror image of nodetask's handler: a
// workflow names its target node directly, so the map is a single request.
type workflowTargetHandler struct{}

func (h *workflowTargetHandler) Create(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	enqueueWorkflowTarget(e.Object, q)
}
func (h *workflowTargetHandler) Update(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	enqueueWorkflowTarget(e.ObjectNew, q)
}
func (h *workflowTargetHandler) Delete(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	enqueueWorkflowTarget(e.Object, q)
}
func (h *workflowTargetHandler) Generic(ctx context.Context, e event.GenericEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	enqueueWorkflowTarget(e.Object, q)
}

func enqueueWorkflowTarget(obj client.Object, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	wf, ok := obj.(*seiv1alpha1.SeiNodeTaskWorkflow)
	if !ok {
		return
	}
	q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
		Namespace: wf.Namespace,
		Name:      wf.Spec.Target.NodeRef.Name,
	}})
}

var _ handler.EventHandler = (*workflowTargetHandler)(nil)
