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
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	nodeFinalizerName     = "sei.io/seinode-finalizer"
	seiNodeControllerName = "seinode"
	statusPollInterval    = 30 * time.Second
	fieldOwner            = client.FieldOwner("seinode-controller")
)

// PlatformConfig is an alias for platform.Config, used throughout the node
// controller package to avoid repeating the full import path.
type PlatformConfig = platform.Config

// SeiNodeReconciler reconciles a SeiNode object.
type SeiNodeReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	Recorder             record.EventRecorder
	Platform             PlatformConfig
	PlanExecutor         planner.PlanExecutor[*seiv1alpha1.SeiNode]
	BuildSidecarClientFn func(node *seiv1alpha1.SeiNode) task.SidecarClient
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

	if err := r.ensureNodeFinalizer(ctx, node); err != nil {
		return ctrl.Result{}, err
	}

	p, err := planner.ForNode(node)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving planner: %w", err)
	}
	if err := p.Validate(node); err != nil {
		return ctrl.Result{}, fmt.Errorf("validating spec: %w", err)
	}

	// TODO: reconcile peers should become a part of a plan if it needs to be performed.
	if err := r.reconcilePeers(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling peers: %w", err)
	}

	// TODO: this should be a part of a plan and not part of the main reconciliation flow.
	if err := r.ensureNodeDataPVC(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring data PVC: %w", err)
	}

	// TODO: if the plan becomes the abstraction it should be, then p from planner.ForNode should just take an existing
	// plan off the node if one exists, otherwise, create one based on the state it sees.
	switch node.Status.Phase {
	case "", seiv1alpha1.PhasePending:
		return r.reconcilePending(ctx, node, p)
	case seiv1alpha1.PhaseInitializing:
		return r.reconcileInitializing(ctx, node)
	case seiv1alpha1.PhaseRunning:
		return r.reconcileRunning(ctx, node)
	case seiv1alpha1.PhaseFailed:
		r.Recorder.Eventf(node, corev1.EventTypeWarning, "NodeFailed",
			"SeiNode is in Failed state. Delete and recreate the resource to retry.")
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{}, nil
	}
}

// reconcilePending builds the unified Plan and transitions to Initializing.
// For genesis ceremony nodes the plan includes artifact generation and assembly
// steps. For bootstrap nodes the plan includes controller-side Job lifecycle
// tasks followed by production config. All orchestration lives in the plan.
func (r *SeiNodeReconciler) reconcilePending(ctx context.Context, node *seiv1alpha1.SeiNode, p planner.NodePlanner) (ctrl.Result, error) {
	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})

	plan, err := p.BuildPlan(node)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building plan: %w", err)
	}
	node.Status.Plan = plan
	node.Status.Phase = seiv1alpha1.PhaseInitializing

	if err := r.Status().Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("initializing plans: %w", err)
	}

	ns, name := node.Namespace, node.Name
	nodePhaseTransitions.WithLabelValues(ns, string(seiv1alpha1.PhasePending), string(seiv1alpha1.PhaseInitializing)).Inc()
	emitNodePhase(ns, name, seiv1alpha1.PhaseInitializing)
	r.Recorder.Eventf(node, corev1.EventTypeNormal, "PhaseTransition",
		"Phase changed from %s to %s", seiv1alpha1.PhasePending, seiv1alpha1.PhaseInitializing)

	return planner.ResultRequeueImmediate, nil
}

// reconcileInitializing drives the unified Plan to completion. For
// bootstrap nodes the StatefulSet and Service are created only after
// bootstrap teardown is complete (to avoid RWO PVC conflicts). For
// non-bootstrap nodes they are created immediately.
func (r *SeiNodeReconciler) reconcileInitializing(ctx context.Context, node *seiv1alpha1.SeiNode) (ctrl.Result, error) {
	plan := node.Status.Plan

	// TODO: This should be a part of the plan, not a special case we need to filter out for the reconciliation before we
	// go ahead with the actual plan.
	if !planner.NeedsBootstrap(node) || planner.IsBootstrapComplete(plan) {
		if err := r.reconcileNodeStatefulSet(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling statefulset: %w", err)
		}
		if err := r.reconcileNodeService(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling service: %w", err)
		}
	}

	result, err := r.PlanExecutor.ExecutePlan(ctx, node, plan)
	if err != nil {
		return result, err
	}

	// TODO: transitioning should be a part of ExecutePlan, I would think. This way, when a plan is done, it has materialized
	// the proper state and does some final patch to the node, basically leaving it in a state that other logic can observe and
	// act on as well. For instance, if a plan succeeds, the plan knows what state to put it in. Same for if it fails.
	if plan.Phase == seiv1alpha1.TaskPlanComplete {
		return r.transitionPhase(ctx, node, seiv1alpha1.PhaseRunning)
	}
	if plan.Phase == seiv1alpha1.TaskPlanFailed {
		return r.transitionPhase(ctx, node, seiv1alpha1.PhaseFailed)
	}
	return result, nil
}

