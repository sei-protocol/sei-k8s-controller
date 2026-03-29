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

// startDeployment detects a pending deployment, builds the plan, and
// writes the initial DeploymentStatus. The next reconcile will pick
// up the active plan via reconcileDeployment.
func (r *SeiNodeGroupReconciler) startDeployment(
	ctx context.Context,
	group *seiv1alpha1.SeiNodeGroup,
	statusBase client.Patch,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Collect current (blue) node names.
	nodes, err := r.listChildSeiNodes(ctx, group)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing child SeiNodes for deployment: %w", err)
	}
	blueNames := make([]string, len(nodes))
	for i := range nodes {
		blueNames[i] = nodes[i].Name
	}

	deployment, err := planner.BuildDeploymentPlan(group, blueNames)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("building deployment plan: %w", err)
	}

	group.Status.Deployment = deployment
	group.Status.Phase = seiv1alpha1.GroupPhaseUpgrading

	apimeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionDeploymentInProgress,
		Status:             metav1.ConditionTrue,
		Reason:             "DeploymentStarted",
		Message:            fmt.Sprintf("Blue-green deployment to revision %s started", deployment.IncomingRevision),
		ObservedGeneration: group.Generation,
	})

	if err := r.Status().Patch(ctx, group, statusBase); err != nil {
		return ctrl.Result{}, fmt.Errorf("writing deployment status: %w", err)
	}

	r.Recorder.Eventf(group, corev1.EventTypeNormal, "DeploymentStarted",
		"Blue-green deployment started: %s → %s (%d blue, %d green)",
		deployment.ActiveRevision, deployment.IncomingRevision,
		len(blueNames), len(deployment.GreenNodes))

	logger.Info("deployment started",
		"activeRevision", deployment.ActiveRevision,
		"incomingRevision", deployment.IncomingRevision,
		"strategy", group.Spec.UpdateStrategy.Type)

	return planner.ResultRequeueImmediate, nil
}

// reconcileDeployment drives an active deployment plan to completion.
// When the plan completes, the deployment status is finalized and
// ObservedGeneration is updated. When the plan fails, the group is
// marked Degraded.
func (r *SeiNodeGroupReconciler) reconcileDeployment(
	ctx context.Context,
	group *seiv1alpha1.SeiNodeGroup,
	statusBase client.Patch,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	deployment := group.Status.Deployment

	result, err := r.PlanExecutor.ExecutePlan(ctx, group, &deployment.Plan)
	if err != nil {
		return result, err
	}

	switch deployment.Plan.Phase {
	case seiv1alpha1.TaskPlanComplete:
		// Deployment succeeded. Update observed generation and clear
		// the deployment-in-progress condition.
		group.Status.ObservedGeneration = group.Generation

		apimeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:               seiv1alpha1.ConditionDeploymentInProgress,
			Status:             metav1.ConditionFalse,
			Reason:             "DeploymentComplete",
			Message:            fmt.Sprintf("Deployment to revision %s completed", deployment.IncomingRevision),
			ObservedGeneration: group.Generation,
		})

		r.Recorder.Eventf(group, corev1.EventTypeNormal, "DeploymentComplete",
			"Blue-green deployment to revision %s completed successfully",
			deployment.IncomingRevision)

		logger.Info("deployment completed", "revision", deployment.IncomingRevision)

		// Status will be patched by the plan executor, but we also need
		// to update networking to point at the new revision. Run a
		// networking reconciliation pass.
		if netErr := r.reconcileNetworking(ctx, group); netErr != nil {
			logger.Error(netErr, "reconciling networking after deployment")
		}

		if err := r.updateStatus(ctx, group, statusBase); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating status after deployment: %w", err)
		}
		return planner.ResultRequeueImmediate, nil

	case seiv1alpha1.TaskPlanFailed:
		// Deployment failed. Mark Degraded but keep blue running.
		group.Status.Phase = seiv1alpha1.GroupPhaseDegraded

		apimeta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
			Type:               seiv1alpha1.ConditionDeploymentInProgress,
			Status:             metav1.ConditionFalse,
			Reason:             "DeploymentFailed",
			Message:            "Deployment failed; blue nodes remain active. Investigate green nodes and retry.",
			ObservedGeneration: group.Generation,
		})

		r.Recorder.Eventf(group, corev1.EventTypeWarning, "DeploymentFailed",
			"Blue-green deployment to revision %s failed",
			deployment.IncomingRevision)

		logger.Info("deployment failed", "revision", deployment.IncomingRevision)

		if err := r.updateStatus(ctx, group, statusBase); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating status after deployment failure: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Plan still active — requeue at the plan executor's requested interval.
	return result, nil
}
