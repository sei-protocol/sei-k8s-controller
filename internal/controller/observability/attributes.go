package observability

import "go.opentelemetry.io/otel/attribute"

// Metric attribute keys for per-datapoint labels. These use short names
// that match existing Prometheus label names so the OTel Prometheus exporter
// produces identical label names on /metrics (dots → underscores).
var (
	// AttrController identifies which reconciler emitted the metric.
	AttrController = attribute.Key("controller")

	// AttrNamespace is the Kubernetes namespace of the reconciled resource.
	AttrNamespace = attribute.Key("namespace")

	// AttrName is the name of the reconciled resource.
	AttrName = attribute.Key("name")

	// AttrPhase is the lifecycle phase of the resource.
	AttrPhase = attribute.Key("phase")

	// AttrFromPhase is the source phase in a phase transition.
	AttrFromPhase = attribute.Key("from_phase")

	// AttrToPhase is the destination phase in a phase transition.
	AttrToPhase = attribute.Key("to_phase")

	// AttrChainID is the blockchain network identifier.
	AttrChainID = attribute.Key("chain_id")

	// AttrPlanType categorizes the plan (init, node-update, genesis, etc.).
	AttrPlanType = attribute.Key("plan_type")

	// AttrReplicaState is the replica count type (desired, ready).
	// Named explicitly to avoid overloading a generic "type" attribute.
	AttrReplicaState = attribute.Key("replica_state")

	// AttrOutcome is the plan outcome (complete, failed).
	AttrOutcome = attribute.Key("outcome")
)
