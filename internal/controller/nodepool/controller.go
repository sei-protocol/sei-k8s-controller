package nodepool

import (
	"context"
	"fmt"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	finalizerName           = "sei.io/seinodepool-finalizer"
	gracefulShutdownTimeout = 60 * time.Second
	statusPollInterval      = 30 * time.Second
	genesisJobPollInterval  = 10 * time.Second
	controllerName          = "seinodepool"

	phasePending     = string(seiv1alpha1.PhasePending)
	phaseRunning     = string(seiv1alpha1.PhaseRunning)
	phaseFailed      = string(seiv1alpha1.PhaseFailed)
	phaseTerminating = string(seiv1alpha1.PhaseTerminating)
)

// SeiNodePoolReconciler reconciles a SeiNodePool object.
type SeiNodePoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=sei.io,resources=seinodepools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sei.io,resources=seinodepools/finalizers,verbs=update
// +kubebuilder:rbac:groups=sei.io,resources=seinodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodes/status,verbs=get
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

// Reconcile moves the current state of the cluster closer to the desired state
// specified by the SeiNodePool object.
func (r *SeiNodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	sn := &seiv1alpha1.SeiNodePool{}
	if err := r.Get(ctx, req.NamespacedName, sn); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !sn.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, sn)
	}

	if err := r.ensureFinalizer(ctx, sn); err != nil {
		return ctrl.Result{}, err
	}

	ready, err := r.reconcileDataPreparation(ctx, sn)
	if err != nil {
		log.Error(err, "data preparation failed")
		return ctrl.Result{}, fmt.Errorf("reconciling data preparation: %w", err)
	}
	if !ready {
		return ctrl.Result{RequeueAfter: genesisJobPollInterval}, nil
	}

	if err := r.reconcileNetworkPolicy(ctx, sn); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling NetworkPolicy: %w", err)
	}

	if err := r.reconcileSeiNodes(ctx, sn); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling SeiNodes: %w", err)
	}

	if err := r.updateStatus(ctx, sn); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SeiNodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&seiv1alpha1.SeiNodePool{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&seiv1alpha1.SeiNode{}).
		Named(controllerName).
		Complete(r)
}

func (r *SeiNodePoolReconciler) ensureFinalizer(ctx context.Context, sn *seiv1alpha1.SeiNodePool) error {
	if controllerutil.ContainsFinalizer(sn, finalizerName) {
		return nil
	}
	controllerutil.AddFinalizer(sn, finalizerName)
	return r.Update(ctx, sn)
}

func (r *SeiNodePoolReconciler) handleDeletion(ctx context.Context, sn *seiv1alpha1.SeiNodePool) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sn, finalizerName) {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(sn.DeepCopy())
	sn.Status.Phase = phaseTerminating
	setCondition(sn, ConditionTypeReady, metav1.ConditionFalse, "Terminating", "SeiNodePool is being deleted")
	if err := r.patchStatus(ctx, sn, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting terminating status: %w", err)
	}

	nodesGone, err := r.allSeiNodesGone(ctx, sn)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking SeiNode termination: %w", err)
	}
	if !nodesGone {
		deletionAge := time.Since(sn.DeletionTimestamp.Time)
		if deletionAge < gracefulShutdownTimeout {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
	}

	if !sn.Spec.Storage.RetainOnDelete {
		if err := r.deleteOwnedPVCs(ctx, sn); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting PVCs: %w", err)
		}
	}

	controllerutil.RemoveFinalizer(sn, finalizerName)
	return ctrl.Result{}, r.Update(ctx, sn)
}

func (r *SeiNodePoolReconciler) allSeiNodesGone(ctx context.Context, sn *seiv1alpha1.SeiNodePool) (bool, error) {
	nodeList := &seiv1alpha1.SeiNodeList{}
	if err := r.List(ctx, nodeList,
		client.InNamespace(sn.Namespace),
		client.MatchingLabels(resourceLabels(sn)),
	); err != nil {
		return false, err
	}
	return len(nodeList.Items) == 0, nil
}

func (r *SeiNodePoolReconciler) deleteOwnedPVCs(ctx context.Context, sn *seiv1alpha1.SeiNodePool) error {
	pvcList := &corev1.PersistentVolumeClaimList{}
	if err := r.List(ctx, pvcList,
		client.InNamespace(sn.Namespace),
		client.MatchingLabels(resourceLabels(sn)),
	); err != nil {
		return err
	}

	for i := range pvcList.Items {
		if err := r.Delete(ctx, &pvcList.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting PVC %s: %w", pvcList.Items[i].Name, err)
		}
	}
	return nil
}

