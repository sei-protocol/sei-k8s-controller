package node

import (
	"context"
	"fmt"
	"time"

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
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	nodeFinalizerName     = "sei.io/seinode-finalizer"
	seiNodeControllerName = "seinode"
	statusPollInterval    = 30 * time.Second
	fieldOwner            = client.FieldOwner("seinode-controller")

	ReasonInitPlanComplete      = "InitPlanComplete"
	ReasonNodeUpdateComplete    = "NodeUpdateComplete"
	ReasonSidecarNotReady       = "SidecarNotReady"
	ReasonNodeUpdatePlanStarted = "NodeUpdatePlanStarted"
	ReasonNodeUpdatePlanComplete    = "NodeUpdatePlanDone"
	ReasonNodeUpdatePlanFailed  = "NodeUpdatePlanFailed"
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

	if err := r.reconcilePeers(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling peers: %w", err)
	}

	if err := r.ensureNodeDataPVC(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring data PVC: %w", err)
	}

	switch node.Status.Phase {
	case "", seiv1alpha1.PhasePending:
		return r.reconcilePending(ctx, node, p)
	case seiv1alpha1.PhaseInitializing:
		return r.reconcileInitializing(ctx, node)
	case seiv1alpha1.PhaseRunning:
		return r.reconcileRunning(ctx, node, p)
	case seiv1alpha1.PhaseFailed:
		r.Recorder.Eventf(node, corev1.EventTypeWarning, "NodeFailed",
			"SeiNode is in Failed state. Delete and recreate the resource to retry.")
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{}, nil
	}
}

// ---------------------------------------------------------------------------
// Phase-specific reconciliation
// ---------------------------------------------------------------------------

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

func (r *SeiNodeReconciler) reconcileInitializing(ctx context.Context, node *seiv1alpha1.SeiNode) (ctrl.Result, error) {
	if !planner.NeedsBootstrap(node) || planner.IsBootstrapComplete(node.Status.Plan) {
		if err := r.reconcileNodeStatefulSet(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling statefulset: %w", err)
		}
		if err := r.reconcileNodeService(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling service: %w", err)
		}
	}

	return r.drivePlan(ctx, node)
}

func (r *SeiNodeReconciler) reconcileRunning(ctx context.Context, node *seiv1alpha1.SeiNode, p planner.NodePlanner) (ctrl.Result, error) {
	if err := r.reconcileNodeStatefulSet(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling statefulset: %w", err)
	}
	if err := r.reconcileNodeService(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling service: %w", err)
	}

	if err := r.observeStatefulSetState(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("observing statefulset state: %w", err)
	}
	if err := r.observeCurrentImage(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("observing current image: %w", err)
	}

	if node.Status.Plan != nil && node.Status.Plan.Phase == seiv1alpha1.TaskPlanActive {
		return r.drivePlan(ctx, node)
	}

	plan, err := p.BuildPlan(node)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building plan: %w", err)
	}
	if plan != nil {
		return r.startNodeUpdatePlan(ctx, node, plan)
	}

	return r.steadyState(ctx, node)
}

// ---------------------------------------------------------------------------
// Shared plan lifecycle
// ---------------------------------------------------------------------------

func (r *SeiNodeReconciler) drivePlan(ctx context.Context, node *seiv1alpha1.SeiNode) (ctrl.Result, error) {
	plan := node.Status.Plan

	switch plan.Phase {
	case seiv1alpha1.TaskPlanComplete:
		return r.handlePlanComplete(ctx, node)
	case seiv1alpha1.TaskPlanFailed:
		return r.handlePlanFailed(ctx, node)
	}

	result, err := r.PlanExecutor.ExecutePlan(ctx, node, plan)
	if err != nil {
		return result, err
	}

	switch plan.Phase {
	case seiv1alpha1.TaskPlanComplete:
		return r.handlePlanComplete(ctx, node)
	case seiv1alpha1.TaskPlanFailed:
		return r.handlePlanFailed(ctx, node)
	}

	return result, nil
}