// reconcileRunning converges owned resources and handles runtime tasks.
func (r *SeiNodeReconciler) reconcileRunning(ctx context.Context, node *seiv1alpha1.SeiNode) (ctrl.Result, error) {

	// TODO: ideally this becomes a task in a plan
	if err := r.reconcileNodeStatefulSet(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling statefulset: %w", err)
	}
	// TODO: ideally this becomes a task in a plan
	if err := r.reconcileNodeService(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling service: %w", err)
	}

	if err := r.observeCurrentImage(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("observing current image: %w", err)
	}

	sc := r.buildSidecarClient(node)
	if sc == nil {
		sidecarUnreachableTotal.WithLabelValues(node.Namespace, node.Name).Inc()
		log.FromContext(ctx).Info("sidecar not reachable, will retry")
		return ctrl.Result{RequeueAfter: statusPollInterval}, nil
	}
	return r.reconcileRuntimeTasks(ctx, node, sc)
}

func (r *SeiNodeReconciler) observeCurrentImage(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	// Wait for the StatefulSet controller to process the latest spec change.
	if sts.Status.ObservedGeneration < sts.Generation {
		return nil
	}
	if sts.Spec.Replicas == nil || sts.Status.UpdatedReplicas < *sts.Spec.Replicas {
		return nil
	}

	if node.Status.CurrentImage != node.Spec.Image {
		patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
		node.Status.CurrentImage = node.Spec.Image
		return r.Status().Patch(ctx, node, patch)
	}
	return nil
}

// transitionPhase transitions the node to a new phase and emits the associated
// metric counter, phase gauge, and Kubernetes event.
func (r *SeiNodeReconciler) transitionPhase(ctx context.Context, node *seiv1alpha1.SeiNode, phase seiv1alpha1.SeiNodePhase) (ctrl.Result, error) {
	prev := node.Status.Phase
	if prev == "" {
		prev = seiv1alpha1.PhasePending
	}

	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	node.Status.Phase = phase
	if err := r.Status().Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting phase to %s: %w", phase, err)
	}

	ns, name := node.Namespace, node.Name
	nodePhaseTransitions.WithLabelValues(ns, string(prev), string(phase)).Inc()
	emitNodePhase(ns, name, phase)

	if phase == seiv1alpha1.PhaseRunning {
		dur := time.Since(node.CreationTimestamp.Time).Seconds()
		nodeInitDuration.WithLabelValues(ns, node.Spec.ChainID).Observe(dur)
		nodeLastInitDuration.WithLabelValues(ns, name).Set(dur)
	}

	r.Recorder.Eventf(node, corev1.EventTypeNormal, "PhaseTransition",
		"Phase changed from %s to %s", prev, phase)

	return planner.ResultRequeueImmediate, nil
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

func (r *SeiNodeReconciler) ensureNodeDataPVC(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	desired := noderesource.GenerateDataPVC(node, r.Platform)
	if err := ctrl.SetControllerReference(node, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	return err
}

func (r *SeiNodeReconciler) reconcileNodeStatefulSet(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	desired := noderesource.GenerateStatefulSet(node, r.Platform)
	desired.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
	if err := ctrl.SetControllerReference(node, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	return r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership)
}

func (r *SeiNodeReconciler) reconcileNodeService(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	desired := noderesource.GenerateHeadlessService(node)
	desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	if err := ctrl.SetControllerReference(node, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	return r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership)
}
