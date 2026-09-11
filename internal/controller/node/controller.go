package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/observability"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
	"github.com/sei-protocol/sei-k8s-controller/internal/peering"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	nodeFinalizerName     = "sei.io/seinode-finalizer"
	seiNodeControllerName = "seinode"
	statusPollInterval    = 30 * time.Second
	// heightReadTimeout bounds one sidecar status read. The sidecar answers
	// /v0/status within its own budget (rpc.statusTimeout + rpc.heightTimeout,
	// 2.5s), so a read that runs this long is a sidecar that is not there;
	// after one the node sits out heightReadBackoff before the next attempt
	// so a cell-wide sidecar outage costs the single reconcile worker one
	// timeout per node per backoff, not per poll. The backoff skips one poll,
	// no more: the network counts a stamp as fresh for three polls
	// (seinetwork.heightReadingMaxAge), so a sidecar that blips once and
	// recovers is re-read before its last stamp ages out.
	heightReadTimeout = 3 * time.Second
	heightReadBackoff = statusPollInterval
)

// PlatformConfig is an alias for platform.Config, used throughout the node
// controller package to avoid repeating the full import path.
type PlatformConfig = platform.Config

// SeiNodeReconciler reconciles a SeiNode object.
type SeiNodeReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Recorder     record.EventRecorder
	Platform     PlatformConfig
	Planner      *planner.NodeResolver
	PlanExecutor planner.PlanExecutor[*seiv1alpha1.SeiNode]
	// EC2Peers resolves EC2Tags peer sources via the AWS EC2 API. Nil is
	// tolerated: an EC2Tags source declared with no resolver preserves the
	// prior peer set rather than erroring (EC2Tags is vestigial today).
	EC2Peers peering.EC2Resolver
	// WorkflowConfigFor builds the task.ExecutionConfig for an adopted
	// workflow's plan execution. The target node (this reconcile's node) is
	// passed so the sidecar client and Resource resolve to it. Wired by
	// cmd/main.go with the same factories as the node plan executor.
	WorkflowConfigFor func(ctx context.Context, node *seiv1alpha1.SeiNode, wf *seiv1alpha1.SeiNodeTaskWorkflow) task.ExecutionConfig
	// HeightReader reads a Running node's committed height each steady-state
	// poll. Nil (tests) skips the read and leaves status.committedHeight as
	// it was.
	HeightReader HeightReader

	// heightReadRetryAt holds, per node UID, the earliest time the height
	// read may run again after a failure.
	heightReadRetryAt sync.Map
}

// HeightReader returns a node's committed height from its sidecar.
type HeightReader func(ctx context.Context, node *seiv1alpha1.SeiNode) (int64, error)