func (r *SeiNodeReconciler) handlePlanComplete(ctx context.Context, node *seiv1alpha1.SeiNode) (ctrl.Result, error) {
	switch node.Status.Phase {
	case seiv1alpha1.PhaseInitializing:
		return r.transitionPhase(ctx, node, seiv1alpha1.PhaseRunning)

	case seiv1alpha1.PhaseRunning:
		planID := node.Status.Plan.ID
		patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
		node.Status.Plan = nil
		setSeiNodeCondition(node, seiv1alpha1.ConditionSidecarReady, metav1.ConditionTrue,
			ReasonNodeUpdateComplete,
			fmt.Sprintf("NodeUpdate plan %s completed", planID))
		setSeiNodeCondition(node, seiv1alpha1.ConditionNodeUpdateInProgress, metav1.ConditionFalse,
			ReasonNodeUpdatePlanComplete,
			fmt.Sprintf("NodeUpdate plan %s completed", planID))
		if err := r.Status().Patch(ctx, node, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("completing NodeUpdate plan: %w", err)
		}
		r.Recorder.Eventf(node, corev1.EventTypeNormal, "NodeUpdateComplete",
			"NodeUpdate plan %s completed, sidecar ready", planID)
		return planner.ResultRequeueImmediate, nil

	default:
		return ctrl.Result{}, fmt.Errorf("unexpected plan completion in phase %s", node.Status.Phase)
	}
}

func (r *SeiNodeReconciler) handlePlanFailed(ctx context.Context, node *seiv1alpha1.SeiNode) (ctrl.Result, error) {
	plan := node.Status.Plan

	switch node.Status.Phase {
	case seiv1alpha1.PhaseInitializing:
		return r.transitionPhase(ctx, node, seiv1alpha1.PhaseFailed)

	case seiv1alpha1.PhaseRunning:
		detail := "unknown"
		if plan.FailedTaskDetail != nil {
			detail = fmt.Sprintf("task %s failed: %s (retries: %d/%d)",
				plan.FailedTaskDetail.Type,
				plan.FailedTaskDetail.Error,
				plan.FailedTaskDetail.RetryCount,
				plan.FailedTaskDetail.MaxRetries)
		}

		r.Recorder.Eventf(node, corev1.EventTypeWarning, "NodeUpdatePlanFailed",
			"NodeUpdate plan %s failed: %s. Will retry on next reconcile.", plan.ID, detail)
		log.FromContext(ctx).Info("NodeUpdate plan failed, clearing for retry",
			"planID", plan.ID, "detail", detail)
		nodeUpdatePlanFailuresTotal.WithLabelValues(node.Namespace, node.Name).Inc()

		patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
		node.Status.Plan = nil
		setSeiNodeCondition(node, seiv1alpha1.ConditionNodeUpdateInProgress, metav1.ConditionFalse,
			ReasonNodeUpdatePlanFailed,
			fmt.Sprintf("NodeUpdate plan %s failed: %s", plan.ID, detail))
		if err := r.Status().Patch(ctx, node, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("clearing failed NodeUpdate plan: %w", err)
		}
		return ctrl.Result{RequeueAfter: statusPollInterval}, nil

	default:
		return ctrl.Result{}, fmt.Errorf("unexpected plan failure in phase %s", node.Status.Phase)
	}
}

