package node

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	PlanExecutor planner.PlanExecutor[*seiv1alpha1.SeiNode]
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

	// Resolve or resume plan. ResolvePlan either resumes an active plan or
	// builds a new one based on the node's phase, stamping it onto
	// node.Status.Plan (and transitioning Pending → Initializing).
	planAlreadyActive := node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive
	if err := planner.ResolvePlan(node); err != nil {
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
	} else if node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive {
		// Existing plan — execute tasks in-memory.
		result, execErr = r.PlanExecutor.ExecutePlan(ctx, node, node.Status.Plan)
		statusDirty = true
	}

	// Running phase: observe image convergence in-memory.
	if node.Status.Phase == seiv1alpha1.PhaseRunning {
		if dirty, err := r.observeCurrentImage(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("observing current image: %w", err)
		} else if dirty {
			statusDirty = true
		}
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
		nodePhaseTransitions.WithLabelValues(ns, string(observedPhase), string(node.Status.Phase)).Inc()
		emitNodePhase(ns, name, node.Status.Phase)
		r.Recorder.Eventf(node, corev1.EventTypeNormal, "PhaseTransition",
			"Phase changed from %s to %s", observedPhase, node.Status.Phase)

		if node.Status.Phase == seiv1alpha1.PhaseRunning {
			dur := time.Since(node.CreationTimestamp.Time).Seconds()
			nodeInitDuration.WithLabelValues(ns, node.Spec.ChainID).Observe(dur)
			nodeLastInitDuration.WithLabelValues(ns, name).Set(dur)
		}
	}

	// Running nodes with no active plan requeue on a steady-state interval.
	// Spec changes trigger immediate reconciles via GenerationChangedPredicate.
	if node.Status.Phase == seiv1alpha1.PhaseRunning && (node.Status.Plan == nil || node.Status.Plan.Phase != seiv1alpha1.TaskPlanActive) {
		return ctrl.Result{RequeueAfter: statusPollInterval}, nil
	}

	return result, nil
}

// observeCurrentImage checks whether the StatefulSet rollout has completed
// and stamps status.currentImage in-memory. Returns true if the image changed.
func (r *SeiNodeReconciler) observeCurrentImage(ctx context.Context, node *seiv1alpha1.SeiNode) (bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if sts.Status.ObservedGeneration < sts.Generation {
		return false, nil
	}
	if sts.Spec.Replicas == nil || sts.Status.UpdatedReplicas < *sts.Spec.Replicas {
		return false, nil
	}

	if node.Status.CurrentImage != node.Spec.Image {
		node.Status.CurrentImage = node.Spec.Image
		return true, nil
	}
	return false, nil
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