// +kubebuilder:rbac:groups=sei.io,resources=seinodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sei.io,resources=seinodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=sei.io,resources=seinodetaskworkflows,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sei.io,resources=seinodetaskworkflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sei.io,resources=seinodetaskworkflows/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// configmaps read-only here; write is node-namespace-scoped via platform Roles.
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Reconcile drives the SeiNode lifecycle. All status mutations after the
// finalizer are accumulated in-memory and flushed in a single status patch.
func (r *SeiNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, retErr error) {
	node := &seiv1alpha1.SeiNode{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if node.Status.Phase != "" {
		emitNodePhase(node.Namespace, node.Name, node.Status.Phase)
	}

	if !node.DeletionTimestamp.IsZero() {
		return r.handleNodeDeletion(ctx, node)
	}

	// Finalizer is a metadata Update — must happen before we snapshot
	// the status patch base because Update changes resourceVersion.
	if err := r.ensureNodeFinalizer(ctx, node); err != nil {
		return ctrl.Result{}, err
	}

	before := node.DeepCopy()
	statusBase := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
	observedPhase := node.Status.Phase
	prevSidecar := apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionSidecarReady)
	prevStateSync := apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionStateSyncReady)

	setNodePausedCondition(node)

	// flushStatus is the ONLY writer of this node's status for the rest of the
	// reconcile — the adoption commit in maybeAdoptWorkflow goes through it too,
	// which is what lets the backstop below be unconditional.
	//
	// Idempotent in both directions. It no-ops when nothing has changed since
	// the last successful patch, so the backstop cannot double-write a path that
	// already flushed; and a successful patch re-baselines the watermark, so a
	// later write preconditions on the resourceVersion the API just returned
	// rather than one it has already superseded (a stale precondition would
	// surface as a spurious conflict, which is how a naive deferred flush turns
	// a successful workflow adoption into an error).
	flushFailed := false
	flushStatus := func() error {
		if apiequality.Semantic.DeepEqual(before.Status, node.Status) {
			return nil
		}
		if err := r.Status().Patch(ctx, node, statusBase); err != nil {
			flushFailed = true
			return err
		}
		before = node.DeepCopy()
		statusBase = client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
		return nil
	}

	// The backstop, and the reason it is deferred rather than repeated before
	// each return: resolving an always-present condition in memory is not the
	// same as persisting it. Several error returns below attempt no status write
	// of their own (StatefulSet render, peer resolution, workflow handling, a
	// fatal planner error), and a node parked on any of them would keep whatever
	// absence it started with for as long as the error persists — the
	// always-present defect by another route. A flush on the way out closes the
	// whole class, including the next early return someone adds here.
	//
	// The original error always wins: it is why the reconcile ended and what
	// earns the requeue. A backstop failure becomes the returned error only when
	// there is no original error to preserve; when both fail, the flush error is
	// logged and dropped, and the requeue the original error earns re-resolves
	// and re-flushes on the next round. The patch keeps its optimistic lock, so
	// a concurrent writer yields a conflict — never a silent overwrite.
	//
	// Not installed on the deletion path: that returns above, and a terminating
	// node is a separate lifecycle decision (no condition is resolved for it).
	//
	// WHAT THIS MEANS FOR ANY IN-MEMORY STATUS MUTATION BELOW. A mutation
	// upstream of a fallible call is now PERSISTED when that call fails, where it
	// used to be discarded. That is the fix for a resolved condition, and it is
	// safe for anything derived purely from spec plus a completed read — the
	// state-sync gate, the VAC pre-flight, the Paused mirror, the terminal-plan
	// clear. It is NOT safe for a mutation that releases a hold or clears a
	// pointer whose durability depends on a write that has not happened yet: the
	// external write must land FIRST, or a failure persists the release and
	// strands the object it was holding. finalizeWorkflow and
	// releaseCompletedWorkflow both carry that ordering and say why; put any new
	// release on the same side of its write.
	//
	// One accepted consequence: a condition persisted on an error exit skips the
	// paired transition Event, which is emitted further down (see
	// emitSidecarReadinessEvent / emitStateSyncBlockedEvent) and therefore not at
	// all on a path that returns early. The next reconcile then reads its own
	// newly-persisted value as the previous one and sees no transition. The
	// condition is the durable, PromQL-visible contract and is now correct;
	// Events are best-effort and lossy by design, so this trade is deliberate.
	defer func() {
		if flushFailed {
			return // the call site that attempted it already reported the failure
		}
		if err := flushStatus(); err != nil {
			if retErr != nil {
				log.FromContext(ctx).Error(err, "status flush failed on the way out; returning the original reconcile error")
				return
			}
			retErr = fmt.Errorf("flushing status: %w", err)
		}
	}()

	// Resolve the always-present StateSyncReady condition before the Failed and
	// Paused early-returns so it rides the existing flush on every path (Failed
	// flush, Paused flush, and the normal end-of-reconcile patch) — no separate
	// status write. Fail-closed enforcement lives in ResolvePlan, which declines
	// to build a state-sync plan when this condition isn't True; that keeps
	// terminal-plan cleanup and non-state-sync work running. A blocked gate
	// requeues (see end of reconcile) without aborting the steps below.
	stateSyncBlocked := r.reconcileStateSyncGate(node)

	// Same discipline, same placement, same reason: the always-present
	// VolumeAttributesClassReady condition is resolved here so it rides the
	// existing flush on every path — including the paths that run no plan at
	// all, which is where its first home inside ensure-data-pvc left it absent.
	// Read-only; enforcement is the ensure-data-pvc task's provisioning hold.
	r.reconcileVolumeAttributesClass(ctx, node)

	// Failed is terminal — flush any condition updates and exit.
	if node.Status.Phase == seiv1alpha1.PhaseFailed {
		if err := flushStatus(); err != nil {
			return ctrl.Result{}, fmt.Errorf("flushing status on Failed: %w", err)
		}
		r.Recorder.Eventf(node, corev1.EventTypeWarning, "NodeFailed",
			"SeiNode is in Failed state. Delete and recreate the resource to retry.")
		return ctrl.Result{}, nil
	}

	// Hold initial StatefulSet creation while the state-sync gate suppresses
	// the init plan: the plan's EnsureDataPVC task is the only creator of the
	// data PVC the pod mounts by claimName, so an STS created now would strand
	// a Pending pod until the gate opens. Only initial creation is held
	// (Status.StatefulSet == nil) — an existing STS is never touched, and
	// StateSyncBlocksPlan is pre-Running-only, so a Running node's STS keeps
	// syncing through transient syncer-source errors. One narrow stall is
	// accepted: if the impostor branch just cleared Status.StatefulSet and the
	// gate is blocked in the same window, STS re-creation waits for the gate's
	// poll to re-resolve.
	holdInitialSTS := planner.StateSyncBlocksPlan(node) && node.Status.StatefulSet == nil
	// No roll mid-hold: a workflow ACTIVELY executing may have seid gated and its
	// data directory mid-wipe. reconcileStatefulSet's unconditional SSA would
	// push a spec.image template change immediately, defeating the drift
	// suppression below (which only gates plan-driven replace-pod, not the direct
	// apply). Skip the apply only while the adopted workflow is executing; a
	// parked-Failed hold releases the skip so sidecar hotfixes and impostor-STS
	// recovery aren't suspended indefinitely (the readiness gate keeps seid held,
	// and a pod replacement of a parked node is in the safe interrupt class).
	holdForWorkflow := node.Status.AdoptedWorkflow != nil && !adoptedWorkflowParkedFailed(node)
	if !holdInitialSTS && !holdForWorkflow {
		if err := r.reconcileStatefulSet(ctx, node); err != nil {
			// Whatever status was resolved this far is persisted by the flush on the
			// way out, so this return no longer leaves a bare SeiNode behind. The
			// render failure itself is not a condition, so the Event stays the only
			// place it reaches an operator — keep it: `kubectl describe` beats
			// grepping controller logs. Reachable on operator-supplied app-config:
			// infra fields load once at startup, so a seed applied before the
			// controller restarts renders against the old config.
			r.Recorder.Eventf(node, corev1.EventTypeWarning, "StatefulSetRenderFailed",
				"Cannot render the StatefulSet: %v", err)
			return ctrl.Result{}, fmt.Errorf("reconciling statefulset: %w", err)
		}
	}

	if node.Spec.Paused {
		if err := flushStatus(); err != nil {
			return ctrl.Result{}, fmt.Errorf("flushing paused status: %w", err)
		}
		return ctrl.Result{}, nil
	}

	if err := r.reconcilePeers(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling peers: %w", err)
	}

	// Workflow adoption/execution occupies the node's single plan slot. When a
	// workflow occupies the node (driving or parked-held), drift planning and
	// sidecar reapproval are suppressed so an image roll or mark-ready cannot
	// disturb the recipe. The adopting reconcile is self-contained (node-first
	// pointer patch) and returns handled.
	suppressDrift, wfResult, handled, wfErr := r.reconcileWorkflow(ctx, node, flushStatus)
	if wfErr != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling workflow: %w", wfErr)
	}
	if handled {
		return wfResult, nil
	}

	var result ctrl.Result
	var execErr error

	if suppressDrift {
		result = wfResult
	} else {
		if err := r.backfillNodeIsolation(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("backfilling node isolation: %w", err)
		}
		var fatal error
		if result, execErr, fatal = r.resolveDriftPlan(ctx, node, prevSidecar, prevStateSync); fatal != nil {
			return ctrl.Result{}, fatal
		}
	}

	r.observeCommittedHeight(ctx, node, suppressDrift)

	if err := flushStatus(); err != nil {
		if execErr != nil {
			log.FromContext(ctx).Error(execErr, "plan execution error lost due to status flush failure")
		}
		return ctrl.Result{}, fmt.Errorf("flushing status: %w", err)
	}

	if execErr != nil {
		return result, execErr
	}

	r.emitPhaseTransition(ctx, node, observedPhase)

	return steadyStateRequeue(node, result, suppressDrift, stateSyncBlocked), nil
}

