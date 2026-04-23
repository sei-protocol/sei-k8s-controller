package planner

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/metric"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/observability"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// Only False/NotReady triggers a plan; Unknown cases re-probe next tick.
func probeSidecarHealth(ctx context.Context, node *seiv1alpha1.SeiNode, client task.SidecarClient) {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	var status metav1.ConditionStatus
	var reason, message, outcome string

	ready, err := client.Healthz(probeCtx)
	switch {
	case err != nil:
		status = metav1.ConditionUnknown
		reason = "Unreachable"
		message = fmt.Sprintf("sidecar Healthz error: %v", err)
		outcome = "unreachable"
	case ready:
		status = metav1.ConditionTrue
		reason = "Ready"
		message = "sidecar returned 200"
		outcome = "ready"
	default:
		status = metav1.ConditionFalse
		reason = "NotReady"
		message = "sidecar returned 503, re-issuing mark-ready"
		outcome = "not_ready"
	}

	sidecarHealthProbes.Add(ctx, 1,
		metric.WithAttributes(
			observability.AttrController.String("seinode"),
			observability.AttrNamespace.String(node.Namespace),
			observability.AttrOutcome.String(outcome),
		),
	)

	var readyVal float64
	if status == metav1.ConditionTrue {
		readyVal = 1
	}
	sidecarReadyTracker.Set(node.Namespace, node.Name, readyVal)

	setSidecarReadyCondition(node, status, reason, message)
}

func setSidecarReadyCondition(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionSidecarReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}
