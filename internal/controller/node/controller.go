package node

import (
	"context"
	"fmt"
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
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
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
	Planner      *planner.NodeResolver
	PlanExecutor planner.PlanExecutor[*seiv1alpha1.SeiNode]
}

// +kubebuilder:rbac:groups=sei.io,resources=seinodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sei.io,resources=seinodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
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

	before := node.DeepCopy()
	statusBase := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
	observedPhase := node.Status.Phase
	prevSidecar := apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionSidecarReady)

	if err := r.reconcilePeers(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling peers: %w", err)
	}

	planAlreadyActive := node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive
	if err := r.Planner.ResolvePlan(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving plan: %w", err)
	}

	r.emitSidecarReadinessEvent(node, prevSidecar)

	var result ctrl.Result
	var execErr error

	if !planAlreadyActive && node.Status.Plan != nil {
		// Requeue immediately so the plan is persisted and visible to
		// observers before execution begins on the next tick.
		result = planner.ResultRequeueImmediate
	} else if node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive {
		result, execErr = r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
	}

	if !apiequality.Semantic.DeepEqual(before.Status, node.Status) {
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
