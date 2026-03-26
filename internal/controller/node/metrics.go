package node

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/observability"
)

var allNodePhases = []string{
	string(seiv1alpha1.PhasePending),
	string(seiv1alpha1.PhaseInitializing),
	string(seiv1alpha1.PhaseRunning),
	string(seiv1alpha1.PhaseFailed),
	string(seiv1alpha1.PhaseTerminating),
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

	sidecarRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sei_controller_sidecar_request_duration_seconds",
			Help:    "Duration of HTTP requests to the seictl sidecar",
			Buckets: observability.ReconcileBuckets,
		},
		[]string{"namespace", "method", "route", "status_code"},
	)

	sidecarUnreachableTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sei_controller_sidecar_unreachable_total",
			Help: "Number of times the sidecar was unreachable",
		},
		[]string{"namespace", "node"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		nodePhaseGauge,
		nodePhaseTransitions,
		nodeInitDuration,
		nodeLastInitDuration,
		sidecarRequestDuration,
		sidecarUnreachableTotal,
	)
}

func emitNodePhase(ns, name string, phase seiv1alpha1.SeiNodePhase) {
	observability.EmitPhaseGauge(nodePhaseGauge, ns, name, string(phase), allNodePhases)
}

func cleanupNodeMetrics(namespace, name string) {
	observability.DeletePhaseGauge(nodePhaseGauge, namespace, name, allNodePhases)
	nodeLastInitDuration.DeleteLabelValues(namespace, name)
	sidecarUnreachableTotal.DeleteLabelValues(namespace, name)
	observability.ReconcileErrorsTotal.DeleteLabelValues(seiNodeControllerName, namespace, name)
}
