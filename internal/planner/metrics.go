package planner

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/metric"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/observability"
)

var (
	// planDuration records wall-clock time from plan creation to completion/failure.
	// The outcome attribute (complete/failed) distinguishes success from failure,
	// and _count gives you the total plan count — no separate counter needed.
	planDuration metric.Float64Histogram

	// planActiveCount tracks the number of active plans per controller/namespace.
	planActiveCount metric.Int64UpDownCounter

	sidecarHealthProbes metric.Int64Counter
)

var meter = observability.NewMeter("planner")

func init() {
	var err error

	planDuration, err = meter.Float64Histogram(
		"sei.controller.plan.duration",
		metric.WithDescription("Wall-clock time from plan creation to completion or failure"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(observability.PlanBuckets...),
	)
	handlePlanInitErr(err)

	planActiveCount, err = meter.Int64UpDownCounter(
		"sei.controller.plan.active",
		metric.WithDescription("Number of active plans"),
	)
	handlePlanInitErr(err)

	sidecarHealthProbes, err = meter.Int64Counter(
		"sei.controller.seinode.sidecar_health_probes",
		metric.WithDescription("Sidecar Healthz probe outcomes observed during plan resolution"),
	)
	handlePlanInitErr(err)

	_, err = meter.Float64ObservableGauge(
		"sei.controller.seinode.sidecar_ready",
		metric.WithDescription("Latest observed sidecar readiness per SeiNode (1=ready, 0=not-ready or unknown)"),
		metric.WithFloat64Callback(sidecarReadyTracker.Observe),
	)
	handlePlanInitErr(err)
}

var sidecarReadyTracker = newSidecarReadyTracker()

type srTracker struct {
	mu    sync.RWMutex
	state map[string]float64
}

func newSidecarReadyTracker() *srTracker {
	return &srTracker{state: make(map[string]float64)}
}

func (t *srTracker) Set(ns, name string, ready float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state[ns+"/"+name] = ready
}

func (t *srTracker) Observe(_ context.Context, o metric.Float64Observer) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for key, v := range t.state {
		ns, name := splitKey(key)
		o.Observe(v,
			metric.WithAttributes(
				observability.AttrNamespace.String(ns),
				observability.AttrName.String(name),
			),
		)
	}
	return nil
}

func splitKey(key string) (ns, name string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return "", key
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
