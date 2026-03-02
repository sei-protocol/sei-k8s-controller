package node

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	seiv1alpha1 "github.com/sei-protocol/sei-node-controller/api/v1alpha1"
)

const (
	nodeFinalizerName     = "sei.io/seinode-finalizer"
	seiNodeControllerName = "seinode"
	statusPollInterval    = 30 * time.Second
	fieldOwner            = client.FieldOwner("seinode-controller")
)

// SeiNodeReconciler reconciles a SeiNode object.
type SeiNodeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// BuildSidecarClientFn overrides sidecar client construction for testing.
	BuildSidecarClientFn func(node *seiv1alpha1.SeiNode) SidecarStatusClient
	// MaxBootstrapRetries overrides the default retry limit (3) for testing.
	MaxBootstrapRetries int
	// retryState tracks per-node, per-task retry counts in memory.
	// Controller-runtime guarantees serial reconciliation per object,
	// so no mutex is needed.
	retryState map[types.NamespacedName]map[string]*retryInfo
}

// +kubebuilder:rbac:groups=sei.io,resources=seinodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sei.io,resources=seinodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

func (r *SeiNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	node := &seiv1alpha1.SeiNode{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !node.DeletionTimestamp.IsZero() {
		return r.handleNodeDeletion(ctx, node)
	}

	if err := r.ensureNodeFinalizer(ctx, node); err != nil {
		return ctrl.Result{}, err
	}

	// Sidecar mode: the sidecar handles bootstrap and runtime tasks.
	// Still need to ensure infrastructure (PVC, StatefulSet, Service).
	if node.Spec.Sidecar != nil {
		if !hasGenesisPVC(node) {
			if err := r.ensureNodeDataPVC(ctx, node); err != nil {
				return ctrl.Result{}, fmt.Errorf("ensuring data PVC: %w", err)
			}
		}
		if err := r.reconcileNodeStatefulSet(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling statefulset: %w", err)
		}
		if err := r.reconcileNodeService(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling service: %w", err)
		}
		return r.reconcileSidecarProgression(ctx, node)
	}

	// Non-sidecar snapshot mode: ensure the data PVC exists.
	if hasSnapshot(node) {
		if err := r.ensureNodeDataPVC(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring data PVC: %w", err)
		}
	}

	if err := r.reconcileNodeStatefulSet(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling statefulset: %w", err)
	}

	if err := r.reconcileNodeService(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling service: %w", err)
	}

	if err := r.updateNodeStatus(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SeiNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&seiv1alpha1.SeiNode{}).
		Owns(&appsv1.StatefulSet{}).
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
	node.Status.Phase = "Terminating"
	if err := r.Status().Patch(ctx, node, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting terminating status: %w", err)
	}

	// Non-genesis SeiNodes own their data PVC; genesis PVCs are owned by SeiNodePool.
	if !hasGenesisPVC(node) && !node.Spec.Storage.RetainOnDelete {
		if err := r.deleteNodeDataPVC(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting data PVC: %w", err)
		}
	}

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
	desired := generateNodeDataPVC(node)
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
	desired := generateNodeStatefulSet(node)
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

func (r *SeiNodeReconciler) updateNodeStatus(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	patch := client.MergeFrom(node.DeepCopy())

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(node.Namespace),
		client.MatchingLabels(resourceLabelsForNode(node)),
	); err != nil {
		return fmt.Errorf("listing pods: %w", err)
	}

	node.Status.Phase = nodePhase(podList)
	applyNodeStatusConditions(node)

	return r.Status().Patch(ctx, node, patch)
}

func nodePhase(podList *corev1.PodList) string {
	for i := range podList.Items {
		pod := &podList.Items[i]
		if isPodReady(pod) {
			return "Running"
		}
		if pod.Status.Phase == corev1.PodFailed {
			return "Failed"
		}
	}
	return "Pending"
}

func applyNodeStatusConditions(node *seiv1alpha1.SeiNode) {
	switch node.Status.Phase {
	case "Running":
		setNodeCondition(node, ConditionTypeReady, metav1.ConditionTrue, ReasonAllPodsReady, "Node is ready and healthy")
		setNodeCondition(node, ConditionTypeProgressing, metav1.ConditionFalse, ReasonReconcileSucceeded, "Reconciliation complete")
	case "Failed":
		setNodeCondition(node, ConditionTypeReady, metav1.ConditionFalse, ReasonPodsNotReady, "Node has failed")
		setNodeCondition(node, ConditionTypeDegraded, metav1.ConditionTrue, ReasonPodsNotReady, "Node has failed")
	default:
		setNodeCondition(node, ConditionTypeReady, metav1.ConditionFalse, ReasonPodsNotReady, "Node is not yet ready")
		setNodeCondition(node, ConditionTypeProgressing, metav1.ConditionTrue, ReasonPodsNotReady, "Waiting for node to become ready")
	}
}

func hasSnapshot(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.Snapshot != nil
}

func hasGenesisPVC(node *seiv1alpha1.SeiNode) bool {
	return node.Spec.Genesis.PVC != nil
}
