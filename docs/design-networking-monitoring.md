# Design: SeiNodeGroup — Fleet Orchestration with Networking & Monitoring

**Branch:** `feature/networking-monitoring`
**Status:** Final (v3) — review findings incorporated
**Goal:** Introduce a `SeiNodeGroup` CRD that orchestrates N SeiNodes behind shared networking (Service, Istio Gateway routing, network isolation) and monitoring (ServiceMonitor), closing the gap between the current controller and sei-infra's EC2 production deployment.

---

## 1. Problem Statement

The SeiNode controller manages single-node lifecycle (bootstrap → init → running → snapshots). Production deployments require:

- **Multiple nodes** behind a shared load balancer (sei-infra runs 3 instances per role)
- **External Service** (ClusterIP/LoadBalancer) for RPC, REST, and EVM traffic
- **Ingress routing** (Istio Gateway / Kubernetes Ingress) with TLS for public endpoints
- **Network isolation** so that only the ingress gateway and authorized peers can reach node APIs
- **DNS** (Route53 via external-dns) for stable hostnames
- **Monitoring** (Prometheus ServiceMonitor) for observability

These are fleet-level concerns: a load balancer sits in front of N nodes, not one. Putting networking on each SeiNode would create N independent load balancers instead of one shared entry point.

---

## 2. Architecture Overview

```
┌──────────────────────────────────────────────────────────────────┐
│                        SeiNodeGroup                               │
│  "pacific-1-archive-rpc"                                          │
│                                                                   │
│  Owns:                                                            │
│  ├── SeiNode "pacific-1-archive-rpc-0"  ─┐                       │
│  │   └── (SeiNode controller manages     │                       │
│  │       StatefulSet, PVC, headless Svc, │  shared label:         │
│  │       sidecar tasks)                  │  sei.io/group: ...     │
│  ├── SeiNode "pacific-1-archive-rpc-1"  ─┤                       │
│  ├── SeiNode "pacific-1-archive-rpc-2"  ─┘                       │
│  │                                                                │
│  ├── Service "...-external"  (selects all 3 pods by group label)  │
│  ├── HTTPRoute "..."         (routes to shared Service)           │
│  ├── AuthorizationPolicy "..." (applied to all 3 pods)           │
│  └── ServiceMonitor "..."    (scrapes all 3 pods)                 │
└──────────────────────────────────────────────────────────────────┘
```

**SeiNode** is unchanged — it manages single-node lifecycle.
**SeiNodeGroup** is the new orchestration layer that composes SeiNodes with shared infrastructure.

This follows the same pattern as the existing `SeiNodePool → SeiNode` relationship, where SeiNodePool creates child SeiNode CRs and aggregates their status.

---

## 3. Design Principles

1. **SeiNode stays single-responsibility** — One small prerequisite change (`spec.podLabels`) is needed on SeiNode to support label propagation to pods. No networking logic is added. SeiNodeGroup owns the fleet and exposure layer.

2. **Same patterns as SeiNodePool** — The SeiNodeGroup controller follows the same `ensureSeiNode` / `updateStatus` / owner-reference patterns already established by SeiNodePool. No new controller patterns to learn.

3. **Passthrough over abstraction** — Service annotations, Ingress annotations, and Istio config use Kubernetes-native values. No DSL wrappers.

4. **Safe by default** — `DeletionPolicy` governs both networking resources and child SeiNodes. Network isolation is an additive feature, not a breaking change.

5. **Two-way doors only** — Every field is optional. WAF is just an annotation. Istio support is additive to Ingress support. Update strategy for rolling out changes across replicas is a future concern that the current design does not block.

6. **SeiNodePool vs SeiNodeGroup** — SeiNodePool is for genesis network bootstrapping (prep jobs, shared genesis PVC, then SeiNodes). SeiNodeGroup is for production fleet management (N nodes from a template + shared networking/monitoring). They target different use cases and should not manage the same SeiNodes.

---

## 4. Prerequisite: SeiNode `podLabels` Field

The shared external Service selects pods by `sei.io/group: {groupName}`. For this label to reach the pod template, the SeiNode controller must propagate it. Today, `resourceLabelsForNode()` only sets `sei.io/node: {name}` on the pod template — SeiNode metadata labels are ignored.

**Change:** Add an optional `podLabels` field to `SeiNodeSpec`. The SeiNode controller merges these into the StatefulSet pod template labels alongside the existing `sei.io/node` label.

```go
type SeiNodeSpec struct {
    // ... existing fields ...

    // PodLabels are additional labels merged into the StatefulSet pod template.
    // The controller always sets sei.io/node; these are additive.
    // +optional
    PodLabels map[string]string `json:"podLabels,omitempty"`
}
```

In `resources.go`:

```go
func resourceLabelsForNode(node *seiv1alpha1.SeiNode) map[string]string {
    labels := make(map[string]string, len(node.Spec.PodLabels)+1)
    maps.Copy(labels, node.Spec.PodLabels)   // user/group labels first
    labels[nodeLabel] = node.Name             // system label wins
    return labels
}
```

