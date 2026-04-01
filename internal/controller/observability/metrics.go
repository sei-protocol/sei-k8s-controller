package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ReconcileBuckets extends prometheus.DefBuckets to 60s, covering slow
// API server writes, sidecar interactions, and controller-runtime's
// default reconcile timeout.
var ReconcileBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// InitBuckets covers 10s to 1h for node initialisation durations
// (PVC binding, snapshot restore, sidecar health).
var InitBuckets = []float64{10, 30, 60, 120, 300, 600, 900, 1200, 1800, 3600}

// ReconcileErrorsTotal tracks reconcile errors beyond what controller-runtime
// tracks. The "controller" label disambiguates between controllers.
var ReconcileErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "sei_controller_reconcile_errors_total",
		Help: "Reconcile errors beyond what controller-runtime tracks",
	},
	[]string{"controller", "namespace", "name"},
)

func init() {
	metrics.Registry.MustRegister(ReconcileErrorsTotal)
}

// EmitPhaseGauge sets 1.0 for the current phase and 0.0 for all others,
// following the kube-state-metrics convention (e.g., kube_pod_status_phase).
func EmitPhaseGauge(gauge *prometheus.GaugeVec, ns, name, current string, allPhases []string) {
	for _, p := range allPhases {
		val := 0.0
		if p == current {
			val = 1.0
		}
		gauge.WithLabelValues(ns, name, p).Set(val)
	}
}

// DeletePhaseGauge removes all phase series for a deleted resource.
func DeletePhaseGauge(gauge *prometheus.GaugeVec, ns, name string, allPhases []string) {
	for _, p := range allPhases {
		gauge.DeleteLabelValues(ns, name, p)
	}
}

