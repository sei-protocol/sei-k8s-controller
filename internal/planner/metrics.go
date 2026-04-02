package planner

import (
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	taskRetriesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sei_controller_task_retries_total",
			Help: "Total task retry attempts",
		},
		[]string{"controller", "namespace", "task_type"},
	)

	taskFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sei_controller_task_failures_total",
			Help: "Total terminal task failures",
		},
		[]string{"controller", "namespace", "task_type"},
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
		taskRetriesTotal,
		taskFailuresTotal,
		planActive,
	)
}

// controllerName derives a lowercase controller label from the object's GVK Kind.
func controllerName(obj client.Object) string {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		return "unknown"
	}
	return strings.ToLower(kind)
}

// CleanupPlanMetrics removes the plan_active gauge for a deleted resource.
func CleanupPlanMetrics(controller, namespace, name string) {
	planActive.DeleteLabelValues(controller, namespace, name)
}
