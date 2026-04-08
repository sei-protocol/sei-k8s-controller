# Component: Controller & Sidecar Observability

## Owner
Platform Engineering (controller) / Sidecar team (seictl)

## Phase
Phase 1: Metrics registration, events, annotation-based scraping, controller self-monitoring
Phase 2: Grafana dashboards, sidecar metrics, kube-prometheus-stack migration, alerting
Phase 3 (deferred): Distributed tracing, cost attribution

## Purpose
Make the sei-k8s-controller and its seictl sidecar operationally ergonomic by wiring
Prometheus metrics, Kubernetes events, structured logging, self-monitoring, dashboards,
and alerting throughout the system. Today the controller emits zero custom metrics, only
the SeiNodeDeployment reconciler produces Kubernetes events, the sidecar has no `/metrics`
endpoint, and no dashboards or alert rules exist. Operators have no signal beyond
`kubectl logs` and manual status inspection to understand fleet health.

## Dependencies

### External systems consumed
- **Prometheus** (community chart, annotation-based scraping) — TSDB for metrics storage and PromQL evaluation
- **Grafana** — Dashboard rendering
- **Loki** — Log aggregation (structured JSON logs from all pods via Promtail)
- **controller-runtime metrics** — built-in workqueue, reconcile duration, REST client metrics

### Phase 2 dependencies (not yet present on dev)
- **Prometheus Operator** — ServiceMonitor/PodMonitor CRD reconciliation (requires migration from community chart to kube-prometheus-stack)
- **Alertmanager** — Alert routing (currently disabled on dev; `alertmanager.enabled: false`)
- **Grafana sidecar** — Dashboard auto-discovery from ConfigMaps (currently disabled)

### Internal Tide components consumed
- **SeiNodeDeployment controller** — already has EventRecorder; metrics hook points in reconcile loop
- **SeiNode controller** — needs EventRecorder; metrics hook points in reconcile/plan execution
- **SeiNodePool controller** — needs EventRecorder; metrics hook points in genesis lifecycle
- **seictl sidecar** — task execution engine; needs `/metrics` HTTP handler (Phase 2)

### Explicit exclusions
- Does NOT depend on any tracing backend (Jaeger, Tempo) — deferred to Phase 3
- Does NOT depend on external APM tools (Datadog, New Relic)
- Does NOT modify CRD spec fields — observability is orthogonal to user-facing API
- Does NOT depend on Prometheus Operator in Phase 1 (annotation-based scraping only)

---

## Interface Specification

### 1. Controller Prometheus Metrics

All metrics use the `sei_controller_` prefix. Labels follow controller-runtime conventions.

#### Reconciliation Metrics

```go
// Gauges — per-resource state (cleaned up on deletion via §1e)
sei_controller_seinode_phase{namespace, name, phase}                   gauge   // 1.0 for current phase, 0.0 for others
sei_controller_seinodedeployment_phase{namespace, name, phase}              gauge   // same pattern
sei_controller_seinodepool_phase{namespace, name, phase}               gauge   // same pattern
sei_controller_seinodedeployment_replicas{namespace, name, type}            gauge   // type=desired|ready|initializing|failed
sei_controller_seinodepool_replicas{namespace, name, type}             gauge   // type=total|ready
sei_controller_seinodedeployment_condition{namespace, name, type, status}   gauge   // 1.0 when condition matches

// Counters
sei_controller_reconcile_errors_total{controller, namespace, name}     counter // reconcile errors beyond what controller-runtime tracks
sei_controller_seinode_phase_transitions_total{namespace, name, from, to}  counter // phase state machine transitions

// Histograms — aggregate across resources for meaningful percentiles
sei_controller_seinode_init_duration_seconds{namespace, chain_id}          histogram // time from Pending to Running (no `name` label — aggregates across all nodes for a chain)
sei_controller_seinode_last_init_duration_seconds{namespace, name}         gauge     // per-node init duration for debugging (set once when node reaches Running)
sei_controller_seinodedeployment_reconcile_substep_duration_seconds{controller, substep}  histogram // per-substep timing
```

**Histogram bucket selection:** Custom buckets for all histograms to cover the full range of
controller operations, including slow API server writes and sidecar interactions:

```go
var reconcileBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
```

This extends `prometheus.DefBuckets` (max 10s) to 60s, covering controller-runtime's default
reconcile timeout and slow network conditions.

#### Sidecar Interaction Metrics

```go
sei_controller_sidecar_request_duration_seconds{namespace, node, method, route, status_code}  histogram
sei_controller_sidecar_task_submit_total{namespace, node, task_type}         counter
sei_controller_sidecar_task_complete_total{namespace, node, task_type, result}  counter  // result=success|failure
sei_controller_sidecar_unreachable_total{namespace, node}                   counter
```

**Route normalization:** The `route` label uses fixed route templates, not raw paths.
Bounded allowlist of known routes:

