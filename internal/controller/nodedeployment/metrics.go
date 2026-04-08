package nodedeployment

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/observability"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
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

var allConditionTypes = []string{
	seiv1alpha1.ConditionNodesReady,
	seiv1alpha1.ConditionExternalServiceReady,
	seiv1alpha1.ConditionRouteReady,
	seiv1alpha1.ConditionIsolationReady,
	seiv1alpha1.ConditionServiceMonitorReady,
}

var (
	groupPhaseGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sei_controller_seinodedeployment_phase",
			Help: "Current phase of each SeiNodeDeployment (1=active, 0=inactive)",
		},
		[]string{"namespace", "name", "phase"},
	)

	groupReplicasGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sei_controller_seinodedeployment_replicas",
			Help: "Replica counts for each SeiNodeDeployment",
		},
		[]string{"namespace", "name", "type"},
	)

	groupConditionGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sei_controller_seinodedeployment_condition",
			Help: "Condition status for each SeiNodeDeployment (1=match, 0=no match)",
		},
		[]string{"namespace", "name", "type", "status"},
	)

	reconcileSubstepDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sei_controller_seinodedeployment_reconcile_substep_duration_seconds",
			Help:    "Duration of individual reconcile substeps",
			Buckets: observability.ReconcileBuckets,
		},
		[]string{"controller", "substep"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		groupPhaseGauge,
		groupReplicasGauge,
		groupConditionGauge,
		reconcileSubstepDuration,
	)
}

func emitGroupPhase(ns, name string, phase seiv1alpha1.SeiNodeDeploymentPhase) {
	observability.EmitPhaseGauge(groupPhaseGauge, ns, name, string(phase), allGroupPhases)
}

func emitGroupReplicas(ns, name string, desired, ready int32) {
	groupReplicasGauge.WithLabelValues(ns, name, "desired").Set(float64(desired))
	groupReplicasGauge.WithLabelValues(ns, name, "ready").Set(float64(ready))
}

func emitGroupConditions(ns, name string, conditions []metav1.Condition) {
	for _, cond := range allConditionTypes {
		current := "Unknown"
		for i := range conditions {
			if conditions[i].Type == cond {
				current = string(conditions[i].Status)
				break
			}
		}
		for _, s := range []string{"True", "False", "Unknown"} {
			val := 0.0
			if s == current {
				val = 1.0
			}
			groupConditionGauge.WithLabelValues(ns, name, cond, s).Set(val)
		}
	}
}

func timeSubstep(substep string, fn func() error) error {
	start := time.Now()
	err := fn()
	reconcileSubstepDuration.WithLabelValues(controllerName, substep).Observe(time.Since(start).Seconds())
	return err
}

func cleanupGroupMetrics(namespace, name string) {
	observability.DeletePhaseGauge(groupPhaseGauge, namespace, name, allGroupPhases)
	for _, typ := range []string{"desired", "ready"} {
		groupReplicasGauge.DeleteLabelValues(namespace, name, typ)
	}
	for _, cond := range allConditionTypes {
		for _, status := range []string{"True", "False", "Unknown"} {
			groupConditionGauge.DeleteLabelValues(namespace, name, cond, status)
		}
	}
	observability.ReconcileErrorsTotal.DeleteLabelValues(controllerName, namespace, name)
	planner.CleanupPlanMetrics("seinodedeployment", namespace, name)
}
