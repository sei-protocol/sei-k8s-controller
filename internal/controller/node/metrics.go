package node

import (
	"go.opentelemetry.io/otel/metric"

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

var nodePhaseTracker = observability.NewPhaseTracker(allNodePhases)

var (
	// nodePhaseTransitions counts phase transitions.
	nodePhaseTransitions metric.Int64Counter

	// nodePhaseDuration records time spent in each phase when transitioning out.
	nodePhaseDuration metric.Float64Histogram
)

var meter = observability.NewMeter("node")

func init() {
	var err error

	// The observable gauge is registered for its callback side effect.
	_, err = meter.Float64ObservableGauge(
		"sei.controller.seinode.phase",
		metric.WithDescription("Current phase of each SeiNode (1=active, 0=inactive)"),
		metric.WithFloat64Callback(nodePhaseTracker.Observe),
	)
	handleInitErr(err)

	nodePhaseTransitions, err = meter.Int64Counter(
		"sei.controller.seinode.phase.transitions",
		metric.WithDescription("Phase state machine transitions"),
	)
	handleInitErr(err)

	nodePhaseDuration, err = meter.Float64Histogram(
		"sei.controller.seinode.phase.duration",
		metric.WithDescription("Time spent in each phase before transitioning"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(observability.InitBuckets...),
	)
	handleInitErr(err)
}

func handleInitErr(err error) {
	if err != nil {
		panic("otel metric init: " + err.Error())
	}
}

func emitNodePhase(ns, name string, phase seiv1alpha1.SeiNodePhase) {
	nodePhaseTracker.Set(ns, name, string(phase))
}

func cleanupNodeMetrics(namespace, name string) {
	nodePhaseTracker.Delete(namespace, name)
}