```go
var knownRoutes = map[string]string{
    "/v0/tasks":     "/v0/tasks",
    "/v0/healthz":   "/v0/healthz",
    "/v0/status":    "/v0/status",
}
// Any path starting with "/v0/tasks/" maps to "/v0/tasks/:id"
// Any unrecognized path maps to "other"
```

#### Networking Resource Metrics

```go
sei_controller_networking_resource_reconcile_total{namespace, group, resource_type, result}  counter  // resource_type=Service|HTTPRoute|AuthorizationPolicy, result=created|updated|unchanged|error
sei_controller_networking_crd_available{crd}                                                 gauge    // 1.0 if CRD is installed, 0.0 if not
```

**Registration:** All metrics registered in a `metrics.go` file per controller package using
`prometheus.NewGaugeVec` / `NewCounterVec` / `NewHistogramVec` from `github.com/prometheus/client_golang`.
Registered with controller-runtime's default registry (`metrics.Registry`).

**Cardinality budget:** Phase labels are bounded enums (6 phases for SeiNode, 6 for SeiNodeDeployment,
4 for SeiNodePool). `namespace` × `name` cardinality is bounded by cluster size (target: <100
SeiNodes, <20 SeiNodeDeployments, <10 SeiNodePools). `from` × `to` transition pairs are bounded by
the state machine (max 30 combinations). `route` is bounded by the allowlist (4 values + "other").

The 0/1-per-phase gauge pattern follows the kube-state-metrics convention (e.g., `kube_pod_status_phase`)
where each enum value is a separate time series with value 0 or 1. This enables simple `== 1`
queries and alerting without cardinality issues from multi-valued labels.

### 2. Kubernetes Events

#### SeiNode Events (new — EventRecorder to be added)

| Reason | Type | When |
|--------|------|------|
| `PhaseTransition` | Normal | Phase changes (e.g. Initializing → Running) |
| `PreInitStarted` | Normal | PreInit Job created |
| `PreInitComplete` | Normal | PreInit Job succeeded |
| `PreInitFailed` | Warning | PreInit Job failed |
| `InitPlanStarted` | Normal | Init task sequence begins |
| `InitPlanComplete` | Normal | All init tasks done, node is ready |
| `InitTaskFailed` | Warning | Individual init task failed |
| `SidecarUnreachable` | Warning | Sidecar HTTP calls fail (debounced: once per 5 min) |
| `ConfigDriftDetected` | Warning | ConfigStatus.DriftDetected transitions to true |
| `SnapshotScheduled` | Normal | Snapshot generation task submitted |
| `SnapshotComplete` | Normal | Snapshot upload complete |
| `SnapshotFailed` | Warning | Snapshot task failed |

**RBAC requirement:** Add the following kubebuilder marker to `SeiNodeReconciler`:
```go
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
```
Then run `make manifests` to regenerate the ClusterRole.

#### SeiNodePool Events (new — EventRecorder to be added)

| Reason | Type | When |
|--------|------|------|
| `GenesisStarted` | Normal | Genesis preparation Job created |
| `GenesisComplete` | Normal | Genesis Job succeeded, nodes being created |
| `GenesisFailed` | Warning | Genesis Job failed |
| `NodeCreated` | Normal | Child SeiNode created |
| `NodeDeleted` | Normal | Child SeiNode deleted |
| `PoolReady` | Normal | All nodes in Running phase |

**RBAC requirement:** Add the following kubebuilder marker to `SeiNodePoolReconciler`:
```go
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
```
Then run `make manifests` to regenerate the ClusterRole.

#### SeiNodeDeployment Events (existing, extended)

Add to existing event set:

| Reason | Type | When |
|--------|------|------|
| `GroupReady` | Normal | Phase transitions to Ready (all nodes Running) |
| `GroupDegraded` | Warning | Phase transitions to Degraded (some nodes not Running) |
| `ScaleUp` | Normal | Replicas increased, new SeiNodes being created |
| `ScaleDown` | Normal | Replicas decreased, excess SeiNodes being deleted |

### 3. Controller Self-Monitoring (Phase 1 — Annotation-Based)

The dev cluster runs the community Prometheus chart with annotation-based pod discovery, **not**
Prometheus Operator. Phase 1 uses annotations for scraping; Phase 2 migrates to ServiceMonitor.

#### Phase 1: Annotation-based scraping

Switch the controller's metrics endpoint to HTTP on a non-public port for in-cluster scraping.
This avoids the complexity of bearer token auth and TLS for annotation-based discovery:

```go
// cmd/main.go — Phase 1 change
flag.BoolVar(&secureMetrics, "metrics-secure", false, // changed from true
    "If set, the metrics endpoint is served securely via HTTPS.")
```

The metrics bind address stays at `:8443`. Add annotations to the controller Deployment pod template:

```yaml
# config/manager/manager.yaml — pod template metadata
template:
  metadata:
    annotations:
      kubectl.kubernetes.io/default-container: manager
      prometheus.io/scrape: "true"
      prometheus.io/port: "8443"
      prometheus.io/path: "/metrics"
```

