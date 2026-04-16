# OpenTelemetry Telemetry Strategy

## Overview

Migrate the controller from `prometheus/client_golang` to the OpenTelemetry metrics SDK so users can configure any observability backend. The telemetry is designed from the operator's perspective: what do I need measured to understand issues, and what dimensions do I care about?

## Architecture

Single `MeterProvider` with two readers:

```
                                    +---> OTLP PeriodicReader ---> user's collector
                                    |
OTel MeterProvider (with 2 readers) |
                                    |
                                    +---> Prometheus Exporter ---> controller-runtime /metrics
```

- **Prometheus exporter**: registers into controller-runtime's `metrics.Registry` via `promexporter.WithRegisterer(crmetrics.Registry)`. OTel metrics appear alongside controller-runtime's own workqueue/reconcile metrics on the existing `/metrics` endpoint. Zero scraping changes.
- **OTLP exporter**: enabled when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. Users point at their collector (Datadog, Grafana Cloud, etc.).

controller-runtime's Prometheus metrics (workqueue depth, reconcile duration, REST client) remain untouched.

## Configuration

Standard OTel environment variables — no custom flags:
- `OTEL_EXPORTER_OTLP_ENDPOINT` — collector address
- `OTEL_EXPORTER_OTLP_PROTOCOL` — grpc or http/protobuf
- `OTEL_SERVICE_NAME` — override (defaults to `sei-k8s-controller`)
- `OTEL_RESOURCE_ATTRIBUTES` — user-provided extras
- `OTEL_SDK_DISABLED` — kill switch

Kubernetes downward API provides pod identity:
```yaml
env:
  - name: POD_NAME
    valueFrom:
      fieldRef:
        fieldPath: metadata.name
  - name: POD_NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
  - name: NODE_NAME
    valueFrom:
      fieldRef:
        fieldPath: spec.nodeName
```

## Resource Attributes (Soft Context)

Set once on the MeterProvider. Apply to all metrics via OTLP. Invisible to Prometheus scrapers (Resource attributes are not emitted as labels on `/metrics`).

| Attribute | Source |
|-----------|--------|
| `service.name` | Hardcoded: `sei-k8s-controller` |
| `service.version` | Build-time ldflags |
| `service.instance.id` | Downward API: pod UID |
| `k8s.pod.name` | Downward API: `POD_NAME` |
| `k8s.namespace.name` | Downward API: `POD_NAMESPACE` |
| `k8s.node.name` | Downward API: `NODE_NAME` |

## Metric Naming

OTel uses dot-separated names. The Prometheus exporter auto-converts to underscores and appends `_total` / `_seconds` suffixes, so existing PromQL and dashboards continue working.

Meter name: `github.com/sei-protocol/sei-k8s-controller`

Per-datapoint attribute keys use short names matching current Prometheus labels exactly (`namespace`, `name`, `phase`, `controller`, `task_type`, etc.) to preserve dashboard compatibility.

## The Operational Questions

| Scenario | Metric(s) That Answer It |
|----------|--------------------------|
| Is my fleet healthy? | `seinode.phase` by chain_id and node_mode |
| Is a node stuck or just slow? | `plan.task.current` / `plan.task.count` + `task.submitted.at` |
| How's the rollout going? | `plan.active` with plan_type + `plan.completions` |
| What failed and where? | `task.failures` with name + task_type |
| How long do things normally take? | `task.duration` histogram + `seinode.init.duration` |
| Is the controller itself healthy? | controller-runtime builtins (reconcile rate, queue depth) |

## Metrics: Cut (4)

| Current Metric | Reason |
|---------------|--------|
| `seinode.phase_transitions_total` | Combinatorial from/to matrix. Replaced by `plan.completions` with outcome dimension |
| `seinode.last_init_duration_seconds` | Per-node gauge that persists forever. The init histogram covers aggregate analysis; task duration covers per-operation debugging |
| `seinodedeployment.reconcile_substep_duration` | Internal implementation detail. controller-runtime's total reconcile latency covers alerting |
| `reconcile_errors_total` (per-name) | Unbounded cardinality. controller-runtime's `reconcile_total{result=error}` covers the alert case |

## Metrics: Keep (8, with dimension additions)

### Fleet Health

**1. `sei.controller.seinode.phase`** — Observable Float64 Gauge
- Dimensions: `namespace`, `name`, `chain_id`, `node_mode`, `phase`
- 0/1 per phase (kube-state-metrics pattern). Observable gauge with phaseTracker callback — cleanup on deletion is automatic (stop reporting, series disappears)
- *Added*: `chain_id`, `node_mode`

**2. `sei.controller.seinodedeployment.phase`** — Observable Float64 Gauge
- Dimensions: `namespace`, `name`, `phase`
- Same 0/1 pattern

**3. `sei.controller.seinodedeployment.replicas`** — Float64 Gauge
- Dimensions: `namespace`, `name`, `type` (desired/ready)

**4. `sei.controller.seinodedeployment.condition`** — Observable Float64 Gauge
- Dimensions: `namespace`, `name`, `type`, `status`
- Same 0/1 pattern

### Initialization Performance

**5. `sei.controller.seinode.init.duration`** — Float64 Histogram (unit: s)
- Dimensions: `namespace`, `chain_id`, `node_mode`, `bootstrap_mode`
- Buckets: 10, 30, 60, 120, 300, 600, 900, 1200, 1800, 3600
- Recorded when a node reaches Running
- *Added*: `node_mode`, `bootstrap_mode` (snapshot / state-sync / genesis)

