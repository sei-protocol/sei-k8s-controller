package planner

import (
	"go.opentelemetry.io/otel/metric"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/observability"
)

var (
	// planFailures counts terminal plan failures.
	planFailures metric.Int64Counter

	// planDuration records wall-clock time from plan creation to completion/failure.
	planDuration metric.Float64Histogram

	// planActiveCount tracks the number of active plans per controller/namespace.
	planActiveCount metric.Int64UpDownCounter
)

func init() {
	var err error

	planFailures, err = observability.Meter.Int64Counter(
		"sei.controller.plan.failures",
		metric.WithDescription("Terminal plan failures"),
	)
	handlePlanInitErr(err)

	planDuration, err = observability.Meter.Float64Histogram(
		"sei.controller.plan.duration",
		metric.WithDescription("Wall-clock time from plan creation to completion or failure"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(observability.PlanBuckets...),
	)
	handlePlanInitErr(err)

	planActiveCount, err = observability.Meter.Int64UpDownCounter(
		"sei.controller.plan.active",
		metric.WithDescription("Number of active plans"),
	)
	handlePlanInitErr(err)
}

func handlePlanInitErr(err error) {
	if err != nil {
		panic("otel metric init: " + err.Error())
	}
}

// controllerName returns the controller label for metrics.
func controllerName(obj client.Object) string {
	switch obj.(type) {
	case *seiv1alpha1.SeiNode:
		return "seinode"
	case *seiv1alpha1.SeiNodeDeployment:
		return "seinodedeployment"
	default:
		return unknownValue
	}
}

// CleanupPlanMetrics is a no-op in OTel — active series stop being
// reported when no new observations occur. Retained for interface compat.
func CleanupPlanMetrics(_, _, _ string) {}