// observeCommittedHeight stamps status.committedHeight/committedHeightTime
// from the sidecar on a Running node with no active plan — the same cadence
// steadyStateRequeue polls on. A failed read keeps the previous stamp so the
// network controller ages it out by time rather than seeing a false zero.
func (r *SeiNodeReconciler) observeCommittedHeight(ctx context.Context, node *seiv1alpha1.SeiNode, suppressDrift bool) {
	if r.HeightReader == nil || suppressDrift || node.Status.Phase != seiv1alpha1.PhaseRunning ||
		(node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive) {
		return
	}
	if retryAt, ok := r.heightReadRetryAt.Load(node.UID); ok && time.Now().Before(retryAt.(time.Time)) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, heightReadTimeout)
	defer cancel()
	h, err := r.HeightReader(ctx, node)
	if err != nil {
		r.heightReadRetryAt.Store(node.UID, time.Now().Add(heightReadBackoff))
		log.FromContext(ctx).V(1).Info("committed height unreadable", "error", err, "retryAfter", heightReadBackoff)
		return
	}
	r.heightReadRetryAt.Delete(node.UID)
	now := metav1.Now()
	node.Status.CommittedHeight = &h
	node.Status.CommittedHeightTime = &now
}

// steadyStateRequeue picks the requeue cadence once plan work is done: a
// Running node with no active plan (and a blocked state-sync node, whose syncer
// file is a mounted volume with no watch) polls on statusPollInterval so the
// gate re-resolves. A workflow-occupied node keeps result (its executor
// cadence). IsZero defers to any stronger requeue already set.
func steadyStateRequeue(node *seiv1alpha1.SeiNode, result ctrl.Result, suppressDrift, stateSyncBlocked bool) ctrl.Result {
	if suppressDrift {
		return result
	}
	if node.Status.Phase == seiv1alpha1.PhaseRunning &&
		(node.Status.Plan == nil || node.Status.Plan.Phase != seiv1alpha1.TaskPlanActive) {
		return ctrl.Result{RequeueAfter: statusPollInterval}
	}
	if stateSyncBlocked && result.IsZero() {
		return ctrl.Result{RequeueAfter: statusPollInterval}
	}
	return result
}

