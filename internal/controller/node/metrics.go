package node

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/observability"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

var allNodePhases = []string{
	string(seiv1alpha1.PhasePending),
	string(seiv1alpha1.PhaseInitializing),
	string(seiv1alpha1.PhaseRunning),
	string(seiv1alpha1.PhaseFailed),
	string(seiv1alpha1.PhaseTerminating),
}

var allTaskStatuses = []string{
	string(seiv1alpha1.TaskPending),
	string(seiv1alpha1.TaskComplete),
	string(seiv1alpha1.TaskFailed),
}

var (
	nodePhaseGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sei_controller_seinode_phase",
			Help: "Current phase of each SeiNode (1=active, 0=inactive)",
		},
		[]string{"namespace", "name", "phase"},
	)

	nodePhaseTransitions = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sei_controller_seinode_phase_transitions_total",
			Help: "Phase state machine transitions",
		},
		[]string{"namespace", "from", "to"},
	)

	nodeInitDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sei_controller_seinode_init_duration_seconds",
			Help:    "Time from Pending to Running",
			Buckets: observability.InitBuckets,
		},
		[]string{"namespace", "chain_id"},
	)

	nodeLastInitDuration = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sei_controller_seinode_last_init_duration_seconds",
			Help: "Per-node init duration, set once when node reaches Running",
		},
		[]string{"namespace", "name"},
	)

	sidecarUnreachableTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sei_controller_sidecar_unreachable_total",
			Help: "Number of times the sidecar was unreachable",
		},
		[]string{"namespace", "node"},
	)

	monitorTaskCompletedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sei_controller_monitor_task_completed_total",
			Help: "Monitor task terminal state transitions (DivergenceDetected, TaskFailed, TaskLost)",
		},
		[]string{"namespace", "node", "task_type", "reason"},
	)

	monitorTaskStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sei_controller_monitor_task_status",
			Help: "Current status of each monitor task (1=active, 0=inactive)",
		},
		[]string{"namespace", "node", "task_type", "status"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		nodePhaseGauge,
		nodePhaseTransitions,
		nodeInitDuration,
		nodeLastInitDuration,
		sidecarUnreachableTotal,
		monitorTaskCompletedTotal,
		monitorTaskStatus,
	)
}

func emitNodePhase(ns, name string, phase seiv1alpha1.SeiNodePhase) {
	observability.EmitPhaseGauge(nodePhaseGauge, ns, name, string(phase), allNodePhases)
}

func emitMonitorTaskTerminal(ns, node, taskType, reason string) {
	monitorTaskCompletedTotal.WithLabelValues(ns, node, taskType, reason).Inc()
}

func emitMonitorTaskStatus(ns, node, taskType, status string) {
	for _, s := range allTaskStatuses {
		val := float64(0)
		if s == status {
			val = 1
		}
		monitorTaskStatus.WithLabelValues(ns, node, taskType, s).Set(val)
	}
}

func cleanupMonitorTaskMetrics(ns, name string, taskTypes []string) {
	for _, tt := range taskTypes {
		for _, s := range allTaskStatuses {
			monitorTaskStatus.DeleteLabelValues(ns, name, tt, s)
		}
	}
}

func cleanupNodeMetrics(namespace, name string) {
	observability.DeletePhaseGauge(nodePhaseGauge, namespace, name, allNodePhases)
	nodeLastInitDuration.DeleteLabelValues(namespace, name)
	sidecarUnreachableTotal.DeleteLabelValues(namespace, name)
	observability.ReconcileErrorsTotal.DeleteLabelValues(seiNodeControllerName, namespace, name)
	cleanupMonitorTaskMetrics(namespace, name, []string{planner.TaskResultExport})
	planner.CleanupPlanMetrics("seinode", namespace, name)
}
