package nodegroup

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	groupFinalizerName = "sei.io/seinodegroup-finalizer"
	controllerName     = "seinodegroup"
	statusPollInterval = 30 * time.Second
	fieldOwner         = client.FieldOwner("seinodegroup-controller")
)

// SeiNodeGroupReconciler reconciles a SeiNodeGroup object.
type SeiNodeGroupReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// ControllerSA is the SPIFFE principal of the controller's ServiceAccount.
	// It is auto-injected into every AuthorizationPolicy to ensure the
	// controller can always reach the seictl sidecar.
	ControllerSA string
}

// +kubebuilder:rbac:groups=sei.io,resources=seinodegroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodegroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sei.io,resources=seinodegroups/finalizers,verbs=update
// +kubebuilder:rbac:groups=sei.io,resources=seinodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodes/status,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=security.istio.io,resources=authorizationpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete

func (r *SeiNodeGroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	group := &seiv1alpha1.SeiNodeGroup{}
	if err := r.Get(ctx, req.NamespacedName, group); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !group.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, group)
	}

	if err := r.ensureFinalizer(ctx, group); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileSeiNodes(ctx, group); err != nil {
		logger.Error(err, "reconciling SeiNodes")
		return ctrl.Result{}, fmt.Errorf("reconciling SeiNodes: %w", err)
	}

	if err := r.reconcileNetworking(ctx, group); err != nil {
		logger.Error(err, "reconciling networking")
		return ctrl.Result{}, fmt.Errorf("reconciling networking: %w", err)
	}

	if err := r.reconcileMonitoring(ctx, group); err != nil {
		logger.Error(err, "reconciling monitoring")
		return ctrl.Result{}, fmt.Errorf("reconciling monitoring: %w", err)
	}

	if err := r.updateStatus(ctx, group); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}

func (r *SeiNodeGroupReconciler) ensureFinalizer(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	if controllerutil.ContainsFinalizer(group, groupFinalizerName) {
		return nil
	}
	controllerutil.AddFinalizer(group, groupFinalizerName)
	return r.Update(ctx, group)
}

func (r *SeiNodeGroupReconciler) handleDeletion(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(group, groupFinalizerName) {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(group.DeepCopy())
	group.Status.Phase = seiv1alpha1.GroupPhaseTerminating
	if err := r.Status().Patch(ctx, group, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting terminating status: %w", err)
	}

	policy := group.Spec.DeletionPolicy
	if policy == "" {
		policy = seiv1alpha1.DeletionPolicyDelete
	}

	if policy == seiv1alpha1.DeletionPolicyRetain {
		if err := r.orphanChildSeiNodes(ctx, group); err != nil {
			return ctrl.Result{}, fmt.Errorf("orphaning child SeiNodes: %w", err)
		}
		if err := r.orphanNetworkingResources(ctx, group); err != nil {
			return ctrl.Result{}, fmt.Errorf("orphaning networking resources: %w", err)
		}
	} else {
		if err := r.deleteNetworkingResources(ctx, group); err != nil {
			return ctrl.Result{}, fmt.Errorf("cleaning up networking: %w", err)
		}
	}

	controllerutil.RemoveFinalizer(group, groupFinalizerName)
	return ctrl.Result{}, r.Update(ctx, group)
}

// SetupWithManager sets up the controller with the Manager.
func (r *SeiNodeGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&seiv1alpha1.SeiNodeGroup{}).
		Owns(&seiv1alpha1.SeiNode{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.Ingress{}).
		Named(controllerName).
		Complete(r)
}