The SeiNodeGroup controller sets `podLabels: {"sei.io/group": groupName}` on each child SeiNode. This is a small, backward-compatible change — existing SeiNodes without `podLabels` behave identically.

This is scoped as a standalone prerequisite PR before Phase 1.

---

## 5. API Types

### 5.1 SeiNodeGroup (`api/v1alpha1/seinodegroup_types.go`)

```go
// SeiNodeGroupSpec defines the desired state of a SeiNodeGroup.
type SeiNodeGroupSpec struct {
    // Replicas is the number of SeiNode instances to create.
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=16
    // +kubebuilder:default=1
    Replicas int32 `json:"replicas"`

    // Template defines the SeiNode spec stamped out for each replica.
    // Each SeiNode is named "{group-name}-{ordinal}".
    Template SeiNodeTemplate `json:"template"`

    // Networking controls how the group is exposed to traffic.
    // Networking resources are shared across all replicas.
    // +optional
    Networking *NetworkingConfig `json:"networking,omitempty"`

    // Monitoring configures observability resources shared across
    // all replicas.
    // +optional
    Monitoring *MonitoringConfig `json:"monitoring,omitempty"`
}

// SeiNodeTemplate wraps a SeiNodeSpec for use in the group template.
type SeiNodeTemplate struct {
    // Metadata allows setting labels and annotations on child SeiNodes.
    // The controller always adds sei.io/group and sei.io/group-ordinal
    // labels; user-specified labels are merged.
    // +optional
    Metadata *SeiNodeTemplateMeta `json:"metadata,omitempty"`

    // Spec is the SeiNodeSpec applied to each replica.
    Spec SeiNodeSpec `json:"spec"`
}

// SeiNodeTemplateMeta defines metadata for templated SeiNodes.
type SeiNodeTemplateMeta struct {
    // Labels are merged onto each child SeiNode's metadata.
    // +optional
    Labels map[string]string `json:"labels,omitempty"`

    // Annotations are merged onto each child SeiNode's metadata.
    // +optional
    Annotations map[string]string `json:"annotations,omitempty"`
}
```

### 4.2 SeiNodeGroup Status

```go
// SeiNodeGroupPhase represents the high-level lifecycle state.
// +kubebuilder:validation:Enum=Pending;Initializing;Ready;Degraded;Failed;Terminating
type SeiNodeGroupPhase string

const (
    GroupPhasePending      SeiNodeGroupPhase = "Pending"
    GroupPhaseInitializing SeiNodeGroupPhase = "Initializing"
    GroupPhaseReady        SeiNodeGroupPhase = "Ready"
    GroupPhaseDegraded     SeiNodeGroupPhase = "Degraded"
    GroupPhaseFailed       SeiNodeGroupPhase = "Failed"
    GroupPhaseTerminating  SeiNodeGroupPhase = "Terminating"
)

// SeiNodeGroupStatus defines the observed state of a SeiNodeGroup.
type SeiNodeGroupStatus struct {
    // ObservedGeneration is the most recent generation observed by the controller.
    // Clients can check this against metadata.generation to know if the
    // status reflects the current spec.
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // Phase is the high-level lifecycle state.
    Phase SeiNodeGroupPhase `json:"phase,omitempty"`

    // Replicas is the desired number of SeiNodes.
    Replicas int32 `json:"replicas,omitempty"`

    // ReadyReplicas is the number of SeiNodes in Running phase.
    ReadyReplicas int32 `json:"readyReplicas,omitempty"`

    // Nodes reports the status of each child SeiNode.
    // +optional
    Nodes []GroupNodeStatus `json:"nodes,omitempty"`

    // NetworkingStatus reports the observed state of networking resources.
    // +optional
    NetworkingStatus *NetworkingStatus `json:"networkingStatus,omitempty"`

    // +listType=map
    // +listMapKey=type
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GroupNodeStatus is a summary of a child SeiNode's state.
type GroupNodeStatus struct {
    // Name is the SeiNode resource name.
    Name string `json:"name"`

    // Phase is the SeiNode's current phase.
    Phase SeiNodePhase `json:"phase,omitempty"`
}

// NetworkingStatus reports the observed state of networking resources.
type NetworkingStatus struct {
    // ExternalServiceName is the name of the managed external Service.
    // +optional
    ExternalServiceName string `json:"externalServiceName,omitempty"`

    // LoadBalancerIngress contains the hostname/IP assigned by the cloud
    // provider once the LoadBalancer is provisioned.
    // +optional
    LoadBalancerIngress []corev1.LoadBalancerIngress `json:"loadBalancerIngress,omitempty"`
}
```

### 4.3 SeiNodeGroup CRD markers

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sng
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Host",type=string,JSONPath=`.spec.networking.gateway.hostnames[0]`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type SeiNodeGroup struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   SeiNodeGroupSpec   `json:"spec,omitempty"`
    Status SeiNodeGroupStatus `json:"status,omitempty"`
}
```

### 4.4 Networking Types (`api/v1alpha1/networking_types.go`)

```go
// PortName is a well-known sei-node port identifier from sei-config.
// +kubebuilder:validation:Enum=rpc;rest;evm-http;evm-ws;grpc;p2p;prometheus
type PortName string