Also declare the metrics port in the container spec (currently `ports: []`):

```yaml
ports:
  - name: metrics
    containerPort: 8443
    protocol: TCP
```

The existing `extraScrapeConfigs` in the Prometheus Helm values discovers pods with
`prometheus.io/scrape: "true"` and honors the `port` and `path` annotations.

#### Phase 2: ServiceMonitor-based scraping (after kube-prometheus-stack migration)

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: sei-k8s-controller-manager
  namespace: sei-k8s-controller-system
  labels:
    control-plane: controller-manager
spec:
  namespaceSelector:
    matchNames:
      - sei-k8s-controller-system
  selector:
    matchLabels:
      control-plane: controller-manager
  endpoints:
    - port: https
      scheme: https
      tlsConfig:
        insecureSkipVerify: true
      authorization:
        type: Bearer
        credentials:
          name: prometheus-sa-token
          key: token
      interval: 30s
```

Note: Uses `authorization` block instead of deprecated `bearerTokenFile` (deprecated in
Prometheus Operator v0.59+).

**RBAC prerequisite for Phase 2:** When re-enabling secure metrics, deploy:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: sei-controller-metrics-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: sei-controller-metrics-reader   # already generated by controller-gen
subjects:
  - kind: ServiceAccount
    name: prometheus-server              # verify actual SA name on target cluster
    namespace: monitoring
```

### 4. Sidecar Metrics Endpoint (seictl — Phase 2)

The seictl sidecar exposes a Prometheus-compatible `/metrics` endpoint on its existing HTTP server port (7777).

```go
// Sidecar metrics — sei_sidecar_ prefix
sei_sidecar_task_execution_duration_seconds{task_type}            histogram  // custom buckets: reconcileBuckets
sei_sidecar_task_executions_total{task_type, result}              counter    // result=success|failure
sei_sidecar_active_tasks                                          gauge
sei_sidecar_config_applies_total{result}                          counter
sei_sidecar_config_generation                                     gauge      // monotonically increasing integer
sei_sidecar_health_checks_total{result}                           counter    // result=ok|degraded
sei_sidecar_process_start_time_seconds                            gauge      // for uptime calculation
```

Note: `sei_sidecar_config_generation` (renamed from `config_version`) is a monotonically increasing
integer representing the config schema generation. If a string identifier is needed later, use an
info-style metric: `sei_sidecar_config_info{version="..."}` with constant value 1.

#### Sidecar scraping — Phase 1 (annotation-based)

Add annotations to the sidecar container's pod template in `generateNodeStatefulSet`:

```go
// internal/controller/node/resources.go — StatefulSet pod template annotations
podAnnotations := map[string]string{
    "prometheus.io/scrape": "true",
    "prometheus.io/port":   "7777",
    "prometheus.io/path":   "/metrics",
}
```

This works immediately with the existing annotation-based Prometheus discovery.

#### Sidecar scraping — Phase 2 (PodMonitor after kube-prometheus-stack migration)

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: sei-sidecar
  namespace: monitoring
spec:
  namespaceSelector:
    matchNames: []     # empty = all namespaces
  selector:
    matchLabels:
      sei.io/node: ""  # label present on all SeiNode StatefulSet pods
  podMetricsEndpoints:
    - port: seictl
      path: /metrics
      interval: 30s
```

Verify that `sei.io/node` label is set on StatefulSet pod templates by checking
`generateNodeStatefulSet` output in `resources.go`.

**Network policy consideration:** SeiNodePool-generated NetworkPolicies may restrict ingress
to node pods. When deploying the PodMonitor, ensure the pool NetworkPolicy includes an ingress
rule allowing traffic from the `monitoring` namespace on port 7777, or deploy a supplementary
NetworkPolicy:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-prometheus-sidecar-scrape
spec:
  podSelector:
    matchLabels:
      sei.io/node: ""
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: monitoring
      ports:
        - port: 7777
          protocol: TCP
```

### 5. Structured Logging Standards

All controllers and the sidecar follow these conventions:

```
Logger hierarchy:
  controller.seinode          — SeiNode reconciler
  controller.seinodedeployment     — SeiNodeDeployment reconciler
  controller.seinodepool      — SeiNodePool reconciler
  sidecar.engine              — seictl task engine
  sidecar.task.<type>         — per-task-type logger

Required fields on every reconcile log line:
  "namespace"    string
  "name"         string
  "reconcileID"  string   (from context, controller-runtime provides this)

Required fields on phase transitions:
  "fromPhase"    string
  "toPhase"      string

Required fields on sidecar interactions:
  "taskType"     string
  "taskID"       string   (when known)
  "duration"     string   (for completed calls)
```

Eliminate all `log.Log.Info(...)` (global logger) usage in favor of `log.FromContext(ctx)`.

Promtail is deployed with default scrape configs (collects all pod logs from all namespaces).
Loki has `allow_structured_metadata: true`. JSON structured logs from the controller and sidecar
will be ingested automatically via Promtail without additional configuration.