### Plan Execution

**6. `sei.controller.plan.active`** — Float64 Gauge
- Dimensions: `controller`, `namespace`, `name`, `plan_type`
- 1 when a plan is in progress, 0 otherwise
- *Added*: `plan_type` (init / node-update / genesis / deployment)

**7. `sei.controller.task.failures`** — Int64 Counter
- Dimensions: `controller`, `namespace`, `name`, `task_type`
- *Added*: `name` (resource name)

**8. `sei.controller.task.retries`** — Int64 Counter
- Dimensions: `controller`, `namespace`, `name`, `task_type`
- *Added*: `name`

## Metrics: Add (7)

### Task-Level Diagnostics

**9. `sei.controller.task.duration`** — Float64 Histogram (unit: s)
- Dimensions: `controller`, `namespace`, `task_type`
- Recorded when a task completes: `time.Since(submittedAt)`
- Buckets: 1, 5, 10, 30, 60, 120, 300, 600, 1800
- Highest-value missing metric. Answers: "is snapshot-restore slow or is config-apply slow?"

### Plan Progress

**10. `sei.controller.plan.task.current`** — Int64 Gauge
- Dimensions: `controller`, `namespace`, `name`
- Zero-indexed position of the current task in the plan
- Combined with task.count, gives a progress bar

**11. `sei.controller.plan.task.count`** — Int64 Gauge
- Dimensions: `controller`, `namespace`, `name`
- Total tasks in the active plan. 0 when no plan active.

**12. `sei.controller.task.submitted.at`** — Float64 Gauge (unix epoch)
- Dimensions: `controller`, `namespace`, `name`
- When the current task was submitted. Detect stuck tasks: `time() - submitted_at > threshold`

### Plan Outcomes

**13. `sei.controller.plan.completions`** — Int64 Counter
- Dimensions: `controller`, `namespace`, `plan_type`, `outcome` (success/failure)
- Compute success rate: `rate(completions{outcome=success}) / rate(completions)`

### Controller Identity

**14. `sei.controller.info`** — Float64 Gauge (always 1)
- Dimensions: `version`, `cluster`, `environment`
- One time series. Correlate version with behavior changes.

## Per-Metric Attribute Constants

Defined in `internal/controller/observability/`:

```go
var (
    AttrController    = attribute.Key("controller")
    AttrNamespace     = attribute.Key("namespace")
    AttrName          = attribute.Key("name")
    AttrPhase         = attribute.Key("phase")
    AttrChainID       = attribute.Key("chain_id")
    AttrNodeMode      = attribute.Key("node_mode")
    AttrBootstrapMode = attribute.Key("bootstrap_mode")
    AttrPlanType      = attribute.Key("plan_type")
    AttrTaskType      = attribute.Key("task_type")
    AttrOutcome       = attribute.Key("outcome")
    AttrReplicaType   = attribute.Key("type")
    AttrCondType      = attribute.Key("type")
    AttrCondStatus    = attribute.Key("status")
)
```

## Alerts (4)

| Alert | Expression | Rationale |
|-------|-----------|-----------|
| Node stuck | Non-Running phase >30min AND `task.submitted.at` unchanged | Distinguishes "slow but progressing" from "stuck" |
| Plan retrying without progress | Same task retried N times, no task index advance | Catches infinite retry loops |
| Sustained reconcile error rate | controller-runtime `reconcile_total{result=error}` >25% over 10min | Controller is broken (RBAC, CRDs missing) |
| Queue depth growing | Monotonic increase over 5min | Controller is overwhelmed |

## Cardinality Analysis

| Metric | Max Series (50 nodes, 3 chains) |
|--------|------|
| seinode.phase | 50 nodes × 5 phases = 250 |
| task.failures | 50 nodes × ~10 active task types = 500 |
| plan.active | 50 nodes = 50 |
| plan.task.current | 50 nodes = 50 |
| task.duration | ~10 task types × 11 buckets = 110 |
| **Total** | **< 2,000 active series** |

## Implementation Phases

### Phase 1: OTel Infrastructure
- Add OTel SDK dependencies
- `initMeterProvider()` in `cmd/main.go` with Prometheus + OTLP readers
- Attribute constants in `internal/controller/observability/`
- Downward API env vars in Deployment spec
- No metric changes — validates the bridge works

### Phase 2: Migrate Planner Metrics (3 metrics)
- Convert `task.retries`, `task.failures`, `plan.active`
- Add `name` and `plan_type` dimensions
- Add new `task.duration` histogram
- Add new `plan.completions` counter

### Phase 3: Migrate Node Metrics (4 → 5 metrics)
- Convert `seinode.phase`, `seinode.init.duration`
- Introduce `phaseTracker` for observable gauge
- Add `chain_id`, `node_mode`, `bootstrap_mode` dimensions
- Add new plan progress metrics (`task.current`, `task.count`, `submitted.at`)

### Phase 4: Migrate Deployment Metrics (4 metrics)
- Convert phase, replicas, condition gauges
- Add `controller.info` gauge

### Phase 5: Remove Prometheus Client
- Remove cut metrics
- Remove `prometheus/client_golang` direct dependency
- Remove `init()` registrations
- Only transitive Prometheus dependency through controller-runtime remains

Each phase is independently deployable. Parallel operation during migration is safe.