func (r *SeiNodePoolReconciler) reconcileDataPreparation(ctx context.Context, sn *seiv1alpha1.SeiNodePool) (bool, error) {
	nodeCount := int(sn.Spec.NodeConfiguration.NodeCount)

	for i := range nodeCount {
		if err := r.ensureDataPVC(ctx, sn, i); err != nil {
			return false, fmt.Errorf("ensuring data PVC %d: %w", i, err)
		}
	}

	if err := r.ensureGenesisPVC(ctx, sn); err != nil {
		return false, fmt.Errorf("ensuring genesis PVC: %w", err)
	}
	if err := r.ensureGenesisScriptConfigMap(ctx, sn); err != nil {
		return false, fmt.Errorf("ensuring genesis script ConfigMap: %w", err)
	}
	if err := r.ensureGenesisJobCreated(ctx, sn); err != nil {
		return false, fmt.Errorf("ensuring genesis Job: %w", err)
	}
	genesisReady, err := r.checkGenesisJobStatus(ctx, sn)
	if err != nil || !genesisReady {
		return false, err
	}

	allReady := true
	for i := range nodeCount {
		ready, err := r.ensurePrepJob(ctx, sn, i)
		if err != nil {
			return false, err
		}
		if !ready {
			allReady = false
		}
	}
	return allReady, nil
}

