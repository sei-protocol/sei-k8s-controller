package seinetwork

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

const (
	networkFinalizerName = "sei.io/seinetwork-finalizer"
	controllerName       = "seinetwork"
	statusPollInterval   = 30 * time.Second
	fieldOwner           = client.FieldOwner("seinetwork-controller")
)

// SeiNetworkReconciler reconciles a SeiNetwork object.
type SeiNetworkReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// PlanExecutor drives network-level task plans (e.g. genesis assembly).
	PlanExecutor planner.PlanExecutor[*seiv1alpha1.SeiNetwork]
}

// +kubebuilder:rbac:groups=sei.io,resources=seinetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinetworks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sei.io,resources=seinetworks/finalizers,verbs=update
// +kubebuilder:rbac:groups=sei.io,resources=seinodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodes/status,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *SeiNetworkReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	network := &seiv1alpha1.SeiNetwork{}
	if err := r.Get(ctx, req.NamespacedName, network); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !network.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, network)
	}

	if err := r.ensureFinalizer(ctx, network); err != nil {
		return ctrl.Result{}, err
	}

	// Snapshot the status before any reconciliation mutates it in memory.
	statusBase := client.MergeFromWithOptions(network.DeepCopy(), client.MergeFromWithOptimisticLock{})
	ns, name := network.Namespace, network.Name

	r.seedAlwaysPresentConditions(network)

	if err := r.syncPausedToChildren(ctx, network, network.Spec.Paused); err != nil {
		logger.Error(err, "propagating paused state to children")
		return ctrl.Result{}, fmt.Errorf("propagating paused state: %w", err)
	}

	if err := r.reconcileInternalService(ctx, network); err != nil {
		logger.Error(err, "reconciling internal RPC service")
		return ctrl.Result{}, fmt.Errorf("reconciling internal RPC service: %w", err)
	}

	if err := r.reconcileSeiNodes(ctx, network); err != nil {
		logger.Error(err, "reconciling SeiNodes")
		return ctrl.Result{}, fmt.Errorf("reconciling SeiNodes: %w", err)
	}

	planResult, planErr := r.reconcilePlan(ctx, network)
	if planErr != nil {
		logger.Error(planErr, "reconciling plan")
		return ctrl.Result{}, fmt.Errorf("reconciling plan: %w", planErr)
	}
	if shouldRequeue(planResult) {
		if err := r.updateStatus(ctx, network, statusBase); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
		}
		return planResult, nil
	}

	if err := r.updateStatus(ctx, network, statusBase); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	emitGroupPhase(ns, name, network.Status.Phase)
	emitGroupReplicas(ctx, ns, name, network.Spec.Replicas, network.Status.ReadyReplicas)

	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}

func (r *SeiNetworkReconciler) ensureFinalizer(ctx context.Context, network *seiv1alpha1.SeiNetwork) error {
	if controllerutil.ContainsFinalizer(network, networkFinalizerName) {
		return nil
	}
	patch := client.MergeFrom(network.DeepCopy())
	controllerutil.AddFinalizer(network, networkFinalizerName)
	return r.Patch(ctx, network, patch)
}

func (r *SeiNetworkReconciler) handleDeletion(ctx context.Context, network *seiv1alpha1.SeiNetwork) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(network, networkFinalizerName) {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFromWithOptions(network.DeepCopy(), client.MergeFromWithOptimisticLock{})
	network.Status.Phase = seiv1alpha1.GroupPhaseTerminating
	if err := r.Status().Patch(ctx, network, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting terminating status: %w", err)
	}

	policy := network.Spec.DeletionPolicy
	if policy == "" {
		policy = seiv1alpha1.DeletionPolicyRetain // kubebuilder default is Retain; keep in sync
	}

	if policy == seiv1alpha1.DeletionPolicyRetain {
		r.Recorder.Event(network, corev1.EventTypeNormal, "RetainResources", "Orphaning child SeiNodes and internal Service")
		if err := r.orphanChildSeiNodes(ctx, network); err != nil {
			return ctrl.Result{}, fmt.Errorf("orphaning child SeiNodes: %w", err)
		}
		if err := r.orphanInternalService(ctx, network); err != nil {
			return ctrl.Result{}, fmt.Errorf("orphaning internal Service: %w", err)
		}
	}

	cleanupGroupMetrics(network.Namespace, network.Name)

	finalizerPatch := client.MergeFrom(network.DeepCopy())
	controllerutil.RemoveFinalizer(network, networkFinalizerName)
	return ctrl.Result{}, r.Patch(ctx, network, finalizerPatch)
}

func shouldRequeue(result ctrl.Result) bool {
	return result.RequeueAfter > 0
}

// SetupWithManager sets up the controller with the Manager.
func (r *SeiNetworkReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&seiv1alpha1.SeiNetwork{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&seiv1alpha1.SeiNode{}, builder.WithPredicates(childPhaseChangedPredicate())).
		Owns(&corev1.Service{}).
		Named(controllerName).
		Complete(r)
}