// resolveDriftPlan runs the spec-drift plan lifecycle for a node not occupied
// by a workflow: resolve (clearing terminal plans, building drift plans),
// then execute the active plan. It mutates node.Status in-memory and returns
// (requeue result, execErr from the executor, fatal error). A fatal error
// aborts the reconcile before the status flush, matching the prior inline
// behavior; execErr rides the flush and is returned to controller-runtime.
func (r *SeiNodeReconciler) resolveDriftPlan(
	ctx context.Context,
	node *seiv1alpha1.SeiNode,
	prevSidecar, prevStateSync *metav1.Condition,
) (result ctrl.Result, execErr, fatal error) {
	planAlreadyActive := node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive
	// ResolvePlan runs unconditionally: it clears terminal plans and drives
	// non-state-sync work. Its internal fail-closed gate declines to build a
	// state-sync plan when StateSyncReady isn't True.
	if err := r.Planner.ResolvePlan(ctx, node); err != nil {
		if r.Recorder != nil {
			r.Recorder.Eventf(node, corev1.EventTypeWarning, "PlanBuildFailed", "Cannot build node plan: %v", err)
		}
		return ctrl.Result{}, nil, fmt.Errorf("resolving plan: %w", err)
	}

	r.emitSidecarReadinessEvent(node, prevSidecar)
	r.emitStateSyncBlockedEvent(node, prevStateSync)

	if !planAlreadyActive && node.Status.Plan != nil {
		// Requeue immediately so the plan is persisted and visible to observers
		// before execution begins on the next tick.
		result = planner.ResultRequeueImmediate
	} else if node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive {
		result, execErr = r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
	}

	// Set only when Running and never cleared on a transient non-Running: the
	// URLs are identity-derived, so a Running->update->Running cycle keeps them
	// stable and clearing would flap .status.endpoint for consumers.
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		node.Status.Endpoint = composeNodeEndpoints(node)
	}
	return result, execErr, nil
}

