package nodegroup

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

// reconcileDeployment handles the full deployment lifecycle: detecting
// when a deployment is needed, building the plan, driving it to
// completion, and finalizing. Called when IsDeploymentActive or
// NeedsDeployment is true.
func (r *SeiNodeGroupReconciler) reconcileDeployment(
	ctx context.Context,
	group *seiv1alpha1.SeiNodeGroup,
	statusBase client.Patch,
) (ctrl.Result, error) {
	// If no plan exists yet, build one.
	if group.Status.Deployment == nil {
		return r.initializeDeployment(ctx, group, statusBase)
	}

	// Drive the active plan.
	deployment := group.Status.Deployment
	result, err := r.PlanExecutor.ExecutePlan(ctx, group, &deployment.Plan)
	if err != nil {
		return result, err
	}

	switch deployment.Plan.Phase {
	case seiv1alpha1.TaskPlanComplete:
		return r.completeDeployment(ctx, group, statusBase)
	case seiv1alpha1.TaskPlanFailed:
		return r.failDeployment(ctx, group, statusBase)
	}

	return result, nil
}

func (r *SeiNodeGroupReconciler) initializeDeployment(
	ctx context.Context,
	group *seiv1alpha1.SeiNodeGroup,
	statusBase client.Patch,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	dp, err := planner.ForDeployment(group)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving deployment planner: %w", err)
	}

	plan, err := dp.BuildPlan(group, nil)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building deployment plan: %w", err)
	}

	entrantRevision := planner.EntrantRevision(group)
	incumbentRevision := planner.IncumbentRevision(group)
	entrantNodes := planner.EntrantNodeNames(group)

	group.Status.Deployment = &seiv1alpha1.DeploymentStatus{
		Plan:              *plan,
		IncumbentRevision: incumbentRevision,
		EntrantRevision:   entrantRevision,
		EntrantNodes:      entrantNodes,
	}
	group.Status.Phase = seiv1alpha1.GroupPhaseUpgrading

	apimeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionDeploymentInProgress,
		Status:             metav1.ConditionTrue,
		Reason:             "DeploymentStarted",
		Message:            fmt.Sprintf("Deployment to revision %s started", entrantRevision),
		ObservedGeneration: group.Generation,
	})

	if err := r.Status().Patch(ctx, group, statusBase); err != nil {
		return ctrl.Result{}, fmt.Errorf("writing deployment status: %w", err)
	}

	r.Recorder.Eventf(group, corev1.EventTypeNormal, "DeploymentStarted",
		"Deployment started: %s → %s", incumbentRevision, entrantRevision)
	logger.Info("deployment initialized",
		"incumbentRevision", incumbentRevision,
		"entrantRevision", entrantRevision,
		"strategy", group.Spec.UpdateStrategy.Type)

	return planner.ResultRequeueImmediate, nil
}

func (r *SeiNodeGroupReconciler) completeDeployment(
	ctx context.Context,
	group *seiv1alpha1.SeiNodeGroup,
	statusBase client.Patch,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	entrantRevision := group.Status.Deployment.EntrantRevision

	group.Status.ObservedGeneration = group.Generation

	apimeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionDeploymentInProgress,
		Status:             metav1.ConditionFalse,
		Reason:             "DeploymentComplete",
		Message:            fmt.Sprintf("Deployment to revision %s completed", entrantRevision),
		ObservedGeneration: group.Generation,
	})

	r.Recorder.Eventf(group, corev1.EventTypeNormal, "DeploymentComplete",
		"Deployment to revision %s completed successfully", entrantRevision)
	logger.Info("deployment completed", "revision", entrantRevision)

	// Reconcile networking so the Service selector picks up the new revision.
	if err := r.reconcileNetworking(ctx, group); err != nil {
		logger.Error(err, "reconciling networking after deployment")
	}

	if err := r.updateStatus(ctx, group, statusBase); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after deployment: %w", err)
	}
	return planner.ResultRequeueImmediate, nil
}

func (r *SeiNodeGroupReconciler) failDeployment(
	ctx context.Context,
	group *seiv1alpha1.SeiNodeGroup,
	statusBase client.Patch,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	entrantRevision := group.Status.Deployment.EntrantRevision

	group.Status.Phase = seiv1alpha1.GroupPhaseDegraded

	apimeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionDeploymentInProgress,
		Status:             metav1.ConditionFalse,
		Reason:             "DeploymentFailed",
		Message:            "Deployment failed; incumbent nodes remain active. Investigate entrant nodes and retry.",
		ObservedGeneration: group.Generation,
	})

	r.Recorder.Eventf(group, corev1.EventTypeWarning, "DeploymentFailed",
		"Deployment to revision %s failed", entrantRevision)
	logger.Info("deployment failed", "revision", entrantRevision)

	if err := r.updateStatus(ctx, group, statusBase); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after deployment failure: %w", err)
	}
	return ctrl.Result{}, nil
}
