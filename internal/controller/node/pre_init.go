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
		if err := r.cleanupPreInit(ctx, node); err != nil {
			log.FromContext(ctx).Error(err, "failed to clean up pre-init resources after plan failure")
		}
		return r.setPhase(ctx, node, seiv1alpha1.PhaseFailed)
	}

	if len(plan.Tasks) == 0 {
		patch := client.MergeFrom(node.DeepCopy())
		plan.Phase = seiv1alpha1.TaskPlanComplete
		if err := r.Status().Patch(ctx, node, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("marking empty pre-init plan complete: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if err := r.ensurePreInitService(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring pre-init service: %w", err)
	}
	if _, err := r.ensurePreInitJob(ctx, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring pre-init job: %w", err)
	}

	sc := r.buildJobSidecarClient(node)
	if sc == nil {
		log.FromContext(ctx).Info("pre-init job sidecar not reachable yet, will retry")
		return ctrl.Result{RequeueAfter: bootstrapPollInterval}, nil
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
		if err := r.cleanupPreInit(ctx, node); err != nil {
			log.FromContext(ctx).Error(err, "failed to clean up pre-init resources after plan failure")
		}
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
		return nil
	}
	return c
}

// cleanupPreInit removes the pre-init Job and its headless Service.
func (r *SeiNodeReconciler) cleanupPreInit(ctx context.Context, node *seiv1alpha1.SeiNode) error {
	key := types.NamespacedName{Name: preInitJobName(node), Namespace: node.Namespace}

	job := &batchv1.Job{}
	if err := r.Get(ctx, key, job); err == nil {
		if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting pre-init job: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

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