### 6. Grafana Dashboards

#### Phase 1: Manual provisioning

Since the Grafana sidecar for ConfigMap auto-discovery is not enabled on dev, Phase 1 uses
Grafana's built-in `dashboardProviders` and `dashboardsConfigMaps` Helm values.

Dashboard JSON files are committed to the controller repo at `config/monitoring/dashboards/`
for version control alongside the code.

Deployment requires a platform repo change to the Grafana Helm values:

```yaml
# platform/clusters/dev/monitoring/grafana.yaml — add to values:
dashboardProviders:
  dashboardproviders.yaml:
    apiVersion: 1
    providers:
      - name: sei-controller
        orgId: 1
        folder: Sei Controller
        type: file
        disableDeletion: false
        editable: true
        options:
          path: /var/lib/grafana/dashboards/sei-controller

dashboardsConfigMaps:
  sei-controller: sei-controller-dashboards
```

A ConfigMap named `sei-controller-dashboards` is deployed to the `monitoring` namespace
(NOT bundled with the controller — see Deployment section).

#### Phase 2: Sidecar auto-discovery (after Grafana sidecar enablement)

Enable in platform repo:
```yaml
sidecar:
  dashboards:
    enabled: true
    label: grafana_dashboard
    labelValue: "1"
    searchNamespace: ALL
```

Then dashboards can be deployed as labeled ConfigMaps to any namespace.

#### Dashboard 1: Fleet Overview

Panels:
- SeiNodeDeployment count by phase (pie/stat)
- SeiNode count by phase (pie/stat)
- SeiNodePool count by phase (pie/stat)
- Replicas desired vs ready per group (timeseries)
- Reconciliation rate and error rate (timeseries)
- SeiNode initialization duration P50/P95/P99 (timeseries)
- Networking resource status (table: group, HTTPRoute status, Service LB IP, AuthorizationPolicy)
- Recent Kubernetes events (logs panel via Loki, filtered to sei.io resources)

#### Dashboard 2: Controller Health

Panels:
- Reconcile duration by controller (P50/P95/P99 heatmap, from controller-runtime metrics)
- Work queue depth and processing time (timeseries)
- Sidecar request latency by node (heatmap)
- Sidecar unreachable rate (timeseries with threshold annotation)
- Controller memory and CPU (from cAdvisor)
- Active reconciles / goroutines
- REST client request rate and errors (from controller-runtime client metrics)

### 7. Alerting

#### Phase 1: No alerting infrastructure

Alertmanager is explicitly disabled on dev (`alertmanager.enabled: false`). Phase 1 focuses
on metrics and dashboards. Operators use Grafana dashboards for visual monitoring.

#### Phase 2: PrometheusRule (after kube-prometheus-stack migration + Alertmanager enablement)

**Prerequisites:**
1. Migrate monitoring stack to kube-prometheus-stack (enables Prometheus Operator)
2. Enable Alertmanager with a receiver (Slack, PagerDuty, etc.)

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: sei-controller-alerts
spec:
  groups:
    - name: sei-controller
      rules:
        - alert: SeiNodeDeploymentDegraded
          expr: sei_controller_seinodedeployment_phase{phase="Degraded"} == 1
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "SeiNodeDeployment {{ $labels.namespace }}/{{ $labels.name }} is degraded"

        - alert: SeiNodeDeploymentFailed
          expr: sei_controller_seinodedeployment_phase{phase="Failed"} == 1
          for: 5m
          labels:
            severity: critical
          annotations:
            summary: "SeiNodeDeployment {{ $labels.namespace }}/{{ $labels.name }} has failed"

        - alert: SeiNodeStuckInitializing
          expr: sei_controller_seinode_phase{phase="Initializing"} == 1
          for: 30m
          labels:
            severity: warning
          annotations:
            summary: "SeiNode {{ $labels.namespace }}/{{ $labels.name }} stuck initializing for 30m"

        - alert: SeiNodeStuckPending
          expr: sei_controller_seinode_phase{phase="Pending"} == 1
          for: 15m
          labels:
            severity: warning
          annotations:
            summary: "SeiNode {{ $labels.namespace }}/{{ $labels.name }} stuck pending for 15m"

        - alert: SeiNodePoolStuckPending
          expr: sei_controller_seinodepool_phase{phase="Pending"} == 1
          for: 15m
          labels:
            severity: warning
          annotations:
            summary: "SeiNodePool {{ $labels.namespace }}/{{ $labels.name }} stuck pending for 15m"

        - alert: SidecarUnreachableHigh
          expr: rate(sei_controller_sidecar_unreachable_total[5m]) > 0.1
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "Sidecar for {{ $labels.namespace }}/{{ $labels.node }} unreachable"

        - alert: ControllerReconcileErrors
          expr: rate(sei_controller_reconcile_errors_total[5m]) > 0
          for: 15m
          labels:
            severity: warning
          annotations:
            summary: "Controller {{ $labels.controller }} has sustained reconcile errors"

        - alert: ControllerHighReconcileLatency
          expr: histogram_quantile(0.99, rate(controller_runtime_reconcile_duration_seconds_bucket[5m])) > 30
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "Controller reconcile P99 latency exceeds 30s"
```

---

## State Model

### Metrics state
- **Location:** In-process Prometheus registry (controller-runtime `metrics.Registry`)
- **Lifecycle:** Created at controller startup, updated during reconcile loops, cleaned up on resource deletion (§1e), scraped by Prometheus
- **Source of truth:** Live process memory; Prometheus TSDB is the persistent store
- **Staleness:** If gauge cleanup is missed (crash between delete and cleanup), Prometheus staleness mechanism (default 5m) handles stale series as a fallback

### Event state
- **Location:** Kubernetes API server (Events)
- **Lifecycle:** Created by EventRecorder, garbage-collected by K8s (default 1h TTL)
- **Source of truth:** etcd via API server

### Dashboard/Alert state
- **Location:** ConfigMaps (dashboards), PrometheusRule CRDs (alerts, Phase 2)
- **Lifecycle:** Deployed via Kustomize/Helm, managed by Flux GitOps
- **Source of truth:** Git repository

---

## Internal Design

### Phase 1 Implementation (controller metrics + events + annotation-based scraping)

#### 1a. Metrics registration pattern

Each controller package gets a `metrics.go` file:

```go
// internal/controller/nodedeployment/metrics.go
package nodedeployment