// emitPhaseTransition records phase-transition metrics and a PhaseTransition
// Event when the node's phase changed during this reconcile. A no-op when the
// phase is unchanged.
func (r *SeiNodeReconciler) emitPhaseTransition(ctx context.Context, node *seiv1alpha1.SeiNode, observedPhase seiv1alpha1.SeiNodePhase) {
	if node.Status.Phase == observedPhase {
		return
	}
	ns, name := node.Namespace, node.Name
	nodePhaseTransitions.Add(ctx, 1,
		metric.WithAttributes(
			observability.AttrController.String(seiNodeControllerName),
			observability.AttrNamespace.String(ns),
			observability.AttrFromPhase.String(string(observedPhase)),
			observability.AttrToPhase.String(string(node.Status.Phase)),
		),
	)
	emitNodePhase(ns, name, node.Status.Phase)
	r.Recorder.Eventf(node, corev1.EventTypeNormal, "PhaseTransition",
		"Phase changed from %s to %s", observedPhase, node.Status.Phase)

	// Record time spent in the previous phase.
	if node.Status.PhaseTransitionTime != nil && observedPhase != "" {
		dur := time.Since(node.Status.PhaseTransitionTime.Time).Seconds()
		nodePhaseDuration.Record(ctx, dur,
			metric.WithAttributes(
				observability.AttrNamespace.String(ns),
				observability.AttrChainID.String(node.Spec.ChainID),
				observability.AttrPhase.String(string(observedPhase)),
			),
		)
	}
}

// SetupWithManager sets up the controller with the Manager. It indexes
// SeiNodeTaskWorkflows by target node and watches them so a node wakes to
// adopt, drive, or finalize a workflow aimed at it (no GenerationChanged
// predicate: status and deletion transitions must wake the node too).
func (r *SeiNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&seiv1alpha1.SeiNodeTaskWorkflow{}, workflowTargetNodeIndex,
		func(o client.Object) []string {
			wf, ok := o.(*seiv1alpha1.SeiNodeTaskWorkflow)
			if !ok {
				return nil
			}
			return []string{wf.Spec.Target.NodeRef.Name}
		}); err != nil {
		return fmt.Errorf("indexing workflows by target node: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&seiv1alpha1.SeiNode{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.StatefulSet{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Watches(&seiv1alpha1.SeiNodeTaskWorkflow{}, &workflowTargetHandler{}).
		Named(seiNodeControllerName).
		Complete(r)
}

func (r *SeiNodeReconciler) ensureNodeFinalizer(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	if controllerutil.ContainsFinalizer(node, nodeFinalizerName) {
		return nil
	}
	controllerutil.AddFinalizer(node, nodeFinalizerName)
	return r.Update(ctx, node)
}

func (r *SeiNodeReconciler) handleNodeDeletion(ctx context.Context, node *seiv1alpha1.SeiNode) (ctrl.Result, error) {
	r.heightReadRetryAt.Delete(node.UID)
	if !controllerutil.ContainsFinalizer(node, nodeFinalizerName) {
		return ctrl.Result{}, nil
	}

	// Deletion path: separate patch is intentional — this runs before the
	// main reconcile flow and must set Terminating before cleaning up resources.
	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	node.Status.Phase = seiv1alpha1.PhaseTerminating
	if err := r.Status().Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting terminating status: %w", err)
	}

	if err := r.deleteNodeDataPVC(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("deleting data PVC: %w", err)
	}

	// Release the deletion gate on every workflow targeting this node: a gone
	// node has no seid to hold, so the hold finalizer would otherwise deadlock
	// those workflows permanently.
	if err := r.releaseWorkflowFinalizersForNode(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("releasing workflow finalizers: %w", err)
	}

	cleanupNodeMetrics(node.Namespace, node.Name)

	controllerutil.RemoveFinalizer(node, nodeFinalizerName)
	return ctrl.Result{}, r.Update(ctx, node)
}

