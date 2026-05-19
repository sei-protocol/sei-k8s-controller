package nodetask

import (
	"go.opentelemetry.io/otel/metric"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/observability"
)

var allTaskPhases = []string{
	string(seiv1alpha1.SeiNodeTaskPhasePending),
	string(seiv1alpha1.SeiNodeTaskPhaseRunning),
	string(seiv1alpha1.SeiNodeTaskPhaseComplete),
	string(seiv1alpha1.SeiNodeTaskPhaseFailed),
}

var taskPhaseTracker = observability.NewPhaseTracker(allTaskPhases)

var taskPhaseTransitions metric.Int64Counter

var meter = observability.NewMeter("nodetask")

func init() {
	if _, err := meter.Float64ObservableGauge(
		"sei.controller.seinodetask.phase",
		metric.WithDescription("Current phase of each SeiNodeTask (1=active, 0=inactive)"),
		metric.WithFloat64Callback(taskPhaseTracker.Observe),
	); err != nil {
		panic("otel metric init (seinodetask phase): " + err.Error())
	}

	var err error
	taskPhaseTransitions, err = meter.Int64Counter(
		"sei.controller.seinodetask.phase.transitions",
		metric.WithDescription("Phase state machine transitions"),
	)
	if err != nil {
		panic("otel metric init (seinodetask phase transitions): " + err.Error())
	}
}

func emitTaskPhase(ns, name string, phase seiv1alpha1.SeiNodeTaskPhase) {
	taskPhaseTracker.Set(ns, name, string(phase))
}