import (
    "github.com/prometheus/client_golang/prometheus"
    "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var reconcileBuckets = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

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

    reconcileSubstepDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "sei_controller_seinodedeployment_reconcile_substep_duration_seconds",
            Help:    "Duration of individual reconcile substeps",
            Buckets: reconcileBuckets,
        },
        []string{"controller", "substep"},
    )
)

func init() {
    metrics.Registry.MustRegister(
        groupPhaseGauge,
        groupReplicasGauge,
        reconcileSubstepDuration,
    )
}
```

#### 1b. Metrics emission in reconcile loop

```go
func (r *SeiNodeDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // ... existing logic ...

    // After status update, emit phase gauge
    emitPhaseGauge(groupPhaseGauge, group.Namespace, group.Name, string(group.Status.Phase),
        allGroupPhases)

    // Emit replica gauges
    groupReplicasGauge.WithLabelValues(group.Namespace, group.Name, "desired").Set(float64(group.Spec.Replicas))
    groupReplicasGauge.WithLabelValues(group.Namespace, group.Name, "ready").Set(float64(group.Status.ReadyReplicas))

    return ctrl.Result{}, nil
}

// emitPhaseGauge sets 1.0 for the current phase and 0.0 for all others,
// giving Prometheus a clean gauge that works with instant queries.
func emitPhaseGauge(gauge *prometheus.GaugeVec, ns, name, current string, allPhases []string) {
    for _, p := range allPhases {
        val := 0.0
        if p == current {
            val = 1.0
        }
        gauge.WithLabelValues(ns, name, p).Set(val)
    }
}
```

#### 1c. Substep timing helper

```go
func (r *SeiNodeDeploymentReconciler) timeSubstep(ctx context.Context, name string, fn func() error) error {
    start := time.Now()
    err := fn()
    reconcileSubstepDuration.WithLabelValues("seinodedeployment", name).Observe(time.Since(start).Seconds())
    return err
}

// Usage in reconcile:
if err := r.timeSubstep(ctx, "ensureNodes", func() error {
    return r.ensureNodes(ctx, group)
}); err != nil {
    return ctrl.Result{}, err
}
```

#### 1d. EventRecorder for SeiNode and SeiNodePool controllers

```go
// cmd/main.go — add recorders
nodeRecorder := mgr.GetEventRecorderFor("seinode-controller")
if err := (&nodecontroller.SeiNodeReconciler{
    Client:   mgr.GetClient(),
    Scheme:   mgr.GetScheme(),
    Platform: platform,
    Recorder: nodeRecorder,
}).SetupWithManager(mgr); err != nil { ... }

poolRecorder := mgr.GetEventRecorderFor("seinodepool-controller")
if err := (&nodepoolcontroller.SeiNodePoolReconciler{
    Client:   mgr.GetClient(),
    Scheme:   mgr.GetScheme(),
    Recorder: poolRecorder,
}).SetupWithManager(mgr); err != nil { ... }
```

Emit events at phase transitions:

```go
func (r *SeiNodeReconciler) transitionPhase(ctx context.Context, node *SeiNode, to SeiNodePhase) {
    from := node.Status.Phase
    if from == to {
        return
    }

    // Metric
    nodePhaseTransitions.WithLabelValues(node.Namespace, node.Name, string(from), string(to)).Inc()

    // Event
    if r.Recorder != nil {
        r.Recorder.Eventf(node, corev1.EventTypeNormal, "PhaseTransition",
            "Phase changed from %s to %s", from, to)
    }

    // Log
    log.FromContext(ctx).Info("phase transition", "fromPhase", from, "toPhase", to)

    node.Status.Phase = to
}
```

**RBAC markers:** Both `SeiNodeReconciler` and `SeiNodePoolReconciler` must add:
```go
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
```
Then run `make manifests` to regenerate the ClusterRole. Without these markers, `Recorder.Eventf()`
calls will silently fail with 403 errors (the event recorder swallows RBAC errors by default).

#### 1e. Gauge cleanup on resource deletion

Each controller implements a `cleanupMetrics` helper called at the beginning of `handleDeletion`,
before any status patch:

```go
// internal/controller/nodedeployment/metrics.go