func (r *SeiNodeReconciler) deleteNodeDataPVC(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	// SigningKey-referenced Secrets are externally managed; the controller
	// never deletes them. No cleanup needed here.

	// Imported PVCs are managed externally — never delete them.
	if node.Spec.DataVolume != nil && node.Spec.DataVolume.Import != nil &&
		node.Spec.DataVolume.Import.PVCName != "" {
		log.FromContext(ctx).Info("skipping data PVC delete for imported volume",
			"pvc", node.Spec.DataVolume.Import.PVCName)
		return nil
	}

	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: noderesource.DataPVCName(node), Namespace: node.Namespace}, pvc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.Delete(ctx, pvc)
}

// setNodePausedCondition mirrors spec.paused into ConditionSeiNodePaused.
func setNodePausedCondition(node *seiv1alpha1.SeiNode) {
	cond := metav1.Condition{
		Type:               seiv1alpha1.ConditionSeiNodePaused,
		ObservedGeneration: node.Generation,
	}
	if node.Spec.Paused {
		cond.Status = metav1.ConditionTrue
		cond.Reason = "Paused"
		cond.Message = "spec.paused is true; reconciliation is frozen"
	} else {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "NotPaused"
		cond.Message = "spec.paused is unset or false"
	}
	apimeta.SetStatusCondition(&node.Status.Conditions, cond)
}

func (r *SeiNodeReconciler) emitSidecarReadinessEvent(node *seiv1alpha1.SeiNode, prev *metav1.Condition) {
	cur := apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionSidecarReady)
	if cur == nil {
		return
	}
	switch {
	case cur.Status == metav1.ConditionFalse && cur.Reason == "NotReady" &&
		(prev == nil || prev.Status != metav1.ConditionFalse):
		r.Recorder.Event(node, corev1.EventTypeWarning, "SidecarReadinessLost",
			"sidecar Healthz returned 503; controller will re-issue mark-ready")
	case cur.Status == metav1.ConditionTrue &&
		prev != nil && prev.Status == metav1.ConditionFalse:
		r.Recorder.Event(node, corev1.EventTypeNormal, "SidecarReadinessRestored",
			"sidecar Healthz returned 200; mark-ready gate is open")
	}
}

// emitStateSyncBlockedEvent fires a StateSyncBlocked Warning once, on the
// transition into fail-closed (StateSyncReady leaving True/absent for a
// fail-closed reason) — not on every requeue. NotApplicable (state-sync
// disabled) never trips it.
func (r *SeiNodeReconciler) emitStateSyncBlockedEvent(node *seiv1alpha1.SeiNode, prev *metav1.Condition) {
	cur := apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionStateSyncReady)
	if cur == nil || cur.Status == metav1.ConditionTrue {
		return
	}
	blockedReason := cur.Reason == seiv1alpha1.ReasonStateSyncNoSyncersConfigured ||
		cur.Reason == seiv1alpha1.ReasonStateSyncSyncerSourceError
	if !blockedReason {
		return
	}
	// Transition = previously True, absent, or a different (non-blocked) reason.
	if prev != nil && prev.Status == cur.Status && prev.Reason == cur.Reason {
		return
	}
	r.Recorder.Eventf(node, corev1.EventTypeWarning, "StateSyncBlocked",
		"state sync enabled but not ready for chain %q (%s); not building plan",
		node.Spec.ChainID, cur.Reason)
}
