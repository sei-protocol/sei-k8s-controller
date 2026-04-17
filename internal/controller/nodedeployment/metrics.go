package nodedeployment

import (
	"context"

	"go.opentelemetry.io/otel/metric"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/observability"
)

var allGroupPhases = []string{
	string(seiv1alpha1.GroupPhasePending),
	string(seiv1alpha1.GroupPhaseInitializing),
	string(seiv1alpha1.GroupPhaseReady),
	string(seiv1alpha1.GroupPhaseUpgrading),
	string(seiv1alpha1.GroupPhaseDegraded),
	string(seiv1alpha1.GroupPhaseFailed),
	string(seiv1alpha1.GroupPhaseTerminating),
}

var groupPhaseTracker = observability.NewPhaseTracker(allGroupPhases)

var deploymentReplicas metric.Float64Gauge

var meter = observability.NewMeter("nodedeployment")

func init() {
	var err error

	// The observable gauge is registered for its callback side effect.
	_, err = meter.Float64ObservableGauge(
		"sei.controller.seinodedeployment.phase",
		metric.WithDescription("Current phase of each SeiNodeDeployment (1=active, 0=inactive)"),
		metric.WithFloat64Callback(groupPhaseTracker.Observe),
	)
	if err != nil {
		panic("otel metric init: " + err.Error())
	}

	deploymentReplicas, err = meter.Float64Gauge(
		"sei.controller.seinodedeployment.replicas",
		metric.WithDescription("Replica counts for each SeiNodeDeployment"),
	)
	if err != nil {
		panic("otel metric init: " + err.Error())
	}
}

func emitGroupPhase(ns, name string, phase seiv1alpha1.SeiNodeDeploymentPhase) {
	groupPhaseTracker.Set(ns, name, string(phase))
}

func emitGroupReplicas(ctx context.Context, ns, name string, desired, ready int32) {
	deploymentReplicas.Record(ctx, float64(desired),
		metric.WithAttributes(
			observability.AttrNamespace.String(ns),
			observability.AttrName.String(name),
			observability.AttrReplicaState.String("desired"),
		),
	)
	deploymentReplicas.Record(ctx, float64(ready),
		metric.WithAttributes(
			observability.AttrNamespace.String(ns),
			observability.AttrName.String(name),
			observability.AttrReplicaState.String("ready"),
		),
	)
}

func cleanupGroupMetrics(namespace, name string) {
	groupPhaseTracker.Delete(namespace, name)
}
