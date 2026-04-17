# OpenTelemetry Telemetry Strategy

## Overview

Migrate the controller from `prometheus/client_golang` to the OpenTelemetry metrics SDK. Designed around the Google SRE Four Golden Signals — start with core service health, add granularity once we understand sharp edges.

## Architecture

Single `MeterProvider` with two readers:

```
                                    +---> OTLP PeriodicReader ---> user's collector
                                    |
OTel MeterProvider (with 2 readers) |
                                    |
                                    +---> Prometheus Exporter ---> controller-runtime /metrics
```

- **Prometheus exporter**: registers into controller-runtime's `metrics.Registry`. OTel metrics appear alongside controller-runtime's own metrics on `/metrics`. Zero scraping changes.
- **OTLP exporter**: enabled when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. Users point at any backend.

## Configuration

Standard OTel environment variables — no custom flags:
- `OTEL_EXPORTER_OTLP_ENDPOINT` — collector address
- `OTEL_SERVICE_NAME` — override (defaults to `sei-k8s-controller`)
- `OTEL_RESOURCE_ATTRIBUTES` — user-provided extras

## Resource Attributes (Soft Context)

| Attribute | Source |
|-----------|--------|
| `service.name` | Hardcoded: `sei-k8s-controller` |
| `service.version` | Build-time ldflags |
| `k8s.pod.name` | Downward API |
| `k8s.namespace.name` | Downward API |

## Four Golden Signals → 7 Metrics

### LATENCY (2 metrics)

**1. `sei.controller.node.phase.duration`** — Float64 Histogram (unit: s)
- Dimensions: `namespace`, `chain_id`, `phase`
- Buckets: 10, 30, 60, 120, 300, 600, 900, 1800, 3600
- Records time spent in each phase when a node transitions out. Phase IS the dimension — no separate init-specific metric. Initializing durations tell you bootstrap health. Running durations are expected to be long.
- Requires adding `PhaseTransitionTime` to SeiNodeStatus (set on every phase transition, observed on the next transition).
- Cardinality at 200 nodes: ~5 phases × ~5 chains = ~25 histogram series. No per-node label.

**2. `sei.controller.plan.duration`** — Float64 Histogram (unit: s)
- Dimensions: `controller`, `namespace`, `plan_type`
- Buckets: 1, 5, 10, 30, 60, 120, 300, 600, 1800
- Records wall-clock time from plan creation to completion/failure. Answers "how long do init plans take vs node-update plans?"
- Cardinality: ~2 controllers × ~4 plan_types = ~8 histogram series.

### TRAFFIC (2 metrics)

**3. `sei.controller.node.phase`** — Observable Float64 Gauge
- Dimensions: `namespace`, `name`, `phase`
- 0/1 per phase (kube-state-metrics pattern). Observable gauge with phaseTracker callback — cleanup on deletion is automatic.
- Cardinality at 200 nodes: 200 × 5 phases = 1,000 series.

**4. `sei.controller.phase.transitions`** — Int64 Counter
- Dimensions: `controller`, `namespace`, `from_phase`, `to_phase`
- Unified for both SeiNode and SeiNodeDeployment (the `controller` label disambiguates).
- Cardinality: ~2 controllers × ~10 valid transitions = ~20 series.

### ERRORS (2 metrics)

**5. `sei.controller.plan.failures`** — Int64 Counter
- Dimensions: `controller`, `namespace`, `plan_type`
- Incremented when a plan reaches terminal Failed state.
- Cardinality: ~2 controllers × ~4 plan_types = ~8 series.

**6. `sei.controller.reconcile.errors`** — Int64 Counter
- Dimensions: `controller`, `namespace`
- No per-node `name` label — unbounded cardinality at scale. Namespace-scoped is sufficient for alerting; logs have the node name for debugging.
- Cardinality: ~2 controllers × ~3 namespaces = ~6 series.

### SATURATION (1 metric)