func (r *SeiNodePoolReconciler) reconcileNetworkPolicy(ctx context.Context, sn *seiv1alpha1.SeiNodePool) error {
	desired := generateNetworkPolicy(sn)
	if err := ctrl.SetControllerReference(sn, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	existing := &networkingv1.NetworkPolicy{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func (r *SeiNodePoolReconciler) reconcileSeiNodes(ctx context.Context, sn *seiv1alpha1.SeiNodePool) error {
	nodeCount := int(sn.Spec.NodeConfiguration.NodeCount)
	for i := range nodeCount {
		if err := r.ensureSeiNode(ctx, sn, i); err != nil {
			return fmt.Errorf("ensuring SeiNode %d: %w", i, err)
		}
	}
	return r.scaleDown(ctx, sn)
}

func (r *SeiNodePoolReconciler) ensureSeiNode(ctx context.Context, sn *seiv1alpha1.SeiNodePool, ordinal int) error {
	desired := generateSeiNode(sn, ordinal)
	if err := ctrl.SetControllerReference(sn, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	existing := &seiv1alpha1.SeiNode{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	updated := false
	if existing.Spec.Image != desired.Spec.Image {
		existing.Spec.Image = desired.Spec.Image
		updated = true
	}
	if desired.Spec.Entrypoint != nil && (existing.Spec.Entrypoint == nil ||
		!slices.Equal(existing.Spec.Entrypoint.Command, desired.Spec.Entrypoint.Command) ||
		!slices.Equal(existing.Spec.Entrypoint.Args, desired.Spec.Entrypoint.Args)) {
		existing.Spec.Entrypoint = desired.Spec.Entrypoint
		updated = true
	}
	if updated {
		return r.Update(ctx, existing)
	}
	return nil
}

func generateSeiNode(sn *seiv1alpha1.SeiNodePool, ordinal int) *seiv1alpha1.SeiNode {
	entrypoint := sn.Spec.NodeConfiguration.Entrypoint
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      seiNodeName(sn, ordinal),
			Namespace: sn.Namespace,
			Labels:    resourceLabels(sn),
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:    sn.Spec.ChainID,
			Image:      sn.Spec.NodeConfiguration.Image,
			Entrypoint: &entrypoint,
			Genesis: seiv1alpha1.GenesisConfiguration{
				PVC: &seiv1alpha1.GenesisPVCSource{
					DataPVC: dataPVCName(sn, ordinal),
				},
			},
			Storage: seiv1alpha1.SeiNodeStorageConfig{
				RetainOnDelete: sn.Spec.Storage.RetainOnDelete,
			},
		},
	}
}

func (r *SeiNodePoolReconciler) ensureDataPVC(ctx context.Context, sn *seiv1alpha1.SeiNodePool, ordinal int) error {
	desired := generateDataPVC(sn, ordinal)
	if err := ctrl.SetControllerReference(sn, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	return err
}

func (r *SeiNodePoolReconciler) ensurePrepJob(ctx context.Context, sn *seiv1alpha1.SeiNodePool, ordinal int) (bool, error) {
	desired := generatePrepJob(sn, ordinal)
	if err := ctrl.SetControllerReference(sn, desired, r.Scheme); err != nil {
		return false, fmt.Errorf("setting owner reference: %w", err)
	}

	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return false, r.Create(ctx, desired)
	}
	if err != nil {
		return false, err
	}

	if isJobFailed(existing) {
		msg := fmt.Sprintf("prep Job %s failed", existing.Name)
		for _, c := range existing.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				msg = fmt.Sprintf("prep Job %s failed: %s", existing.Name, c.Message)
				break
			}
		}
		return false, r.setFailedStatus(ctx, sn, ReasonPrepJobFailed, msg)
	}

	return isJobComplete(existing), nil
}

func (r *SeiNodePoolReconciler) ensureGenesisPVC(ctx context.Context, sn *seiv1alpha1.SeiNodePool) error {
	desired := generateGenesisPVC(sn)
	if err := ctrl.SetControllerReference(sn, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	return err
}

func (r *SeiNodePoolReconciler) ensureGenesisScriptConfigMap(ctx context.Context, sn *seiv1alpha1.SeiNodePool) error {
	desired := generateGenesisScriptConfigMap(sn)
	if err := ctrl.SetControllerReference(sn, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Data = desired.Data
	existing.Labels = desired.Labels
	return r.Update(ctx, existing)
}

func (r *SeiNodePoolReconciler) ensureGenesisJobCreated(ctx context.Context, sn *seiv1alpha1.SeiNodePool) error {
	desired := generateGenesisJob(sn)
	if err := ctrl.SetControllerReference(sn, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	return err
}

func (r *SeiNodePoolReconciler) checkGenesisJobStatus(ctx context.Context, sn *seiv1alpha1.SeiNodePool) (bool, error) {
	job := &batchv1.Job{}
	jobName := fmt.Sprintf("%s-genesis", sn.Name)
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: sn.Namespace}, job); err != nil {
		return false, fmt.Errorf("fetching genesis Job: %w", err)
	}

	if isJobFailed(job) {
		msg := "Genesis Job failed"
		for _, c := range job.Status.Conditions {
			if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
				msg = fmt.Sprintf("Genesis Job failed: %s", c.Message)
				break
			}
		}
		return false, r.setFailedStatus(ctx, sn, ReasonGenesisJobFailed, msg)
	}

	return isJobComplete(job), nil
}

func isJobComplete(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isJobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *SeiNodePoolReconciler) setFailedStatus(ctx context.Context, sn *seiv1alpha1.SeiNodePool, reason, message string) error {
	patch := client.MergeFrom(sn.DeepCopy())
	sn.Status.Phase = phaseFailed
	setCondition(sn, ConditionTypeReady, metav1.ConditionFalse, reason, message)
	if err := r.patchStatus(ctx, sn, patch); err != nil {
		return fmt.Errorf("setting failed status: %w", err)
	}
	return fmt.Errorf("%s: %s", reason, message)
}

func (r *SeiNodePoolReconciler) updateStatus(ctx context.Context, sn *seiv1alpha1.SeiNodePool) error {
	patch := client.MergeFrom(sn.DeepCopy())

	nodeList := &seiv1alpha1.SeiNodeList{}
	if err := r.List(ctx, nodeList,
		client.InNamespace(sn.Namespace),
		client.MatchingLabels(resourceLabels(sn)),
	); err != nil {
		return fmt.Errorf("listing SeiNodes: %w", err)
	}

	totalNodes := sn.Spec.NodeConfiguration.NodeCount
	var readyNodes int32
	nodeStatuses := make([]seiv1alpha1.NodeStatus, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		ready := node.Status.Phase == seiv1alpha1.PhaseRunning
		if ready {
			readyNodes++
		}
		nodeStatuses = append(nodeStatuses, seiv1alpha1.NodeStatus{
			Name:  node.Name,
			Ready: ready,
		})
	}

	sn.Status.TotalNodes = totalNodes
	sn.Status.ReadyNodes = readyNodes
	sn.Status.NodeStatuses = nodeStatuses
	sn.Status.Phase = nodepoolPhase(readyNodes, totalNodes, nodeList)
	applyStatusConditions(sn, readyNodes, totalNodes)

	return r.patchStatus(ctx, sn, patch)
}

func nodepoolPhase(readyNodes, totalNodes int32, nodeList *seiv1alpha1.SeiNodeList) string {
	if totalNodes == 0 {
		return phasePending
	}
	if readyNodes == totalNodes {
		return phaseRunning
	}
	for i := range nodeList.Items {
		if nodeList.Items[i].Status.Phase == seiv1alpha1.PhaseFailed {
			return phaseFailed
		}
	}
	return phasePending
}

func applyStatusConditions(sn *seiv1alpha1.SeiNodePool, readyNodes, totalNodes int32) {
	switch {
	case readyNodes == totalNodes && readyNodes > 0:
		setCondition(sn, ConditionTypeReady, metav1.ConditionTrue, ReasonAllPodsReady, "All nodes are ready and healthy")
		setCondition(sn, ConditionTypeProgressing, metav1.ConditionFalse, ReasonReconcileSucceeded, "Reconciliation complete")
	case readyNodes > 0:
		setCondition(sn, ConditionTypeReady, metav1.ConditionFalse, ReasonPodsNotReady,
			fmt.Sprintf("%d/%d nodes ready", readyNodes, totalNodes))
		setCondition(sn, ConditionTypeDegraded, metav1.ConditionTrue, ReasonPodsNotReady,
			fmt.Sprintf("%d/%d nodes ready", readyNodes, totalNodes))
	default:
		setCondition(sn, ConditionTypeReady, metav1.ConditionFalse, ReasonPodsNotReady, "No nodes are ready")
		setCondition(sn, ConditionTypeProgressing, metav1.ConditionTrue, ReasonPodsNotReady, "Waiting for nodes to become ready")
	}
}
