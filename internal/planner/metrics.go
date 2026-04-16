package planner

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
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

// controllerName returns the controller label for metrics. Uses a type switch
// because controller-runtime strips GVK from objects fetched via client.Get.
func controllerName(obj client.Object) string {
	switch obj.(type) {
	case *seiv1alpha1.SeiNode:
		return "seinode"
	case *seiv1alpha1.SeiNodeDeployment:
		return "seinodedeployment"
	default:
		return "unknown"
	}
}

// CleanupPlanMetrics removes the plan_active gauge for a deleted resource.
func CleanupPlanMetrics(controller, namespace, name string) {
	planActive.DeleteLabelValues(controller, namespace, name)
}