func cleanupGroupMetrics(namespace, name string) {
    for _, phase := range allGroupPhases {
        groupPhaseGauge.DeleteLabelValues(namespace, name, phase)
    }
    for _, typ := range []string{"desired", "ready", "initializing", "failed"} {
        groupReplicasGauge.DeleteLabelValues(namespace, name, typ)
    }
    for _, cond := range allConditionTypes {
        for _, status := range []string{"True", "False", "Unknown"} {
            groupConditionGauge.DeleteLabelValues(namespace, name, cond, status)
        }
    }
}
```

```go
// internal/controller/nodedeployment/controller.go — in handleDeletion:

func (r *SeiNodeDeploymentReconciler) handleDeletion(ctx context.Context, group *SeiNodeDeployment) (ctrl.Result, error) {
    cleanupGroupMetrics(group.Namespace, group.Name)
    // ... existing finalizer logic ...
}
```

Same pattern for SeiNode (`cleanupNodeMetrics`) and SeiNodePool (`cleanupPoolMetrics`).

For counters and histograms with per-resource labels (e.g., `sidecar_unreachable_total`),
`DeleteLabelValues` is semantically acceptable because the resource identity is gone — the
counter for that label set will never be incremented again.

**Fallback:** If the controller crashes between resource deletion and gauge cleanup, Prometheus'
built-in staleness mechanism (default 5m) marks the series as stale. No phantom resources will
persist beyond that window.

### Phase 2 Implementation (dashboards, alerts, sidecar metrics, kube-prometheus-stack)

#### 2a. kube-prometheus-stack migration (prerequisite)

Replace the community Prometheus chart with `kube-prometheus-stack` in the platform repo.
This enables:
- Prometheus Operator (reconciles ServiceMonitor, PodMonitor, PrometheusRule CRDs)
- Alertmanager (alert routing)
- Grafana sidecar (dashboard auto-discovery)
- Built-in kube-state-metrics (replaces standalone deployment)

This is a platform repo change owned by the platform team. Tracked separately.

#### 2b. Dashboard deployment

**Phase 1 path:** Dashboard JSON committed to `config/monitoring/dashboards/` in the controller
repo. A ConfigMap `sei-controller-dashboards` is deployed to the `monitoring` namespace via
a **separate** Kustomization in the platform repo (NOT bundled with the controller's
`config/default`, which has `namespace: sei-k8s-controller-system` applied).

```yaml
# platform/clusters/dev/monitoring/kustomization.yaml — add:
resources:
  # ... existing resources ...
  - github.com/sei-protocol/sei-k8s-controller/config/monitoring/dashboards?ref=main
