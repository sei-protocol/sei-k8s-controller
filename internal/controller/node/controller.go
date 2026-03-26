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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

const (
	nodeFinalizerName     = "sei.io/seinode-finalizer"
	seiNodeControllerName = "seinode"
	statusPollInterval    = 30 * time.Second
	fieldOwner            = client.FieldOwner("seinode-controller")
)

// PlatformConfig holds infrastructure-level settings that vary per deployment
// environment. Values are read from environment variables in main.go with
// sensible defaults.
type PlatformConfig struct {
	NodepoolName        string
	TolerationKey       string
	TolerationVal       string
	ServiceAccount      string
	StorageClassPerf    string
	StorageClassDefault string
	StorageSizeDefault  string
	StorageSizeArchive  string
	ResourceCPUArchive  string
	ResourceMemArchive  string
	ResourceCPUDefault  string
	ResourceMemDefault  string
	SnapshotRegion      string
}

// DefaultPlatformConfig returns PlatformConfig with production defaults.
func DefaultPlatformConfig() PlatformConfig {
	return PlatformConfig{
		NodepoolName:        "sei-node",
		TolerationKey:       "sei.io/workload",
		TolerationVal:       "sei-node",
		ServiceAccount:      "seid-node",
		StorageClassPerf:    "gp3-10k-750",
		StorageClassDefault: "gp3",
		StorageSizeDefault:  "1000Gi",
		StorageSizeArchive:  "2000Gi",
		ResourceCPUArchive:  "16",
		ResourceMemArchive:  "256Gi",
		ResourceCPUDefault:  "4",
		ResourceMemDefault:  "32Gi",
		SnapshotRegion:      "eu-central-1",
	}
}

// SeiNodeReconciler reconciles a SeiNode object.
type SeiNodeReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	Recorder             record.EventRecorder
	Platform             PlatformConfig
	PlanExecutor         *planner.Executor
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

	p, err := planner.ForNode(node, r.Platform.SnapshotRegion)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving planner: %w", err)
	}
	if err := p.Validate(node); err != nil {
		return ctrl.Result{}, fmt.Errorf("validating spec: %w", err)
	}

	if !hasGenesisPVC(node) {
		if err := r.ensureNodeDataPVC(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring data PVC: %w", err)
		}
	}

	switch node.Status.Phase {
	case "", seiv1alpha1.PhasePending:
		return r.reconcilePending(ctx, node, p)
	case seiv1alpha1.PhasePreInitializing:
		return r.reconcilePreInitializing(ctx, node, p)
	case seiv1alpha1.PhaseInitializing:
		return r.reconcileInitializing(ctx, node)
	case seiv1alpha1.PhaseRunning:
		return r.reconcileRunning(ctx, node)
	case seiv1alpha1.PhaseFailed:
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{}, nil
	}
}

// reconcilePending creates all plans up front and selects the starting phase.
// The Init plan is always built here — for genesis ceremony nodes the S3
// source is deterministic and discover-peers is unconditionally included.
// Plan execution order is the single source of truth for orchestration.
func (r *SeiNodeReconciler) reconcilePending(ctx context.Context, node *seiv1alpha1.SeiNode, p planner.NodePlanner) (ctrl.Result, error) {
	patch := client.MergeFrom(node.DeepCopy())

	node.Status.PreInitPlan = planner.BuildPreInitPlan(node, p)
	if planner.IsGenesisCeremonyNode(node) {
		node.Status.InitPlan = planner.BuildGenesisInitPlan(node)
	} else if planner.NeedsPreInit(node) {
		node.Status.InitPlan = planner.BuildPostBootstrapInitPlan(node, nil)
	} else {
		node.Status.InitPlan = p.BuildPlan(node)
	}
	node.Status.Phase = seiv1alpha1.PhasePreInitializing

	if err := r.Status().Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("initializing plans: %w", err)
	}

	ns, name := node.Namespace, node.Name
	nodePhaseTransitions.WithLabelValues(ns, string(seiv1alpha1.PhasePending), string(seiv1alpha1.PhasePreInitializing)).Inc()
	emitNodePhase(ns, name, seiv1alpha1.PhasePreInitializing)
	r.Recorder.Eventf(node, corev1.EventTypeNormal, "PhaseTransition",
		"Phase changed from %s to %s", seiv1alpha1.PhasePending, seiv1alpha1.PhasePreInitializing)

	return ctrl.Result{RequeueAfter: planner.ImmediateRequeue}, nil
}

// reconcileInitializing ensures the StatefulSet and Service exist, then drives
// the InitPlan to completion.
func (r *SeiNodeReconciler) reconcileInitializing(ctx context.Context, node *seiv1alpha1.SeiNode) (ctrl.Result, error) {
	if err := r.reconcileNodeStatefulSet(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling statefulset: %w", err)
	}
	if err := r.reconcileNodeService(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling service: %w", err)
	}

	result, err := r.PlanExecutor.ExecutePlan(ctx, node, node.Status.InitPlan)
	if err != nil {
		return result, err
	}

	if node.Status.InitPlan.Phase == seiv1alpha1.TaskPlanComplete {
		return r.transitionPhase(ctx, node, seiv1alpha1.PhaseRunning)
	}
	if node.Status.InitPlan.Phase == seiv1alpha1.TaskPlanFailed {
		return r.transitionPhase(ctx, node, seiv1alpha1.PhaseFailed)
	}
	return result, nil
}

// reconcileRunning handles runtime tasks (scheduled uploads, exports).
func (r *SeiNodeReconciler) reconcileRunning(ctx context.Context, node *seiv1alpha1.SeiNode) (ctrl.Result, error) {
	sc := r.buildSidecarClient(node)
	if sc == nil {
		sidecarUnreachableTotal.WithLabelValues(node.Namespace, node.Name).Inc()
		log.FromContext(ctx).Info("sidecar not reachable, will retry")
		return ctrl.Result{RequeueAfter: statusPollInterval}, nil
	}
	return r.reconcileRuntimeTasks(ctx, node, sc)
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

	return ctrl.Result{RequeueAfter: planner.ImmediateRequeue}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SeiNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&seiv1alpha1.SeiNode{}).
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

	patch := client.MergeFrom(node.DeepCopy())
	node.Status.Phase = seiv1alpha1.PhaseTerminating
	if err := r.Status().Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting terminating status: %w", err)
	}

	// Non-genesis SeiNodes own their data PVC; genesis PVCs are owned by SeiNodePool.
	if !hasGenesisPVC(node) && !node.Spec.Storage.RetainOnDelete {
		if err := r.deleteNodeDataPVC(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting data PVC: %w", err)
		}
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

func hasGenesisPVC(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.Genesis.PVC != nil
}
