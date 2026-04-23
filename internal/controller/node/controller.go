package node

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/metric"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	nodeFinalizerName     = "sei.io/seinode-finalizer"
	seiNodeControllerName = "seinode"
	statusPollInterval    = 30 * time.Second
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
	PlanExecutor planner.PlanExecutor[*seiv1alpha1.SeiNode]

	// BuildSidecarClient constructs a sidecar client targeting the node's
	// headless Service. Used by probeSidecarHealth to detect a stale
	// mark-ready gate after pod replacement. Same function the
	// PlanExecutor's ConfigFor wires up.
	BuildSidecarClient func(node *seiv1alpha1.SeiNode) (task.SidecarClient, error)
}

// +kubebuilder:rbac:groups=sei.io,resources=seinodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sei.io,resources=seinodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives the SeiNode lifecycle. All status mutations after the
// finalizer are accumulated in-memory and flushed in a single status patch.
func (r *SeiNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

	// Failed is terminal — nothing to do.
	if node.Status.Phase == seiv1alpha1.PhaseFailed {
		r.Recorder.Eventf(node, corev1.EventTypeWarning, "NodeFailed",
			"SeiNode is in Failed state. Delete and recreate the resource to retry.")
		return ctrl.Result{}, nil
	}

	statusBase := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	observedPhase := node.Status.Phase
	statusDirty := false

	// Pre-plan: resolve label-based peers so plan params have fresh data.
	if dirty, err := r.reconcilePeers(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling peers: %w", err)
	} else if dirty {
		statusDirty = true
	}

	// Pre-plan: probe the sidecar's Healthz when Running. A 503 here
	// indicates the mark-ready gate got re-closed by a pod replacement;
	// the planner picks up the SidecarReady=False condition and spawns a
	// one-task MarkReady plan. Probe is skipped during Initializing — the
	// init plan owns sidecar interaction there.
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		if dirty := r.probeSidecarHealth(ctx, node); dirty {
			statusDirty = true
		}
	}

	// Resolve or resume plan. ResolvePlan either resumes an active plan or
	// builds a new one based on the node's phase, stamping it onto
	// node.Status.Plan (and transitioning Pending → Initializing).
	planAlreadyActive := node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive
	if err := planner.ResolvePlan(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving plan: %w", err)
	}

	var result ctrl.Result
	var execErr error

	if !planAlreadyActive && node.Status.Plan != nil {
		// New plan — persist it before executing. Execution starts on the
		// next reconcile. This guarantees external observers see the plan
		// (and any conditions) before side effects occur.
		statusDirty = true
		result = planner.ResultRequeueImmediate
		if isMarkReadyReapplyPlan(node.Status.Plan) {
			r.Recorder.Event(node, corev1.EventTypeNormal, "MarkReadyReapplied",
				"sidecar reported 503; re-issuing mark-ready via a one-task plan")
		}
	} else if node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive {
		// Existing plan — execute tasks in-memory.
		result, execErr = r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
		statusDirty = true
	}

	if statusDirty {
		if err := r.Status().Patch(ctx, node, statusBase); err != nil {
			if execErr != nil {
				log.FromContext(ctx).Error(execErr, "plan execution error lost due to status flush failure")
			}
			return ctrl.Result{}, fmt.Errorf("flushing status: %w", err)
		}
	}

	if execErr != nil {
		return result, execErr
	}

	// Emit metrics/events if the phase changed.
	if node.Status.Phase != observedPhase {
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

	// Running nodes with no active plan requeue on a steady-state interval.
	// Spec changes trigger immediate reconciles via GenerationChangedPredicate.
	if node.Status.Phase == seiv1alpha1.PhaseRunning && (node.Status.Plan == nil || node.Status.Plan.Phase != seiv1alpha1.TaskPlanActive) {
		return ctrl.Result{RequeueAfter: statusPollInterval}, nil
	}

	return result, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SeiNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&seiv1alpha1.SeiNode{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.StatefulSet{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
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

	cleanupNodeMetrics(node.Namespace, node.Name)

	controllerutil.RemoveFinalizer(node, nodeFinalizerName)
	return ctrl.Result{}, r.Update(ctx, node)
}

func (r *SeiNodeReconciler) deleteNodeDataPVC(ctx context.Context, node *seiv1alpha1.SeiNode) error {
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

// probeSidecarHealth queries the sidecar's /v0/healthz and stamps
// ConditionSidecarReady on the node. Returns true iff the condition
// changed. The planner reads this condition on its next reconcile to
// decide whether to spawn a MarkReady re-apply plan.
//
// Three-way signal:
//   - HTTP 200            → True / Ready
//   - HTTP 503            → False / NotReady    (only actionable state)
//   - network error       → Unknown / Unreachable
//   - other (e.g. 4xx/5xx) → Unknown / ProbeError
//
// Only `False/NotReady` triggers a plan. Unknown cases re-probe next tick.
func (r *SeiNodeReconciler) probeSidecarHealth(ctx context.Context, node *seiv1alpha1.SeiNode) bool {
	if r.BuildSidecarClient == nil {
		return false
	}

	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var status metav1.ConditionStatus
	var reason, message, outcome string

	client, err := r.BuildSidecarClient(node)
	if err != nil {
		status = metav1.ConditionUnknown
		reason = "ProbeError"
		message = fmt.Sprintf("failed to construct sidecar client: %v", err)
		outcome = "probe_error"
	} else {
		ready, healthzErr := client.Healthz(probeCtx)
		switch {
		case healthzErr != nil:
			status = metav1.ConditionUnknown
			reason = "Unreachable"
			message = fmt.Sprintf("sidecar Healthz error: %v", healthzErr)
			outcome = "unreachable"
		case ready:
			status = metav1.ConditionTrue
			reason = "Ready"
			message = "sidecar returned 200"
			outcome = "ready"
		default:
			status = metav1.ConditionFalse
			reason = "NotReady"
			message = "sidecar returned 503, re-issuing mark-ready"
			outcome = "not_ready"
		}
	}

	sidecarHealthProbes.Add(ctx, 1,
		metric.WithAttributes(
			observability.AttrController.String(seiNodeControllerName),
			observability.AttrNamespace.String(node.Namespace),
			observability.AttrOutcome.String(outcome),
		),
	)

	prev := findCondition(node, seiv1alpha1.ConditionSidecarReady)
	setSidecarReadyCondition(node, status, reason, message)

	if prev == nil || prev.Status != status || prev.Reason != reason {
		r.emitSidecarReadinessEvent(node, prev, status, reason)
		return true
	}
	return false
}

func setSidecarReadyCondition(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionSidecarReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}

func findCondition(node *seiv1alpha1.SeiNode, t string) *metav1.Condition {
	return apimeta.FindStatusCondition(node.Status.Conditions, t)
}

func (r *SeiNodeReconciler) emitSidecarReadinessEvent(node *seiv1alpha1.SeiNode, prev *metav1.Condition, newStatus metav1.ConditionStatus, newReason string) {
	switch {
	case prev != nil && prev.Status == metav1.ConditionTrue && newStatus == metav1.ConditionFalse:
		r.Recorder.Event(node, corev1.EventTypeWarning, "SidecarReadinessLost",
			"sidecar Healthz returned 503; controller will re-issue mark-ready")
	case newStatus == metav1.ConditionFalse && newReason == "NotReady":
		// First-time False observation (prev was nil or Unknown).
		r.Recorder.Event(node, corev1.EventTypeWarning, "SidecarReadinessLost",
			"sidecar Healthz returned 503; controller will re-issue mark-ready")
	case prev != nil && prev.Status == metav1.ConditionFalse && newStatus == metav1.ConditionTrue:
		r.Recorder.Event(node, corev1.EventTypeNormal, "SidecarReadinessRestored",
			"sidecar Healthz returned 200; mark-ready gate is open")
	}
}

func isMarkReadyReapplyPlan(plan *seiv1alpha1.TaskPlan) bool {
	return plan != nil && len(plan.Tasks) == 1 && plan.Tasks[0].Type == planner.TaskMarkReady
}