```

The dashboards kustomization sets `namespace: monitoring` explicitly.

**Phase 2 path:** After Grafana sidecar enablement, dashboards can be deployed as labeled
ConfigMaps to any namespace with `searchNamespace: ALL`.

#### 2c. Sidecar `/metrics` endpoint
In the seictl repo, register a `promhttp.Handler()` on the existing HTTP mux at `/metrics`.
Register sidecar-specific metrics with the default registry. The port (7777) is already
exposed by the per-node headless Service. Phase 1 annotation-based scraping picks it up
immediately; Phase 2 PodMonitor provides explicit scrape configuration.

#### 2d. PrometheusRule
Deployed after kube-prometheus-stack migration enables Prometheus Operator.
Committed to `config/monitoring/prometheus-rule.yaml` in the controller repo.

---

## Error Handling

| Error | Detection | Surface | Operator Action |
|-------|-----------|---------|----------------|
| Metrics registration collision | `MustRegister` panics at startup | Controller crash, pod restart | Fix duplicate metric name in code |
| Prometheus scrape failure (auth) | `up == 0` for controller target | Grafana dashboard gap | Check pod annotations, Prometheus config, RBAC |
| PrometheusRule rejected (Phase 2) | Apply error | Log + condition | Fix rule syntax |
| Dashboard ConfigMap too large | Apply error (etcd 1.5MB limit) | Log | Split dashboard or compress |
| Cardinality explosion | Prometheus OOM or slow queries | Grafana timeout | Review label cardinality, add metric limits |
| Sidecar /metrics unreachable | Prometheus scrape failure | `up == 0` for sidecar target | Check sidecar health, pod networking, NetworkPolicy |
| Event RBAC missing | Silent 403 (recorder swallows errors) | Events not appearing for SeiNode/Pool | Add RBAC markers, run `make manifests` |

---

## Test Specification

### Unit Tests

| Test | Setup | Action | Expected |
|------|-------|--------|----------|
| `TestPhaseGaugeEmission` | Create SeiNodeDeployment, set phase to Ready | Call `emitPhaseGauge` | Ready=1.0, all others=0.0 |
| `TestReplicaGaugeValues` | Group with 3 replicas, 2 ready | Reconcile | desired=3, ready=2 |
| `TestSubstepTimingRecorded` | Mock substep taking 100ms | Call `timeSubstep` | Histogram has observation ≥ 0.1s, bucket 0.25 incremented |
| `TestPhaseTransitionCounter` | SeiNode transitions Pending→Initializing | Call `transitionPhase` | Counter incremented with from=Pending, to=Initializing |
| `TestPhaseTransitionEvent` | Same as above | Call `transitionPhase` | EventRecorder received Normal/PhaseTransition event |
| `TestSidecarUnreachableCounter` | Mock HTTP client returning connection error | Reconcile SeiNode | `sidecar_unreachable_total` incremented |
| `TestNetworkingResourceMetrics` | Reconcile HTTPRoute successfully | Check counter | `networking_resource_reconcile_total{result=created}` = 1 |
| `TestMetricsCleanupOnDeletion` | Delete SeiNodeDeployment | Call `cleanupGroupMetrics` then check gauges | All gauges for deleted group removed via `DeleteLabelValues` |
| `TestRouteNormalization` | HTTP call to `/v0/tasks/abc-123` | Check histogram label | `route` = `/v0/tasks/:id` |
| `TestInitDurationHistogramAggregates` | 3 SeiNodes in same chain complete init | Check histogram | 3 observations in `sei_controller_seinode_init_duration_seconds{chain_id="pacific-1"}` |
| `TestPoolPhaseGauge` | SeiNodePool in Running phase | Call `emitPhaseGauge` | Running=1.0, all others=0.0 |

### Integration Tests

| Test | Setup | Action | Expected |
|------|-------|--------|----------|
| `TestControllerMetricsScrape` | Deploy controller with annotations | `curl http://<pod>:8443/metrics` | Returns Prometheus text format with `sei_controller_` metrics |
| `TestSidecarMetricsScrape` | Deploy SeiNode with sidecar (Phase 2) | `curl http://<pod>:7777/metrics` | Returns `sei_sidecar_` metrics |
| `TestDashboardConfigMapLoads` | Apply dashboard ConfigMap, add to Grafana values | Check Grafana API | Dashboard appears in Grafana |
| `TestPrometheusDiscoversPods` | Deploy controller with annotations | Query Prometheus `/api/v1/targets` | Controller pod appears as active target |

---

## Deployment

### Phase 1 (controller repo changes only)

**No infrastructure changes required.** Works with existing annotation-based Prometheus.

| File | Change |
|------|--------|
| `internal/controller/nodedeployment/metrics.go` | New — metric registration for SeiNodeDeployment |
| `internal/controller/node/metrics.go` | New — metric registration for SeiNode |
| `internal/controller/nodepool/metrics.go` | New — metric registration for SeiNodePool |
| `internal/controller/nodedeployment/controller.go` | Add metric emission, substep timing, cleanup |
| `internal/controller/node/controller.go` | Add Recorder field, metric emission, phase transition helper, cleanup, RBAC marker |
| `internal/controller/nodepool/controller.go` | Add Recorder field, metric emission, cleanup, RBAC marker |
| `cmd/main.go` | Wire EventRecorders for SeiNode and SeiNodePool; switch `metrics-secure` default to false |
| `config/manager/manager.yaml` | Add `prometheus.io/*` annotations, declare metrics port |
| Various `*_test.go` | New test cases per Test Specification |

Run `make manifests` after adding RBAC markers to regenerate ClusterRole.

### Phase 2 prerequisites (platform repo changes)

| Task | Owner | Repo |
|------|-------|------|
| Migrate to kube-prometheus-stack | Platform team | platform |
| Enable Alertmanager with receiver | Platform team | platform |
| Enable Grafana sidecar | Platform team | platform |
| Deploy dashboard ConfigMap to monitoring namespace | Platform team | platform (references controller repo) |

### Phase 2 (controller + seictl repo changes)

| File | Change |
|------|--------|
| `config/monitoring/service-monitor.yaml` | New — ServiceMonitor for controller |
| `config/monitoring/prometheus-rule.yaml` | New — PrometheusRule with alerts |
| `config/monitoring/dashboards/*.json` | New — Grafana dashboard JSON |
| `config/monitoring/sidecar-pod-monitor.yaml` | New — PodMonitor for sidecar |
| `config/monitoring/networkpolicy.yaml` | New — allow Prometheus→sidecar on 7777 |
| `config/default/kustomization.yaml` | Add `../monitoring` (ServiceMonitor + rule only, not dashboards) |
| seictl: `sidecar/server/server.go` | Add `promhttp.Handler()` on `/metrics` |
| seictl: `sidecar/metrics/metrics.go` | New — sidecar metric registration |

