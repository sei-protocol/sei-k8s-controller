package observability

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

// Meter is the shared OTel meter for all controller metrics.
var Meter = otel.Meter("github.com/sei-protocol/sei-k8s-controller")

// ReconcileErrors counts reconcile errors per controller and namespace.
var ReconcileErrors metric.Int64Counter

func init() {
	var err error
	ReconcileErrors, err = Meter.Int64Counter(
		"sei.controller.reconcile.errors",
		metric.WithDescription("Reconcile errors per controller and namespace"),
	)
	if err != nil {
		panic("otel metric init: " + err.Error())
	}
}

// InitBuckets covers 10s to 1h for phase duration metrics.
var InitBuckets = []float64{10, 30, 60, 120, 300, 600, 900, 1200, 1800, 3600}

// PlanBuckets covers 1s to 30m for plan duration metrics.
var PlanBuckets = []float64{1, 5, 10, 30, 60, 120, 300, 600, 1800}

// PhaseTracker maintains the current phase for each resource and implements
// the OTel observable gauge callback. On deletion, removing the key from
// the map causes the gauge to stop reporting — no explicit cleanup needed.
type PhaseTracker struct {
	mu        sync.RWMutex
	phases    map[string]string // key: "namespace/name", value: current phase
	AllPhases []string
}

// NewPhaseTracker creates a PhaseTracker for the given set of valid phases.
func NewPhaseTracker(allPhases []string) *PhaseTracker {
	return &PhaseTracker{
		phases:    make(map[string]string),
		AllPhases: allPhases,
	}
}

// Set records the current phase for a resource.
func (pt *PhaseTracker) Set(ns, name, phase string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	pt.phases[ns+"/"+name] = phase
}

// Delete removes a resource from tracking.
func (pt *PhaseTracker) Delete(ns, name string) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	delete(pt.phases, ns+"/"+name)
}

// Observe is the callback for an OTel observable gauge. It emits 1.0 for
// the current phase and 0.0 for all others, following the kube-state-metrics
// convention.
func (pt *PhaseTracker) Observe(_ context.Context, o metric.Float64Observer) error {
	pt.mu.RLock()
	defer pt.mu.RUnlock()

	for key, current := range pt.phases {
		ns, name := splitKey(key)
		for _, p := range pt.AllPhases {
			val := 0.0
			if p == current {
				val = 1.0
			}
			o.Observe(val,
				metric.WithAttributes(
					AttrNamespace.String(ns),
					AttrName.String(name),
					AttrPhase.String(p),
				),
			)
		}
	}
	return nil
}

func splitKey(key string) (string, string) {
	for i := range key {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}