func (r *SeiNodeReconciler) startNodeUpdatePlan(ctx context.Context, node *seiv1alpha1.SeiNode, plan *seiv1alpha1.TaskPlan) (ctrl.Result, error) {
	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	node.Status.Plan = plan
	setSeiNodeCondition(node, seiv1alpha1.ConditionNodeUpdateInProgress, metav1.ConditionTrue,
		ReasonNodeUpdatePlanStarted,
		fmt.Sprintf("NodeUpdate plan %s started with %d tasks", plan.ID, len(plan.Tasks)))
	if err := r.Status().Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting NodeUpdate plan: %w", err)
	}
	r.Recorder.Eventf(node, corev1.EventTypeNormal, "NodeUpdatePlanStarted",
		"NodeUpdate plan %s started with %d tasks", plan.ID, len(plan.Tasks))
	return planner.ResultRequeueImmediate, nil
}

// ---------------------------------------------------------------------------
// Observation
// ---------------------------------------------------------------------------

func (r *SeiNodeReconciler) observeStatefulSetState(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if !isStatefulSetRolledOut(sts) {
		return nil
	}

	if !isSidecarReadyConditionCurrent(node) {
		patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
		setSeiNodeCondition(node, seiv1alpha1.ConditionSidecarReady, metav1.ConditionFalse,
			ReasonSidecarNotReady, "StatefulSet rollout complete, sidecar needs re-initialization")
		if err := r.Status().Patch(ctx, node, patch); err != nil {
			return fmt.Errorf("setting SidecarReady=False: %w", err)
		}
		r.Recorder.Eventf(node, corev1.EventTypeWarning, "SidecarNotReady",
			"StatefulSet rolled out new pod template, NodeUpdate plan will be built")
	}

	return nil
}

func (r *SeiNodeReconciler) observeCurrentImage(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: node.Name, Namespace: node.Namespace}, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

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

// ---------------------------------------------------------------------------
// Steady state
// ---------------------------------------------------------------------------

func (r *SeiNodeReconciler) steadyState(_ context.Context, _ *seiv1alpha1.SeiNode) (ctrl.Result, error) {
	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}

// ---------------------------------------------------------------------------
// Phase transitions
// ---------------------------------------------------------------------------

func (r *SeiNodeReconciler) transitionPhase(ctx context.Context, node *seiv1alpha1.SeiNode, phase seiv1alpha1.SeiNodePhase) (ctrl.Result, error) {
	prev := node.Status.Phase
	if prev == "" {
		prev = seiv1alpha1.PhasePending
	}

	patch := client.MergeFromWithOptions(node.DeepCopy(), client.MergeFromWithOptimisticLock{})
	node.Status.Phase = phase
	if phase == seiv1alpha1.PhaseRunning {
		setSeiNodeCondition(node, seiv1alpha1.ConditionSidecarReady, metav1.ConditionTrue,
			ReasonInitPlanComplete, "Initial plan completed, sidecar ready")
	}
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

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func setSeiNodeCondition(node *seiv1alpha1.SeiNode, condType string, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}

func isStatefulSetRolledOut(sts *appsv1.StatefulSet) bool {
	if sts.Status.ObservedGeneration < sts.Generation {
		return false
	}
	if sts.Spec.Replicas == nil {
		return false
	}
	return sts.Status.UpdatedReplicas >= *sts.Spec.Replicas
}

func isSidecarReadyConditionCurrent(node *seiv1alpha1.SeiNode) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == seiv1alpha1.ConditionSidecarReady {
			return c.Status == metav1.ConditionTrue && c.ObservedGeneration >= node.Generation
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Resource management
// ---------------------------------------------------------------------------

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
	err := r.Get(ctx, types.NamespacedName{Name: nodeDataPVCName(node), Namespace: node.Namespace}, pvc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.Delete(ctx, pvc)
}

func (r *SeiNodeReconciler) ensureNodeDataPVC(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	desired := generateNodeDataPVC(node, r.Platform)
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
	desired := generateNodeStatefulSet(node, r.Platform)
	desired.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
	if err := ctrl.SetControllerReference(node, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	return r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership)
}

func (r *SeiNodeReconciler) reconcileNodeService(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	desired := generateNodeHeadlessService(node)
	desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	if err := ctrl.SetControllerReference(node, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	return r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership)
}