### Dev vs Prod differences
- Phase 1 (annotation-based) is identical across environments
- Phase 2: dev uses `kube-prometheus-stack` defaults; prod may have different scrape intervals, alert `for` durations, and Alertmanager receivers
- Dashboard ConfigMaps are identical across environments (parameterized by `$namespace` Grafana variable)

---

## Deferred (Do Not Build)

| Feature | Rationale |
|---------|-----------|
| **Distributed tracing (OpenTelemetry)** | Adds significant complexity; reconcile loops are not RPC-heavy enough to warrant trace propagation yet. Revisit when cross-service debugging is a real pain point. |
| **Custom metrics CRD** | No need for user-configurable metrics; the fixed metric set covers operational needs. |
| **Cost attribution metrics** | Requires cloud billing API integration; not a controller concern. |
| **Log-based alerting (Loki alerts)** | Phase 2 Prometheus alerting covers critical paths; Loki alerts can be added ad-hoc without design. |
| **Sidecar tracing** | Same rationale as controller tracing; sidecar tasks are self-contained. |
| **Multi-cluster metrics aggregation** | Single cluster today; Thanos/Cortex federation is an infrastructure concern. |
| **SLA/SLO reporting** | Premature; need baseline data first. Revisit after 30 days of metrics collection. |
| **Secure metrics in Phase 1** | Annotation-based scraping with in-cluster HTTP is simpler and sufficient. Re-enable TLS+auth when ServiceMonitor-based scraping is available in Phase 2. |

---

## Decision Log

| # | Decision | Rationale | Reversibility |
|---|----------|-----------|---------------|
| 1 | Use `sei_controller_` prefix for all controller metrics | Avoids collision with controller-runtime `controller_runtime_` prefix; groups all custom metrics under one namespace | Two-way: prefix is internal, can rename with a migration |
| 2 | Phase gauge with 0/1 per phase instead of info-style label | Follows kube-state-metrics convention (`kube_pod_status_phase`). Enables simple `== 1` queries and alerting without cardinality issues from multi-valued labels | Two-way: can switch to enum/info metric later |
| 3 | EventRecorder per controller, not shared | Follows controller-runtime convention; event `source.component` field correctly identifies which controller emitted | Two-way: trivial to consolidate |
| 4 | Phase 1 uses annotation-based scraping, not ServiceMonitor | Dev cluster runs community Prometheus chart (not kube-prometheus-stack). ServiceMonitor CRDs exist but no Operator reconciles them. Annotations work immediately with existing `extraScrapeConfigs` | Two-way: migrate to ServiceMonitor in Phase 2 |
| 5 | Switch metrics endpoint to HTTP (not HTTPS) in Phase 1 | Annotation-based scraping has no built-in bearer token support. In-cluster HTTP on a non-public port is the standard pattern for controllers scraped by cluster Prometheus. Re-enable TLS+auth in Phase 2 with ServiceMonitor | Two-way: flag-based, can switch back per-environment |
| 6 | Dashboards deployed to monitoring namespace via platform repo reference, not bundled with controller | Controller Kustomization applies `namespace: sei-k8s-controller-system` to all resources. Dashboard ConfigMaps must be in the monitoring namespace for Grafana to read them | Two-way: can change deployment strategy |
| 7 | PodMonitor for sidecar instead of ServiceMonitor (Phase 2) | Sidecar metrics port (7777) is on the pod, not the external Service; PodMonitor directly targets pods by label | Two-way: could use additional ServiceMonitor port instead |
| 8 | Sidecar metrics on same port (7777) as API | Avoids adding another port/listener; `/metrics` is a standard path alongside `/v0/healthz` and `/v0/tasks` | Two-way: can split to separate port later |
| 9 | Custom histogram buckets extending to 60s | `prometheus.DefBuckets` max at 10s; controller substeps and sidecar interactions can exceed this under load. 60s covers controller-runtime reconcile timeout | Two-way: bucket boundaries can be adjusted |
| 10 | Drop `name` label from init duration histogram | Per-resource histograms with N=1 observations produce meaningless percentiles. Aggregate by `chain_id` for statistical value; use per-node gauge for debugging | Two-way: can add `name` label back if needed |
| 11 | Alerting deferred to Phase 2 | Alertmanager is disabled on dev. No destination for fired alerts. Enable after kube-prometheus-stack migration | Two-way: PrometheusRule can be deployed at any time |
| 12 | Normalize sidecar HTTP paths to route templates | Raw `path` label has unbounded cardinality risk from parameterized paths (`/v0/tasks/{id}`). Bounded allowlist of 5 route templates caps cardinality | Two-way: can expand allowlist |
