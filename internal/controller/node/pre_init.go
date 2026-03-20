package node

import (
	"context"
	"fmt"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// reconcilePreInitializing drives the PreInitPlan to completion. For nodes
// that don't need pre-init the plan has zero tasks and completes trivially
// without creating any Job infrastructure.
func (r *SeiNodeReconciler) reconcilePreInitializing(ctx context.Context, node *seiv1alpha1.SeiNode, planner NodePlanner) (ctrl.Result, error) {
	plan := node.Status.PreInitPlan

	if plan.Phase == seiv1alpha1.TaskPlanComplete {
		if err := r.cleanupPreInit(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("cleaning up pre-init resources: %w", err)
		}
		return r.setPhase(ctx, node, seiv1alpha1.PhaseInitializing)
	}
	if plan.Phase == seiv1alpha1.TaskPlanFailed {
		// if err := r.cleanupPreInit(ctx, node); err != nil {
		// 	log.FromContext(ctx).Error(err, "failed to clean up pre-init resources after plan failure")
		// }
		return r.setPhase(ctx, node, seiv1alpha1.PhaseFailed)
	}

	if len(plan.Tasks) == 0 {
		patch := client.MergeFrom(node.DeepCopy())
		plan.Phase = seiv1alpha1.TaskPlanComplete
		if err := r.Status().Patch(ctx, node, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking empty pre-init plan complete: %w", err)
		}
		return ctrl.Result{RequeueAfter: immediateRequeue}, nil
	}

	if err := r.ensurePreInitService(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring pre-init service: %w", err)
	}
	job, err := r.ensurePreInitJob(ctx, node)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring pre-init job: %w", err)
	}

	if isJobFailed(job) {
		log.FromContext(ctx).Error(fmt.Errorf("pre-init job failed"), "pre-init job terminated unexpectedly", "job", job.Name)
		patch := client.MergeFrom(node.DeepCopy())
		plan.Phase = seiv1alpha1.TaskPlanFailed
		if task := currentTask(plan); task != nil {
			task.Status = seiv1alpha1.PlannedTaskFailed
			task.Error = jobFailureReason(job)
		}
		if err := r.Status().Patch(ctx, node, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking pre-init plan failed after job failure: %w", err)
		}
		return ctrl.Result{RequeueAfter: immediateRequeue}, nil
	}

	if isJobComplete(job) {
		log.FromContext(ctx).Info("pre-init job completed, advancing plan", "job", job.Name)
		patch := client.MergeFrom(node.DeepCopy())
		for i := range plan.Tasks {
			if plan.Tasks[i].Status != seiv1alpha1.PlannedTaskComplete {
				plan.Tasks[i].Status = seiv1alpha1.PlannedTaskComplete
			}
		}
		plan.Phase = seiv1alpha1.TaskPlanComplete
		if err := r.Status().Patch(ctx, node, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking pre-init plan complete after job success: %w", err)
		}
		return ctrl.Result{RequeueAfter: immediateRequeue}, nil
	}

	sc := r.buildJobSidecarClient(node)
	if sc == nil {
		log.FromContext(ctx).Info("pre-init job sidecar not reachable yet, will retry")
		return ctrl.Result{RequeueAfter: taskPollInterval}, nil
	}

	result, err := r.executePlan(ctx, node, plan, planner, sc)
	if err != nil {
		return result, err
	}

	if plan.Phase == seiv1alpha1.TaskPlanComplete {
		if err := r.cleanupPreInit(ctx, node); err != nil {
			return ctrl.Result{}, fmt.Errorf("cleaning up pre-init resources: %w", err)
		}
		return r.setPhase(ctx, node, seiv1alpha1.PhaseInitializing)
	}
	if plan.Phase == seiv1alpha1.TaskPlanFailed {
		// if err := r.cleanupPreInit(ctx, node); err != nil {
		// 	log.FromContext(ctx).Error(err, "failed to clean up pre-init resources after plan failure")
		// }
		return r.setPhase(ctx, node, seiv1alpha1.PhaseFailed)
	}
	return result, nil
}

// ensurePreInitService creates the headless Service for pre-init Job pod DNS.
func (r *SeiNodeReconciler) ensurePreInitService(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	key := types.NamespacedName{Name: preInitJobName(node), Namespace: node.Namespace}
	existing := &corev1.Service{}
	err := r.Get(ctx, key, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	desired := generatePreInitService(node)
	if err := ctrl.SetControllerReference(node, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	return r.Create(ctx, desired)
}

// ensurePreInitJob creates the PreInit Job if it doesn't already exist.
func (r *SeiNodeReconciler) ensurePreInitJob(ctx context.Context, node *seiv1alpha1.SeiNode) (*batchv1.Job, error) {
	existing := &batchv1.Job{}
	key := types.NamespacedName{Name: preInitJobName(node), Namespace: node.Namespace}
	err := r.Get(ctx, key, existing)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	desired := generatePreInitJob(node, r.Platform)
	if err := ctrl.SetControllerReference(node, desired, r.Scheme); err != nil {
		return nil, fmt.Errorf("setting owner reference: %w", err)
	}
	if err := r.Create(ctx, desired); err != nil {
		return nil, fmt.Errorf("creating pre-init job: %w", err)
	}
	return desired, nil
}

// buildJobSidecarClient constructs a sidecar client targeting the pre-init
// Job's pod via hostname/subdomain DNS. Returns nil if the client can't be built.
func (r *SeiNodeReconciler) buildJobSidecarClient(node *seiv1alpha1.SeiNode) SidecarStatusClient {
	if r.BuildSidecarClientFn != nil {
		return r.BuildSidecarClientFn(node)
	}
	c, err := sidecar.NewSidecarClient(preInitSidecarURL(node))
	if err != nil {
		log.Log.Info("failed to build job sidecar client", "node", node.Name, "error", err)
		return nil
	}
	return c
}

// isJobFailed returns true if the Job has a Failed condition.
func isJobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// isJobComplete returns true if the Job has a Complete condition.
func isJobComplete(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// jobFailureReason extracts a human-readable failure reason from the Job's conditions.
func jobFailureReason(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue && c.Message != "" {
			return c.Message
		}
	}
	return "pre-init job failed"
}

// cleanupPreInit removes the pre-init Job and its headless Service. It
// returns an error while the Job still exists so the reconciler requeues,
// preventing the StatefulSet from mounting the RWO PVC before the Job pod
// has fully released it.
func (r *SeiNodeReconciler) cleanupPreInit(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	key := types.NamespacedName{Name: preInitJobName(node), Namespace: node.Namespace}

	job := &batchv1.Job{}
	if err := r.Get(ctx, key, job); err == nil {
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting pre-init job: %w", err)
		}
		// Foreground deletion is async; requeue until the Job is fully gone
		// to avoid RWO PVC mount conflicts with the StatefulSet.
		return fmt.Errorf("waiting for pre-init job %s to be fully deleted", key.Name)
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	// Job is gone -- safe to clean up the headless service.
	svc := &corev1.Service{}
	if err := r.Get(ctx, key, svc); err == nil {
		if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting pre-init service: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	return nil
}
