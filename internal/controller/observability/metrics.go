package observability

import (
	"fmt"
	"strings"

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

// Known sidecar route templates for label normalization.
var knownRoutes = map[string]string{
	"/v0/tasks":   "/v0/tasks",
	"/v0/healthz": "/v0/healthz",
	"/v0/status":  "/v0/status",
}

// NormalizeRoute maps raw HTTP paths to bounded route templates
// to prevent unbounded cardinality from parameterised paths.
func NormalizeRoute(path string) string {
	if r, ok := knownRoutes[path]; ok {
		return r
	}
	if strings.HasPrefix(path, "/v0/tasks/") {
		return "/v0/tasks/:id"
	}
	return "other"
}

// NormalizeStatusCode buckets HTTP status codes into bounded classes
// to prevent unbounded cardinality from unexpected status codes.
func NormalizeStatusCode(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500 && code < 600:
		return "5xx"
	default:
		return fmt.Sprintf("%dxx", code/100)
	}
}