// DeletionPolicy controls what happens to managed networking resources
// when their spec is removed.
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
    DeletionPolicyDelete DeletionPolicy = "Delete"
    DeletionPolicyRetain DeletionPolicy = "Retain"
)

// NetworkingConfig controls how the group is exposed to traffic.
// +kubebuilder:validation:XValidation:rule="!has(self.ingress) || has(self.service)",message="ingress requires service to be configured"
// +kubebuilder:validation:XValidation:rule="!has(self.gateway) || has(self.service)",message="gateway requires service to be configured"
// +kubebuilder:validation:XValidation:rule="!(has(self.ingress) && has(self.gateway))",message="only one of ingress or gateway may be set"
type NetworkingConfig struct {
    // Service creates a non-headless Service shared across all replicas.
    // Each SeiNode still gets its own headless Service for pod DNS.
    // +optional
    Service *ExternalServiceConfig `json:"service,omitempty"`

    // Ingress creates a networking.k8s.io/v1 Ingress resource.
    // Use for clusters without Istio / Gateway API.
    // +optional
    Ingress *IngressConfig `json:"ingress,omitempty"`

    // Gateway creates a gateway.networking.k8s.io/v1 HTTPRoute
    // targeting a shared Gateway (e.g. Istio ingress gateway).
    // Use for clusters with Istio or a Gateway API implementation.
    // +optional
    Gateway *GatewayRouteConfig `json:"gateway,omitempty"`

    // Isolation configures network-level access control for node pods.
    // +optional
    Isolation *NetworkIsolationConfig `json:"isolation,omitempty"`

    // DeletionPolicy controls what happens to the external Service,
    // Ingress/HTTPRoute, and AuthorizationPolicy when spec.networking
    // is removed. "Delete" (default) removes managed resources. "Retain"
    // orphans them so load balancers and DNS survive spec changes.
    // +optional
    // +kubebuilder:default=Delete
    DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// ExternalServiceConfig defines the shared non-headless Service.
type ExternalServiceConfig struct {
    // Type is the Kubernetes Service type. Defaults to ClusterIP.
    // +optional
    // +kubebuilder:default=ClusterIP
    // +kubebuilder:validation:Enum=ClusterIP;LoadBalancer;NodePort
    Type corev1.ServiceType `json:"type,omitempty"`

    // Ports selects which node ports to expose. When empty, all
    // standard sei-config ports are exposed.
    // +optional
    Ports []PortName `json:"ports,omitempty"`

    // Annotations are merged onto the Service metadata.
    // +optional
    Annotations map[string]string `json:"annotations,omitempty"`
}

// IngressConfig defines a networking.k8s.io/v1 Ingress.
// +kubebuilder:validation:XValidation:rule="!has(self.tls) || !self.tls.enabled || has(self.className)",message="tls requires className to be set"
type IngressConfig struct {
    // ClassName is the IngressClass (e.g. "alb", "nginx").
    // +optional
    ClassName *string `json:"className,omitempty"`

    // Host is the DNS hostname for the Ingress rule.
    // +kubebuilder:validation:MinLength=1
    Host string `json:"host"`

    // Annotations are merged onto the Ingress metadata.
    // +optional
    Annotations map[string]string `json:"annotations,omitempty"`

    // TLS configures TLS termination.
    // +optional
    TLS *IngressTLS `json:"tls,omitempty"`

    // Paths defines routing rules. When empty, "/" routes to the
    // first Service port. Order is preserved.
    // +optional
    Paths []IngressPath `json:"paths,omitempty"`
}

// IngressTLS configures TLS for the Ingress.
type IngressTLS struct {
    // Enabled controls whether TLS is configured.
    // +kubebuilder:default=true
    Enabled bool `json:"enabled"`

    // SecretName references a TLS Secret. When omitted, the
    // IngressClass default is used (e.g. ACM for ALB).
    // +optional
    SecretName *string `json:"secretName,omitempty"`
}

// IngressPath maps a URL path to a Service port.
type IngressPath struct {
    // +kubebuilder:validation:MinLength=1
    Path string `json:"path"`

    // +optional
    // +kubebuilder:default=Prefix
    PathType networkingv1.PathType `json:"pathType,omitempty"`

    PortName PortName `json:"portName"`
}

// GatewayRouteConfig creates a gateway.networking.k8s.io/v1 HTTPRoute
// that references a shared Gateway resource.
type GatewayRouteConfig struct {
    // ParentRef identifies the shared Gateway.
    ParentRef GatewayParentRef `json:"parentRef"`

    // Hostnames are the DNS hostnames for the HTTPRoute.
    // +kubebuilder:validation:MinItems=1
    Hostnames []string `json:"hostnames"`

    // Annotations are merged onto the HTTPRoute metadata.
    // +optional
    Annotations map[string]string `json:"annotations,omitempty"`
}

// GatewayParentRef identifies a Gateway resource.
type GatewayParentRef struct {
    // +kubebuilder:validation:MinLength=1
    Name string `json:"name"`

    // +kubebuilder:validation:MinLength=1
    Namespace string `json:"namespace"`
}

// NetworkIsolationConfig defines network-level access control.
type NetworkIsolationConfig struct {
    // AuthorizationPolicy creates an Istio AuthorizationPolicy
    // restricting which identities can reach node pods.
    // +optional
    AuthorizationPolicy *AuthorizationPolicyConfig `json:"authorizationPolicy,omitempty"`
}

// AuthorizationPolicyConfig defines allowed traffic sources.
type AuthorizationPolicyConfig struct {
    // AllowedSources defines who can reach this group's pods.
    // The controller generates an ALLOW policy; traffic from
    // sources not listed here is denied.
    // +kubebuilder:validation:MinItems=1
    AllowedSources []TrafficSource `json:"allowedSources"`
}

// TrafficSource identifies a set of callers by Istio identity.
// +kubebuilder:validation:XValidation:rule="has(self.principals) || has(self.namespaces)",message="at least one of principals or namespaces must be set"
type TrafficSource struct {
    // Principals are SPIFFE identities (e.g.
    // "cluster.local/ns/istio-system/sa/istio-ingressgateway").
    // +optional
    Principals []string `json:"principals,omitempty"`

    // Namespaces allows all pods in these namespaces.
    // +optional
    Namespaces []string `json:"namespaces,omitempty"`
}
```

### 4.5 Monitoring Types (`api/v1alpha1/monitoring_types.go`)

```go
// MonitoringConfig controls observability resources.
type MonitoringConfig struct {
    // ServiceMonitor creates a monitoring.coreos.com/v1 ServiceMonitor.
    // Presence (non-nil) enables it; set to nil to disable.
    // +optional
    ServiceMonitor *ServiceMonitorConfig `json:"serviceMonitor,omitempty"`
}

// ServiceMonitorConfig defines the ServiceMonitor.
type ServiceMonitorConfig struct {
    // Interval is the Prometheus scrape interval.
    // +optional
    // +kubebuilder:default="30s"
    // +kubebuilder:validation:Pattern="^[0-9]+(ms|s|m|h)$"
    Interval string `json:"interval,omitempty"`

    // Labels are added to the ServiceMonitor metadata.
    // +optional
    Labels map[string]string `json:"labels,omitempty"`
}
```

### 4.6 Status Conditions

```go
const (
    ConditionNodesReady            = "NodesReady"
    ConditionExternalServiceReady  = "ExternalServiceReady"
    ConditionRouteReady            = "RouteReady"          // Ingress or HTTPRoute
    ConditionIsolationReady        = "IsolationReady"      // AuthorizationPolicy
    ConditionServiceMonitorReady   = "ServiceMonitorReady"
)
```

---

## 6. Labels and Naming

### Labels injected by SeiNodeGroup controller

| Label | Value | Set on | Purpose |
|-------|-------|--------|---------|
| `sei.io/group` | `{groupName}` | SeiNode metadata + `podLabels` | Shared Service selector, AuthorizationPolicy selector |
| `sei.io/group-ordinal` | `"0"`, `"1"`, ... | SeiNode metadata | Identify individual replicas |
| `sei.io/node` | `{nodeName}` | Pod template (by SeiNode controller) | Existing per-node label |

The SeiNodeGroup controller sets `sei.io/group` on both the child SeiNode's metadata labels AND `spec.podLabels`. The `podLabels` mechanism (Section 4) ensures the label propagates to the StatefulSet pod template. The shared external Service selects on `sei.io/group: {groupName}`, so all replica pods are endpoints of the same Service.

### Label merge order (system labels win)

When building child SeiNode labels, user-specified template labels are applied first, then system labels overwrite. This prevents a user from accidentally breaking the group selector:

```go
// User labels first
maps.Copy(labels, group.Spec.Template.Metadata.Labels)
// System labels overwrite
labels["sei.io/group"] = group.Name
labels["sei.io/group-ordinal"] = strconv.Itoa(ordinal)
```

### Resource naming

| Resource | Name | Why |
|----------|------|-----|
| SeiNode | `{group}-{ordinal}` | Matches SeiNodePool pattern |
| External Service | `{group}-external` | Distinguishes from per-node headless Services |
| HTTPRoute | `{group}` | One route per group |
| Ingress | `{group}` | One ingress per group |
| AuthorizationPolicy | `{group}` | Applied to all group pods |
| ServiceMonitor | `{group}` | Scrapes all group pods |

---

## 7. Controller Reconciliation

### 7.1 File Organization

```
internal/controller/
├── node/                        # SeiNode controller (UNCHANGED)
│   ├── controller.go
│   ├── resources.go
│   ├── plan_execution.go
│   ├── ...
│
├── nodegroup/                   # NEW: SeiNodeGroup controller
│   ├── controller.go            # Reconcile loop, phase transitions
│   ├── nodes.go                 # ensureSeiNode, scaleDown
│   ├── networking.go            # External Service, Ingress, HTTPRoute,
│   │                            # AuthorizationPolicy generation + reconcile
│   ├── monitoring.go            # ServiceMonitor generation + reconcile
│   ├── status.go                # Status aggregation
│   ├── labels.go                # Label helpers, naming
│   ├── networking_test.go
│   ├── monitoring_test.go
│   ├── nodes_test.go
│   └── status_test.go
│
└── nodepool/                    # SeiNodePool controller (UNCHANGED)
    ├── controller.go
    └── ...
```

### 7.2 Reconcile Flow

```go
func (r *SeiNodeGroupReconciler) Reconcile(ctx, req) (Result, error) {
    group := &SeiNodeGroup{}
    r.Get(ctx, req, group)

    // Deletion handling (respects DeletionPolicy for networking AND child SeiNodes)
    if !group.DeletionTimestamp.IsZero() {
        return r.handleDeletion(ctx, group)
    }
    r.ensureFinalizer(ctx, group)

    // 1. Ensure N SeiNodes exist from template
    r.reconcileSeiNodes(ctx, group)

    // 2. Networking (independent of SeiNode readiness)
    r.reconcileNetworking(ctx, group)

    // 3. Monitoring
    r.reconcileMonitoring(ctx, group)

    // 4. Status aggregation (sets observedGeneration)
    r.updateStatus(ctx, group)

    // Periodic requeue to catch drift on unstructured resources
    // (HTTPRoute, AuthorizationPolicy, ServiceMonitor) that lack Owns() watches
    return ctrl.Result{RequeueAfter: statusPollInterval}, nil
}
```

### 7.3 SeiNode Management (`nodes.go`)

Follows the SeiNodePool pattern:

```go
func (r *SeiNodeGroupReconciler) reconcileSeiNodes(ctx, group) error {
    for i := range group.Spec.Replicas {
        r.ensureSeiNode(ctx, group, i)
    }
    return r.scaleDown(ctx, group)
}

func (r *SeiNodeGroupReconciler) ensureSeiNode(ctx, group, ordinal) error {
    desired := generateSeiNode(group, ordinal)
    // Set owner reference, create-or-update
    // On update: sync Image, Entrypoint, Sidecar (same as SeiNodePool)
}

func generateSeiNode(group, ordinal) *SeiNode {
    // User labels first, then system labels overwrite
    labels := make(map[string]string)
    if group.Spec.Template.Metadata != nil {
        maps.Copy(labels, group.Spec.Template.Metadata.Labels)
    }
    labels["sei.io/group"] = group.Name
    labels["sei.io/group-ordinal"] = strconv.Itoa(ordinal)

    spec := group.Spec.Template.Spec.DeepCopy()
    // Inject podLabels so the SeiNode controller propagates sei.io/group to pods
    if spec.PodLabels == nil {
        spec.PodLabels = make(map[string]string)
    }
    spec.PodLabels["sei.io/group"] = group.Name

    return &SeiNode{
        ObjectMeta: ObjectMeta{
            Name:      fmt.Sprintf("%s-%d", group.Name, ordinal),
            Namespace: group.Namespace,
            Labels:    labels,
        },
        Spec: *spec,
    }
}
```

**Scale-down guard:** The `scaleDown` function refuses to delete SeiNodes if the computed desired count is 0 (defensive against uninitialized fields or controller bugs). The `Minimum=1` CEL validation on `replicas` prevents 0 at admission, but the guard catches code-level errors:

```go
func (r *SeiNodeGroupReconciler) scaleDown(ctx, group) error {
    if group.Spec.Replicas <= 0 {
        log.Error("refusing scale-down: desired replicas is zero or negative")
        return nil
    }
    // Delete SeiNodes with ordinal >= group.Spec.Replicas
}
```

### 7.4 Networking Reconciliation (`networking.go`)

Each networking resource is managed independently:

```go
func (r *SeiNodeGroupReconciler) reconcileNetworking(ctx, group) error {
    r.reconcileExternalService(ctx, group)
    r.reconcileRoute(ctx, group)          // Ingress OR HTTPRoute
    r.reconcileIsolation(ctx, group)      // AuthorizationPolicy
}
```

**External Service:**
- Uses server-side apply with `fieldOwner: seinodegroup-controller`
- Selector: `sei.io/group: {groupName}` (matches all replica pods via `podLabels`)
- Does NOT set `PublishNotReadyAddresses` (natural readiness gating)
- If spec is nil and `deletionPolicy: Delete`, delete the Service
- If spec is nil and `deletionPolicy: Retain`, remove owner reference (orphan)

**HTTPRoute / Ingress:**
- Generated as `unstructured.Unstructured` (avoids importing Gateway API Go modules)
- Backend targets `{group}-external` Service
- If CRD not installed (no Gateway API), sets `RouteReady` condition to False

**AuthorizationPolicy:**
- Generated as `unstructured.Unstructured` (avoids importing Istio Go modules)
- Selector: `sei.io/group: {groupName}`
- Action: ALLOW with specified principals/namespaces
- **Controller SA auto-injection:** The controller always adds its own ServiceAccount principal to the AuthorizationPolicy, ensuring sidecar communication (port 7777) is never blocked. This is injected at generation time, not visible in the user's spec. Without this, a SeiNode controller running in a different namespace (e.g. `sei-system`) would be unable to drive node initialization via the sidecar API.
- If CRD not installed (no Istio), sets `IsolationReady` condition to False

### 7.5 Monitoring Reconciliation (`monitoring.go`)

**ServiceMonitor:**
- Generated as `unstructured.Unstructured`
- Selector: `sei.io/group: {groupName}` (scrapes all replica pods)
- Port: `prometheus` (26660)
- If CRD not installed, sets `ServiceMonitorReady` condition to False

### 7.6 Status Aggregation (`status.go`)

```go
func (r *SeiNodeGroupReconciler) updateStatus(ctx, group) error {
    // List child SeiNodes by label
    nodeList := r.listChildSeiNodes(ctx, group)

    // Count ready/total
    var readyReplicas int32
    for _, node := range nodeList {
        if node.Status.Phase == PhaseRunning { readyReplicas++ }
    }

    // Determine group phase
    phase := groupPhase(readyReplicas, group.Spec.Replicas, nodeList)

    // Read external Service for LB status
    networkingStatus := r.readNetworkingStatus(ctx, group)

    // Patch status
    group.Status.Replicas = group.Spec.Replicas
    group.Status.ReadyReplicas = readyReplicas
    group.Status.Phase = phase
    group.Status.Nodes = nodeStatuses(nodeList)
    group.Status.ObservedGeneration = group.Generation
    group.Status.NetworkingStatus = networkingStatus
}
```

**Phase logic:** The `groupPhase` function differentiates between scaling-up (some nodes in Initializing/PreInitializing) and actual failures:

| Condition | Phase |
|-----------|-------|
| All replicas Running | `Ready` |
| Some replicas Running, rest progressing (Pending/Initializing) | `Initializing` |
| Some replicas Running, some Failed | `Degraded` |
| All replicas Failed | `Failed` |
| No replicas exist yet | `Pending` |

The `NodesReady` condition provides detail: `"2/3 nodes ready (1 initializing)"`.

### 7.7 Deletion Handling

The `DeletionPolicy` governs both networking resources AND child SeiNodes:

| DeletionPolicy | Networking resources | Child SeiNodes |
|----------------|---------------------|----------------|
| `Delete` | Deleted | Deleted (via owner ref GC) |
| `Retain` | Orphaned (owner ref removed) | Orphaned (owner ref removed, continue running independently) |

When `Retain`, the finalizer removes owner references from all managed resources before allowing the SeiNodeGroup to be deleted. This prevents Kubernetes GC from cascading the deletion.

### 7.8 RBAC

```go
// +kubebuilder:rbac:groups=sei.io,resources=seinodegroups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodegroups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sei.io,resources=seinodegroups/finalizers,verbs=update
// +kubebuilder:rbac:groups=sei.io,resources=seinodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodes/status,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=security.istio.io,resources=authorizationpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete
```

### 7.9 SetupWithManager

```go
func (r *SeiNodeGroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
    return ctrl.NewControllerManagedBy(mgr).
        For(&seiv1alpha1.SeiNodeGroup{}).
        Owns(&seiv1alpha1.SeiNode{}).
        Owns(&corev1.Service{}).
        Owns(&networkingv1.Ingress{}).
        Named("seinodegroup").
        Complete(r)
}
```

Note: HTTPRoute, AuthorizationPolicy, and ServiceMonitor are unstructured, so we don't add `Owns()` for them. Their reconciliation is idempotent and driven by the SeiNodeGroup reconcile loop.

---

## 8. AWS Topology Patterns

### Pattern 1: Istio Gateway + Network Isolation (recommended)

Traffic flow: `Client → ALB (WAF) → Istio Gateway → Envoy sidecar → seid`

The controller manages the HTTPRoute and AuthorizationPolicy. The ALB and Istio Gateway are platform-level resources.

```yaml
networking:
  deletionPolicy: Retain
  service:
    type: ClusterIP
    ports: ["rpc", "rest", "evm-http", "evm-ws"]
  gateway:
    parentRef:
      name: sei-gateway
      namespace: istio-system
    hostnames:
      - rpc.sei-archive.pacific-1.seinetwork.io
  isolation:
    authorizationPolicy:
      allowedSources:
        - principals: ["cluster.local/ns/istio-system/sa/istio-ingressgateway"]
        - namespaces: ["sei-nodes"]
```

### Pattern 2: ALB Ingress (no Istio)

Traffic flow: `Client → ALB (WAF, TLS) → pod`

```yaml
networking:
  deletionPolicy: Retain
  service:
    type: ClusterIP
    ports: ["rpc", "rest", "evm-http", "evm-ws"]
  ingress:
    className: alb
    host: rpc.sei-archive.pacific-1.seinetwork.io
    tls:
      enabled: true
    annotations:
      alb.ingress.kubernetes.io/scheme: internet-facing
      alb.ingress.kubernetes.io/target-type: ip
      alb.ingress.kubernetes.io/wafv2-acl-arn: "arn:aws:wafv2:..."
```

### Pattern 3: NLB for TCP (p2p, gRPC)

```yaml
networking:
  service:
    type: LoadBalancer
    ports: ["p2p", "grpc"]
    annotations:
      service.beta.kubernetes.io/aws-load-balancer-scheme: internet-facing
      service.beta.kubernetes.io/aws-load-balancer-nlb-target-type: ip
```

### WAF

WAF is an ALB annotation (`alb.ingress.kubernetes.io/wafv2-acl-arn`). The WAF WebACL is provisioned by the platform team (Terraform) and referenced by ARN. This is a two-way door — adding or removing the annotation just toggles WAF on the ALB.

### DNS

DNS is handled by external-dns, which reads HTTPRoute `hostnames` or Ingress `host` fields and creates Route53 records. Prerequisites:
- external-dns deployed with `--source=ingress` and/or `--source=gateway-httproute`
- `--domain-filter` matching the target domain
- IAM permissions for Route53

---

## 9. Complete Sample Manifest

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodeGroup
metadata:
  name: pacific-1-archive-rpc
  namespace: sei-nodes
spec:
  replicas: 3

  template:
    metadata:
      labels:
        sei.io/chain: pacific-1
        sei.io/role: archive
    spec:
      chainId: pacific-1
      image: "ghcr.io/sei-protocol/sei:v6.3.0"
      sidecar:
        image: ghcr.io/sei-protocol/seictl@sha256:64f92fb...
        resources:
          requests:
            cpu: "500m"
            memory: "256Mi"
      entrypoint:
        command: ["seid"]
        args: ["start", "--home", "/sei"]
      storage:
        retainOnDelete: true
      archive:
        peers:
          - ec2Tags:
              region: eu-central-1
              tags:
                ChainIdentifier: pacific-1
                Component: state-syncer
        snapshotGeneration:
          keepRecent: 5
          destination:
            s3:
              bucket: pacific-1-snapshots
              prefix: state-sync/
              region: eu-central-1

  networking:
    deletionPolicy: Retain
    service:
      type: ClusterIP
      ports: ["rpc", "rest", "evm-http", "evm-ws"]
    gateway:
      parentRef:
        name: sei-gateway
        namespace: istio-system
      hostnames:
        - rpc.sei-archive.pacific-1.seinetwork.io
    isolation:
      authorizationPolicy:
        allowedSources:
          - principals:
              - "cluster.local/ns/istio-system/sa/istio-ingressgateway"
          - namespaces:
              - "sei-nodes"

  monitoring:
    serviceMonitor:
      interval: "30s"
      labels:
        release: prometheus
```

**Generated resources:**

| Resource | Name | Kind |
|----------|------|------|
| SeiNode | pacific-1-archive-rpc-0 | sei.io/v1alpha1/SeiNode |
| SeiNode | pacific-1-archive-rpc-1 | sei.io/v1alpha1/SeiNode |
| SeiNode | pacific-1-archive-rpc-2 | sei.io/v1alpha1/SeiNode |
| Service | pacific-1-archive-rpc-external | v1/Service (ClusterIP) |
| HTTPRoute | pacific-1-archive-rpc | gateway.networking.k8s.io/v1/HTTPRoute |
| AuthorizationPolicy | pacific-1-archive-rpc | security.istio.io/v1/AuthorizationPolicy |
| ServiceMonitor | pacific-1-archive-rpc | monitoring.coreos.com/v1/ServiceMonitor |

Plus each SeiNode creates its own StatefulSet, PVC, and headless Service (managed by the existing SeiNode controller).

---

## 10. What Changes vs. What Stays the Same

| Concern | Status |
|---------|--------|
| SeiNode CRD | **SMALL CHANGE** — add optional `spec.podLabels` field (prerequisite) |
| SeiNode controller | **SMALL CHANGE** — merge `podLabels` into StatefulSet pod template |
| SeiNodePool controller | **UNCHANGED** |
| SeiNodePool CRD | **UNCHANGED** |
| SeiNodeGroup CRD | **NEW** |
| Networking types | **NEW** (used by SeiNodeGroup) |
| Monitoring types | **NEW** (used by SeiNodeGroup) |
| SeiNodeGroup controller | **NEW** |

Existing SeiNode manifests without `podLabels` continue to work identically. SeiNodeGroup is additive.

---

## 11. Reversibility Analysis

| Decision | How to reverse | Impact |
|----------|---------------|--------|
| `spec.podLabels` on SeiNode | Remove field, regenerate CRD. Existing nodes unaffected (nil defaults to empty map). | None |
| New SeiNodeGroup CRD | Delete SeiNodeGroup with `DeletionPolicy: Retain`. Child SeiNodes and networking resources are orphaned and keep running. | SeiNodes become standalone |
| `sei.io/group` label on child SeiNodes | Remove label. SeiNode controller doesn't read this label. | None |
| Unstructured HTTPRoute / AuthorizationPolicy / ServiceMonitor | Switch to typed imports later. Same apply semantics. | Code change only |
| `DeletionPolicy` (covers nodes + networking) | Change per-group. Existing groups unaffected. | Per-resource |
| Ingress alongside Gateway API | Both optional, mutually exclusive. Remove one whenever ready. | Additive |
| WAF | Just an annotation on Ingress/HTTPRoute. Add/remove at will. | Two-way door |
| Network isolation via AuthorizationPolicy | Optional field. Remove to disable. Istio defaults to ALLOW-all when no policy exists. | Two-way door |
| Controller SA auto-injection in AuthPolicy | Implementation detail. User never sees it in spec. | Transparent |

---

## 12. Implementation Plan

### Phase 0: SeiNode Prerequisite (standalone PR)
- [ ] Add `spec.podLabels` field to `SeiNodeSpec`
- [ ] Update `resourceLabelsForNode()` to merge `podLabels` into pod template
- [ ] Unit tests for label propagation
- [ ] Regenerate CRD manifests

### Phase 1: SeiNodeGroup CRD + Node Orchestration
- [ ] `api/v1alpha1/seinodegroup_types.go` — SeiNodeGroup CRD types
- [ ] `api/v1alpha1/networking_types.go` — Networking types
- [ ] `api/v1alpha1/monitoring_types.go` — Monitoring types
- [ ] `internal/controller/nodegroup/controller.go` — Reconcile loop with `RequeueAfter`
- [ ] `internal/controller/nodegroup/nodes.go` — SeiNode create/update/scaleDown with guard
- [ ] `internal/controller/nodegroup/labels.go` — Label helpers, naming, merge-order safety
- [ ] `internal/controller/nodegroup/status.go` — Status aggregation with `observedGeneration`
- [ ] Wire into `cmd/main.go`
- [ ] `make manifests` to generate CRD + RBAC
- [ ] Unit tests for node orchestration and status

### Phase 2: Shared Networking
- [ ] `internal/controller/nodegroup/networking.go` — External Service, Ingress, HTTPRoute, AuthorizationPolicy
- [ ] Controller SA auto-injection into AuthorizationPolicy
- [ ] DeletionPolicy logic (delete vs orphan, covers nodes + networking)
- [ ] Status conditions for each networking resource
- [ ] LB ingress reporting
- [ ] Unit tests for resource generation
- [ ] Integration tests

### Phase 3: Monitoring + Samples + Documentation
- [ ] `internal/controller/nodegroup/monitoring.go` — ServiceMonitor
- [ ] Sample manifests (Istio pattern, ALB pattern, NLB pattern)
- [ ] Documentation (external-dns prerequisites, Istio prerequisites)
- [ ] Printer columns on SeiNodeGroup
- [ ] Update `production-deployment-analysis.md` gap table

---

## 13. Resolved Questions

| Question | Resolution |
|----------|-----------|
| Controller SA in AuthorizationPolicy | Auto-injected (Section 7.4). The controller always adds its own SA to prevent sidecar communication being blocked. |
| Scaling-up vs degraded phase | Differentiated (Section 7.6). `Initializing` = some nodes progressing. `Degraded` = some nodes failed. `NodesReady` condition provides detail. |
| Label propagation to pods | Resolved via `spec.podLabels` prerequisite (Section 4). |
| Label merge order | System labels overwrite user labels (Section 6). |
| Child SeiNode GC on group deletion | `DeletionPolicy` covers child SeiNodes (Section 7.7). `Retain` orphans everything. |

## 14. Future Scope (explicitly not blocked)

| Feature | Why deferred | How current design accommodates |
|---------|-------------|--------------------------------|
| **Rolling update strategy** | Whole feature in itself; needs careful design around archive node sync times | `ensureSeiNode` updates one node at a time naturally; adding `maxUnavailable` / ordered rollout is additive to the reconcile loop |
| **Heterogeneous groups** | Current use case is homogeneous replicas | Separate SeiNodeGroups with different templates can share an external Service via matching labels. A future `overrides` per-ordinal field is additive. |
| **WAF provisioning from K8s** | WAF WebACL is a platform concern | Annotation passthrough makes WAF ARN a two-way door. AWS ACK or Crossplane can manage the WebACL separately. |
| **Gateway API route rules** | HTTPRoute with no explicit rules routes all traffic to the Service | A `rules` field can be added to `GatewayRouteConfig` without breaking existing manifests |
| **Multi-listener Gateway** | Current design targets a single Gateway listener | `GatewayParentRef` can be extended with `sectionName *string` for listener targeting |
| **SeiNodePool + SeiNodeGroup unification** | Different lifecycle needs (genesis vs fleet) | Both create SeiNodes but don't share children. Could merge in a future major version. |