**7. `sei.controller.plan.active`** — Int64 Gauge
- Dimensions: `controller`, `namespace`
- Aggregate count of in-progress plans (not per-node). Answers "how loaded is the controller?"
- Cardinality: ~2 controllers × ~3 namespaces = ~6 series.

### Deployment-Level (optional, low cardinality)

These cover SeiNodeDeployment resources (3-10 per cluster, not 200):

**8. `sei.controller.deployment.phase`** — Observable Float64 Gauge
- Dimensions: `namespace`, `name`, `phase`
- Same 0/1 pattern.

**9. `sei.controller.deployment.replicas`** — Float64 Gauge
- Dimensions: `namespace`, `name`, `replica_state` (desired/ready)
- Renamed from `type` to `replica_state` to avoid attribute overload.

## What Gets Cut

| Metric | Reason |
|--------|--------|
| `seinode.init_duration_seconds` | Replaced by generic `node.phase.duration{phase=Initializing}` |
| `seinode.last_init_duration_seconds` | Per-node gauge, high cardinality, replaced by phase duration histogram |
| `seinode.phase_transitions_total` | Replaced by unified `phase.transitions` counter |
| `task.retries_total` | Task-level detail, too granular for launch |
| `task.failures_total` | Rolled up into `plan.failures` |
| `reconcile_errors_total` (per-name) | Fixed: drop `name` label, keep namespace-scoped |
| `seinodedeployment.condition` | Conditions are in CRD status — queryable via kube API, not needed in metrics for launch |
| `seinodedeployment.reconcile_substep_duration` | Internal implementation detail |

## Attribute Constants

```go
var (
    AttrController    = attribute.Key("controller")
    AttrNamespace     = attribute.Key("namespace")
    AttrName          = attribute.Key("name")
    AttrPhase         = attribute.Key("phase")
    AttrFromPhase     = attribute.Key("from_phase")
    AttrToPhase       = attribute.Key("to_phase")
    AttrChainID       = attribute.Key("chain_id")
    AttrPlanType      = attribute.Key("plan_type")
    AttrReplicaState  = attribute.Key("replica_state")
)
```

No overloaded `type` attribute.

## Cardinality at 200 Nodes

| Metric | Series |
|--------|--------|
| node.phase.duration (histogram) | ~25 |
| plan.duration (histogram) | ~8 |
| node.phase (gauge) | ~1,000 |
| phase.transitions (counter) | ~20 |
| plan.failures (counter) | ~8 |
| reconcile.errors (counter) | ~6 |
| plan.active (gauge) | ~6 |
| deployment.phase (gauge) | ~50 |
| deployment.replicas (gauge) | ~20 |
| **Total** | **~1,143 series** |

~70% reduction from the previous design.

## Alerts (4)

| Alert | Signal | Expression |
|-------|--------|-----------|
| Node stuck | Latency | `node.phase{phase=Initializing}` held for >30min with no transition |
| Plan failure rate | Errors | `rate(plan.failures) / rate(phase.transitions)` > threshold |
| Sustained reconcile errors | Errors | controller-runtime `reconcile_total{result=error}` >25% over 10min |
| Controller saturated | Saturation | `plan.active` growing monotonically OR `workqueue_depth` growing |

## CRD Change

Add `PhaseTransitionTime` to `SeiNodeStatus`:
```go
// PhaseTransitionTime is when the node last changed phases.
// Used to compute phase duration metrics.
// +optional
PhaseTransitionTime *metav1.Time `json:"phaseTransitionTime,omitempty"`
```

## Implementation Phases

### Phase 1: OTel Infrastructure
- Add OTel SDK dependencies
- `initMeterProvider()` in `cmd/main.go`
- Attribute constants in `observability/`
- Downward API env vars
- No metric changes

### Phase 2: Core Metrics (the 7+2)
- Replace all existing metrics with the new set
- Add `PhaseTransitionTime` to CRD
- Implement phaseTracker for observable gauges
- Single PR, atomic swap

### Phase 3: Remove Prometheus Client
- Remove `prometheus/client_golang` direct dependency
- Only transitive through controller-runtime
