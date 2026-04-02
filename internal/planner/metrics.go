package planner

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/sei-protocol/sei-k8s-controller/internal/controller/observability"
)

var (
	taskDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sei_controller_task_duration_seconds",
			Help:    "Duration of individual task execution from submission to terminal state",
			Buckets: observability.InitBuckets,
		},
		[]string{"controller", "task_type", "status"},
	)

	taskRetriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sei_controller_task_retries_total",
			Help: "Total task retry attempts",
		},
		[]string{"controller", "task_type"},
	)

	taskFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sei_controller_task_failures_total",
			Help: "Total terminal task failures",
		},
		[]string{"controller", "task_type"},
	)

	planActive = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sei_controller_plan_active",
			Help: "Whether a plan is currently in progress (1=active, 0=inactive)",
		},
		[]string{"controller", "namespace", "name"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		taskDurationSeconds,
		taskRetriesTotal,
		taskFailuresTotal,
		planActive,
	)
}

// controllerName derives a lowercase controller label from the object's GVK Kind.
// Falls back to "unknown" when the Kind is not populated (e.g. in unit tests
// where GVK is stripped by the fake client).
func controllerName(obj client.Object) string {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		return "unknown"
	}
	return strings.ToLower(kind)
}
