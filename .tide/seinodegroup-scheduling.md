# Component: SeiNodeGroup Scheduling and Topology

## Owner

Kubernetes Specialist + Platform Engineer (joint ownership)

## Phase

- **Phase 1**: Implicit AZ spread for grouped nodes (no API change)
- **Phase 2**: Explicit `placement` field on SeiNodeGroup CRD
- **Phase 3**: Full scheduling override (future, deferred)

## Purpose

SeiNodeGroups serving RPC traffic via HTTPRoute must survive availability zone failures without manual intervention. Today, all scheduling is hardcoded in `PlatformConfig` (Karpenter nodepool affinity + toleration) with no topology spread constraints and no pod anti-affinity. A 3-replica group could land entirely in one AZ, making the NLB-backed HTTPRoute a single point of failure at the zone level.

This component adds topology-aware scheduling to the SeiNode controller so that pods belonging to the same SeiNodeGroup are automatically spread across availability zones. It also introduces a PodDisruptionBudget per group so voluntary disruptions (node upgrades, Karpenter consolidation) cannot take down all replicas simultaneously.

**Business need**: Production RPC availability during AZ failures for SeiNodeGroups with `replicas > 1`.

## Dependencies

- **SeiNodeGroup controller** (`sei-node-controller-networking/internal/controller/nodegroup/`) — creates child SeiNode resources with `sei.io/group` pod label
- **SeiNode controller** (`sei-node-controller-networking/internal/controller/node/`) — translates SeiNodeSpec into StatefulSet pod spec via `buildNodePodSpec()`. **Note**: The production binary is `sei-node-controller-networking`, which already has `PodLabels` on `SeiNodeSpec` and merges them into pod template labels via `resourceLabelsForNode`. The `sei-k8s-controller` repo's version does NOT have `PodLabels` support. All Phase 1 changes target the networking controller repo.
- **Karpenter NodePool** — provisions nodes with `karpenter.sh/nodepool` label; respects topology spread constraints when choosing which zone to provision into
- **EBS CSI driver** — creates gp3 RWO PVCs that are zone-bound after initial provisioning. **Prerequisite**: The StorageClass must use `volumeBindingMode: WaitForFirstConsumer` for topology-aware scheduling to work. Immediate binding defeats the spread by binding PVCs to a random zone before the scheduler evaluates topology spread constraints.
- **Does NOT depend on**: Istio, Gateway API, or any networking component. Scheduling is orthogonal to traffic routing.

## Interface Specification

### Phase 1: Implicit AZ Spread (no CRD change)

No new API types. The SeiNode controller detects that a node belongs to a group via the `sei.io/group` pod label and injects a topology spread constraint automatically.

**Target repo**: `sei-node-controller-networking` (the production binary where `SeiNodeSpec.PodLabels` and `resourceLabelsForNode` merging already exist).

**Modified function signature** (no change — internal behavior only):

```go
// resources.go
func buildNodePodSpec(node *seiv1alpha1.SeiNode, platform PlatformConfig) corev1.PodSpec
```

**Injected topology spread constraint** (when `node.Spec.PodLabels["sei.io/group"]` is non-empty):

```go
corev1.TopologySpreadConstraint{
    MaxSkew:           1,
    TopologyKey:       "topology.kubernetes.io/zone",
    WhenUnsatisfiable: corev1.ScheduleAnyway,
    LabelSelector: &metav1.LabelSelector{
        MatchLabels: map[string]string{
            "sei.io/group": node.Spec.PodLabels["sei.io/group"],
        },
    },
}
```

Design decisions for Phase 1:

| Decision | Value | Rationale |
|----------|-------|-----------|
| `MaxSkew` | `1` | Strictest spread — at most 1 more pod in any zone than the least-populated zone |
| `TopologyKey` | `topology.kubernetes.io/zone` | Standard Kubernetes zone label; Karpenter and EKS both populate it |
| `WhenUnsatisfiable` | `ScheduleAnyway` | Prefer spread but don't block scheduling if zones are asymmetric. A node stuck Pending is worse than imperfect spread for Phase 1. Phase 2 lets users choose `DoNotSchedule`. |
| `LabelSelector` | `sei.io/group: {groupName}` | Scoped to the group, not all sei-nodes. Two different groups don't interfere with each other's spread. |

**Karpenter interaction note**: With `ScheduleAnyway`, Karpenter treats the constraint as a soft preference during batch provisioning. Combined with `karpenter.sh/do-not-disrupt: "true"` on all sei-node pods, the initial placement is permanent — Karpenter will never rebalance pods across zones after creation. Operators should verify AZ distribution after initial group creation with `kubectl get pods -l sei.io/group={name} -o wide` and consider recreating pods if the spread is poor. Phase 2's `DoNotSchedule` option provides stronger guarantees.

**PodDisruptionBudget** (Phase 1 — created by SeiNodeGroup controller):

```go
type: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {group-name}
  namespace: {group-namespace}
spec:
  maxUnavailable: 1
  selector:
    matchLabels:
      sei.io/group: {group-name}
```

The PDB uses `maxUnavailable: 1` (not `minAvailable`) to cap simultaneous disruptions at exactly 1 pod regardless of group size. This is the standard practice for availability-critical workloads — with `minAvailable: 1` a 5-replica group would allow 4 simultaneous disruptions, creating a capacity cliff behind the NLB.

The PDB is created by the SeiNodeGroup controller (not the SeiNode controller) because it spans all pods in the group. It is only created when `replicas > 1` (a PDB on a single-replica group would block all voluntary disruptions).

**Scale-up transient**: When scaling from 1 to N replicas, the PDB is created immediately but only 1 pod may be Ready. With `maxUnavailable: 1`, `disruptionsAllowed` may be 0 until new pods complete their multi-hour startup (snapshot restore, state sync). This blocks voluntary disruptions (Karpenter consolidation, node drain) during the scale-up window. This is acceptable — protecting running workloads during scale-up is the safer default. Document this in operational runbooks.

### Phase 2: Explicit Placement Field (CRD change)

New types added to `api/v1alpha1/`:

```go
// placement_types.go

// PlacementConfig controls how pods in a SeiNodeGroup are scheduled
// across the cluster topology.
type PlacementConfig struct {
    // TopologySpread controls how pods are distributed across failure
    // domains. When nil, the controller applies a default AZ spread
    // with MaxSkew=1 and WhenUnsatisfiable=ScheduleAnyway.
    // +optional
    TopologySpread *TopologySpreadConfig `json:"topologySpread,omitempty"`

    // PodDisruptionBudget controls voluntary disruption tolerance.
    // When nil, the controller creates a PDB with maxUnavailable=1
    // for groups with replicas > 1.
    // +optional
    PodDisruptionBudget *PDBConfig `json:"podDisruptionBudget,omitempty"`
}

// TopologySpreadConfig configures topology spread constraints for the group.
type TopologySpreadConfig struct {
    // MaxSkew is the maximum difference in pod count between any two
    // topology domains. Defaults to 1.
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:default=1
    // +optional
    MaxSkew int32 `json:"maxSkew,omitempty"`

    // WhenUnsatisfiable controls what happens when spread cannot be
    // satisfied. "ScheduleAnyway" (default) applies a soft preference.
    // "DoNotSchedule" makes it a hard requirement — pods will stay
    // Pending if no valid zone exists.
    // +kubebuilder:validation:Enum=DoNotSchedule;ScheduleAnyway
    // +kubebuilder:default=ScheduleAnyway
    // +optional
    WhenUnsatisfiable corev1.UnsatisfiableConstraintAction `json:"whenUnsatisfiable,omitempty"`

    // Disabled suppresses the default topology spread constraint.
    // Use this for workloads where AZ locality is preferred over
    // spread (e.g., low-latency validator sets that benefit from
    // same-zone communication).
    // +optional
    Disabled bool `json:"disabled,omitempty"`
}

// PDBConfig controls the PodDisruptionBudget for the group.
type PDBConfig struct {
    // MaxUnavailable is the maximum number of pods that can be
    // simultaneously unavailable during voluntary disruptions.
    // Defaults to 1. Only integer values are supported; percentage-
    // based values are not supported due to ambiguity at small
    // replica counts.
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:default=1
    // +optional
    MaxUnavailable *int32 `json:"maxUnavailable,omitempty"`

    // Disabled suppresses PDB creation entirely.
    // +optional
    Disabled bool `json:"disabled,omitempty"`
}
```

**SeiNodeGroupSpec change:**

```go
type SeiNodeGroupSpec struct {
    Replicas        int32              `json:"replicas"`
    Template        SeiNodeTemplate    `json:"template"`
    DeletionPolicy  DeletionPolicy     `json:"deletionPolicy,omitempty"`
    Networking      *NetworkingConfig  `json:"networking,omitempty"`
    Monitoring      *MonitoringConfig  `json:"monitoring,omitempty"`
    // NEW — Phase 2
    Placement       *PlacementConfig   `json:"placement,omitempty"`
}
```

**SeiNodeSpec change** (to carry topology config down to the SeiNode controller):

```go
type SeiNodeSpec struct {
    // ... existing fields ...

    // Scheduling holds pod scheduling directives passed down from the
    // SeiNodeGroup. Not intended for direct user configuration on
    // standalone SeiNodes — set via SeiNodeGroup.spec.placement instead.
    // +optional
    Scheduling *SchedulingConfig `json:"scheduling,omitempty"`
}

// SchedulingConfig holds pod-level scheduling directives.
type SchedulingConfig struct {
    // TopologySpreadConstraints are injected into the StatefulSet pod spec.
    // +optional
    TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
}
```

**One-way door**: The CRD field names `placement`, `topologySpread`, and `scheduling` become part of the API contract once controllers depend on them. The names are chosen to align with Kubernetes terminology (`placement` is the user-facing group concept; `scheduling` is the lower-level pod concept passed through).

### Cross-Component Interface: SeiNodeGroup → SeiNode → StatefulSet

```
SeiNodeGroup.spec.placement
    ↓ (SeiNodeGroup controller: generateSeiNode)
SeiNode.spec.scheduling.topologySpreadConstraints
    ↓ (SeiNode controller: buildNodePodSpec)
StatefulSet.spec.template.spec.topologySpreadConstraints
```

In Phase 1, the SeiNode controller infers the constraint from `sei.io/group` in `PodLabels`. In Phase 2, the SeiNodeGroup controller explicitly sets `SeiNode.spec.scheduling.topologySpreadConstraints` from the resolved placement config, and the SeiNode controller passes it through to the pod spec.

**Phase 2 invariant**: `buildNodePodSpec` MUST check `node.Spec.Scheduling` FIRST and skip the Phase 1 `sei.io/group` label inference when `Scheduling` is non-nil. This prevents duplicate topology spread constraints during the Phase 1→2 transition.

**Phase 2 rollout window**: During the brief period where the new SeiNodeGroup controller writes `spec.scheduling` but the old SeiNode controller ignores it, the system falls back to Phase 1 inference. This is safe and produces identical behavior.

### Status Condition: ConditionPDBReady

A new status condition `PDBReady` is added to `SeiNodeGroup`, following the pattern of `ExternalServiceReady`, `RouteReady`, `IsolationReady`, and `ServiceMonitorReady`:

```go
const ConditionPDBReady = "PDBReady"
```

- Set to `True` after successful PDB reconciliation
- Set to `False` with reason `ReconcileError` on PDB SSA failure
- Removed via `removeCondition` when `replicas <= 1` or `placement.podDisruptionBudget.disabled == true`

## State Model

### Topology Spread State

| State | Location | Source of Truth |
|-------|----------|-----------------|
| Desired spread config | `SeiNodeGroup.spec.placement` | User / GitOps |
| Resolved per-node constraints | `SeiNode.spec.scheduling` | SeiNodeGroup controller |
| Applied pod constraints | `StatefulSet.spec.template.spec.topologySpreadConstraints` | SeiNode controller |
| Actual pod zone | `pod.spec.nodeName` → node's `topology.kubernetes.io/zone` label | Kubernetes scheduler |

### PDB State

| State | Location | Source of Truth |
|-------|----------|-----------------|
| Desired PDB config | `SeiNodeGroup.spec.placement.podDisruptionBudget` | User / GitOps |
| PDB resource | `PodDisruptionBudget/{group-name}` in group namespace | SeiNodeGroup controller |
| PDB health condition | `SeiNodeGroup.status.conditions[type=PDBReady]` | SeiNodeGroup controller |
| Disruption budget | `PDB.status.disruptionsAllowed` | Kubernetes PDB controller |

### State Transitions

PDB lifecycle:

```
SeiNodeGroup created (replicas > 1)        → PDB created (maxUnavailable=1), ConditionPDBReady=True
SeiNodeGroup scaled to 1                   → PDB deleted, ConditionPDBReady removed
SeiNodeGroup deleted (DeletionPolicy=Delete)→ PDB cascade-deleted via owner reference
SeiNodeGroup deleted (DeletionPolicy=Retain)→ PDB orphaned (owner reference removed), survives with retained pods
```

## Internal Design

### Phase 1 Changes

#### Target Repository

All Phase 1 changes target `sei-node-controller-networking`, which is the production binary. This repo already has:
- `SeiNodeSpec.PodLabels` field
- `resourceLabelsForNode` that merges `PodLabels` into pod template labels
- `sei.io/group` set in `PodLabels` by `generateSeiNode`

Existing StatefulSets created by this controller already have `sei.io/group` in both their selector and pod template labels, so adding `TopologySpreadConstraints` to the pod template does NOT change the selector. SSA will update the pod template and trigger a RollingUpdate of existing pods. **Operator note**: For seid pods with multi-hour startup (snapshot restore), coordinate the topology spread rollout during a maintenance window or accept that pods will restart on next reconcile cycle (30s).

#### SeiNode Controller: `buildNodePodSpec` (resources.go)

After building the existing pod spec, check if the node belongs to a group and inject the topology spread constraint:

```go
func buildNodePodSpec(node *seiv1alpha1.SeiNode, platform PlatformConfig) corev1.PodSpec {
    // ... existing spec construction ...

    if groupName := node.Spec.PodLabels[seiv1alpha1.LabelGroup]; groupName != "" {
        spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
            MaxSkew:           1,
            TopologyKey:       "topology.kubernetes.io/zone",
            WhenUnsatisfiable: corev1.ScheduleAnyway,
            LabelSelector: &metav1.LabelSelector{
                MatchLabels: map[string]string{
                    seiv1alpha1.LabelGroup: groupName,
                },
            },
        }}
    }

    return spec
}
```

#### Shared Label Constants: api/v1alpha1/constants.go

Both controllers need the same label values. Export them from the shared API package:

```go
// api/v1alpha1/constants.go
const (
    LabelGroup        = "sei.io/group"
    LabelGroupOrdinal = "sei.io/group-ordinal"
    LabelNode         = "sei.io/node"
)
```

The existing unexported constants in `nodegroup/labels.go` (`groupLabel`, `groupOrdinalLabel`, `nodeLabel`) must be updated to reference these exported constants to prevent drift.

#### SeiNodeGroup Controller: PDB reconciliation (pdb.go — new file)

```go
func (r *SeiNodeGroupReconciler) reconcilePDB(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
    if group.Spec.Replicas <= 1 {
        removeCondition(group, ConditionPDBReady)
        return r.deletePDB(ctx, group)
    }

    desired := &policyv1.PodDisruptionBudget{
        ObjectMeta: metav1.ObjectMeta{
            Name:        group.Name,
            Namespace:   group.Namespace,
            Labels:      resourceLabels(group),
            Annotations: managedByAnnotations(),
        },
        Spec: policyv1.PodDisruptionBudgetSpec{
            MaxUnavailable: ptr.To(intstr.FromInt32(1)),
            Selector: &metav1.LabelSelector{
                MatchLabels: groupSelector(group),
            },
        },
    }
    if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
        return fmt.Errorf("setting owner reference: %w", err)
    }
    desired.SetGroupVersionKind(policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"))
    //nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
    if err := r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
        setCondition(group, ConditionPDBReady, metav1.ConditionFalse,
            "ReconcileError", fmt.Sprintf("Failed to reconcile PDB: %v", err))
        return err
    }
    setCondition(group, ConditionPDBReady, metav1.ConditionTrue, "Reconciled", "PDB is up to date")
    return nil
}

func (r *SeiNodeGroupReconciler) deletePDB(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
    pdb := &policyv1.PodDisruptionBudget{}
    err := r.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: group.Namespace}, pdb)
    if apierrors.IsNotFound(err) {
        return nil
    }
    if err != nil {
        return err
    }
    if metav1.IsControlledBy(pdb, group) {
        return r.Delete(ctx, pdb)
    }
    return nil
}
```

#### SeiNodeGroup Controller: handleDeletion — PDB under Retain policy

The PDB must be handled in `handleDeletion` for `DeletionPolicy=Retain`. Without this, the owner reference causes cascade deletion, removing disruption protection from retained pods:

```go
func (r *SeiNodeGroupReconciler) handleDeletion(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) (ctrl.Result, error) {
    // ... existing status patch ...

    if policy == seiv1alpha1.DeletionPolicyRetain {
        // ... existing orphan calls ...
        if err := r.orphanPDB(ctx, group); err != nil {
            return ctrl.Result{}, fmt.Errorf("orphaning PDB: %w", err)
        }
    } else {
        // ... existing delete calls ...
        // PDB is cascade-deleted via owner reference, no explicit delete needed
    }

    // ... existing finalizer removal ...
}

func (r *SeiNodeGroupReconciler) orphanPDB(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
    pdb := &policyv1.PodDisruptionBudget{}
    err := r.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: group.Namespace}, pdb)
    if apierrors.IsNotFound(err) {
        return nil
    }
    if err != nil {
        return err
    }
    if !metav1.IsControlledBy(pdb, group) {
        return nil
    }
    patch := client.MergeFrom(pdb.DeepCopy())
    removeOwnerRef(pdb, group)
    return r.Patch(ctx, pdb, patch)
}
```

The `reconcilePDB` call is added to `Reconcile()` in `controller.go` between `reconcileSeiNodes` and `reconcileNetworking`.

RBAC marker addition:

```go
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
```

`SetupWithManager` addition:

```go
Owns(&policyv1.PodDisruptionBudget{}).
```

#### SeiNodeGroup Controller: nodes.go update field propagation

Add `scheduling` to the narrow set of fields that `ensureSeiNode` propagates on update. In Phase 1 this is a no-op (both sides are nil), but prepares for Phase 2:

```go
// In ensureSeiNode, after PodLabels comparison:
// Use apiequality.Semantic.DeepEqual (not reflect.DeepEqual) to correctly
// handle nil vs empty slice equivalence after DeepCopy / JSON round-trip.
if !apiequality.Semantic.DeepEqual(existing.Spec.Scheduling, desired.Spec.Scheduling) {
    existing.Spec.Scheduling = desired.Spec.Scheduling
    updated = true
}
```

Import: `apiequality "k8s.io/apimachinery/pkg/api/equality"`

### Phase 2 Changes (summary)

1. Add `PlacementConfig`, `TopologySpreadConfig`, `PDBConfig` types to `api/v1alpha1/placement_types.go`
2. Add `Scheduling *SchedulingConfig` to `SeiNodeSpec`
3. Add `Placement *PlacementConfig` to `SeiNodeGroupSpec`
4. In `generateSeiNode`, resolve placement config into `SeiNode.spec.scheduling.topologySpreadConstraints`
5. In `buildNodePodSpec`: check `node.Spec.Scheduling` FIRST; if non-nil, use directly and skip `sei.io/group` inference. This prevents duplicate constraints.
6. In `reconcilePDB`, read `maxUnavailable` from `placement.podDisruptionBudget` if set
7. Add `scheduling` to `ensureSeiNode` field propagation (already wired in Phase 1, now active)
8. Regenerate CRDs with `controller-gen`
9. Add unit tests for all new paths
10. Add webhook validation: `maxUnavailable` cannot exceed `replicas - 1`; warn if `DoNotSchedule` + `replicas > number_of_cluster_zones`

## Error Handling

| Error Case | Detection | Surface | Operator Action |
|-----------|-----------|---------|-----------------|
| Pod stuck Pending due to topology constraint | Pod has no node assignment after startup timeout | `kubectl describe pod` shows `FailedScheduling` with topology spread violation | Scale the Karpenter NodePool to allow more zones, or relax spread to `ScheduleAnyway` |
| PDB blocks node drain | Karpenter or kubectl drain blocked | `kubectl get pdb` shows `disruptionsAllowed: 0` | Wait for more replicas to be ready, or temporarily delete the PDB for emergency maintenance |
| PDB blocks during scale-up | Scaling from 1 to N, only 1 pod Ready | `kubectl get pdb` shows `disruptionsAllowed: 0` until new pods Ready | Expected behavior — disruptions blocked until new pods complete multi-hour startup. Document in runbooks. |
| PVC zone mismatch on reschedule | Pod scheduled to zone A, PVC bound to zone B | Pod Pending with `volume node affinity conflict` | Fundamental Kubernetes limitation — RWO PVCs are zone-pinned. Requires PVC migration or recreation. |
| PVC zone pinning after scale-down/up | RetainOnDelete PVC persists zone-bound; scale-up forces pod back to old zone | Pod lands in original PVC zone regardless of current spread | Spread may degrade over scale-down/up cycles with RetainOnDelete. Delete PVCs explicitly if AZ rebalancing is needed. |
| Topology spread ignored by scheduler | Cluster has only 1 zone available | All pods land in same zone; no error surfaced | Add nodes in additional zones to the Karpenter NodePool |
| StorageClass uses Immediate binding | PVC binds before scheduler evaluates topology | Pods may cluster in one zone despite spread constraint | Ensure StorageClass uses `volumeBindingMode: WaitForFirstConsumer` |
| Suboptimal initial spread (ScheduleAnyway) | Karpenter batch-provisions to one zone | `kubectl get pods -o wide` shows imbalanced AZ distribution | Permanent with `do-not-disrupt`; recreate pods or use `DoNotSchedule` in Phase 2 |
| SSA triggers pod rolling restart | Adding TopologySpreadConstraints to pod template | Existing pods restart on next reconcile (30s) | Coordinate Phase 1 rollout during maintenance window for groups with multi-hour startup |

## Test Specification

### Phase 1 Unit Tests

#### Test: TopologySpread_GroupedNode_InjectsConstraint

- **Setup**: Create SeiNode with `PodLabels: {"sei.io/group": "rpc-group"}`
- **Action**: Call `generateNodeStatefulSet(node, DefaultPlatformConfig())`
- **Expected**: `sts.Spec.Template.Spec.TopologySpreadConstraints` has length 1, with `MaxSkew=1`, `TopologyKey="topology.kubernetes.io/zone"`, `WhenUnsatisfiable=ScheduleAnyway`, and `LabelSelector` matching `sei.io/group: rpc-group`

#### Test: TopologySpread_StandaloneNode_NoConstraint

- **Setup**: Create SeiNode with no `sei.io/group` in PodLabels
- **Action**: Call `generateNodeStatefulSet(node, DefaultPlatformConfig())`
- **Expected**: `sts.Spec.Template.Spec.TopologySpreadConstraints` is nil

#### Test: TopologySpread_EmptyGroupLabel_NoConstraint

- **Setup**: Create SeiNode with `PodLabels: {"sei.io/group": ""}`
- **Action**: Call `generateNodeStatefulSet(node, DefaultPlatformConfig())`
- **Expected**: `sts.Spec.Template.Spec.TopologySpreadConstraints` is nil

#### Test: PDB_MultiReplica_Created

- **Setup**: Create SeiNodeGroup with `replicas: 3`
- **Action**: Reconcile and list PodDisruptionBudgets in the namespace
- **Expected**: PDB exists with name matching group name, `maxUnavailable: 1`, selector `sei.io/group: {name}`, owner reference to the group, `ConditionPDBReady=True`

#### Test: PDB_SingleReplica_NotCreated

- **Setup**: Create SeiNodeGroup with `replicas: 1`
- **Action**: Reconcile and list PodDisruptionBudgets
- **Expected**: No PDB exists for this group, no `ConditionPDBReady` in status

#### Test: PDB_ScaleDownToOne_Deleted

- **Setup**: Create SeiNodeGroup with `replicas: 3`, reconcile (PDB created). Then update to `replicas: 1`.
- **Action**: Reconcile again
- **Expected**: PDB is deleted, `ConditionPDBReady` removed from status

#### Test: PDB_GroupDeletion_Cascades

- **Setup**: Create SeiNodeGroup with `replicas: 3`, reconcile (PDB created). Delete the group with `DeletionPolicy=Delete`.
- **Action**: Observe PDB
- **Expected**: PDB is garbage collected via owner reference

#### Test: PDB_GroupDeletion_RetainOrphans

- **Setup**: Create SeiNodeGroup with `replicas: 3`, `DeletionPolicy=Retain`, reconcile (PDB created). Delete the group.
- **Action**: Observe PDB
- **Expected**: PDB survives with owner reference removed, continues protecting retained pods

### Phase 2 Unit Tests

#### Test: Placement_CustomMaxSkew

- **Setup**: SeiNodeGroup with `placement.topologySpread.maxSkew: 2`
- **Action**: Reconcile, inspect generated SeiNode's `spec.scheduling`
- **Expected**: TopologySpreadConstraint has `MaxSkew=2`

#### Test: Placement_DoNotSchedule

- **Setup**: SeiNodeGroup with `placement.topologySpread.whenUnsatisfiable: DoNotSchedule`
- **Action**: Reconcile, inspect pod spec
- **Expected**: TopologySpreadConstraint has `WhenUnsatisfiable=DoNotSchedule`

#### Test: Placement_Disabled

- **Setup**: SeiNodeGroup with `placement.topologySpread.disabled: true`
- **Action**: Reconcile, inspect pod spec
- **Expected**: No topology spread constraints on the pod

#### Test: Placement_CustomPDB

- **Setup**: SeiNodeGroup with `replicas: 5`, `placement.podDisruptionBudget.maxUnavailable: 2`
- **Action**: Reconcile, inspect PDB
- **Expected**: PDB has `maxUnavailable: 2`

#### Test: Placement_PDBDisabled

- **Setup**: SeiNodeGroup with `placement.podDisruptionBudget.disabled: true`
- **Action**: Reconcile
- **Expected**: No PDB created, no `ConditionPDBReady`

#### Test: Placement_Nil_DefaultBehavior

- **Setup**: SeiNodeGroup with no `placement` field, `replicas: 3`
- **Action**: Reconcile
- **Expected**: Default topology spread (MaxSkew=1, ScheduleAnyway) and default PDB (maxUnavailable=1)

#### Test: Phase1_To_Phase2_Transition_NoDuplicateConstraints

- **Setup**: Create SeiNode with both `PodLabels: {"sei.io/group": "rpc"}` AND explicit `Scheduling.TopologySpreadConstraints` set
- **Action**: Call `generateNodeStatefulSet(node, DefaultPlatformConfig())`
- **Expected**: Uses `Scheduling.TopologySpreadConstraints` directly; no duplicate constraints from label inference

### Sample Manifest (Phase 2)

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodeGroup
metadata:
  name: pacific-1-rpc
  namespace: default
spec:
  replicas: 3
  placement:
    topologySpread:
      maxSkew: 1
      whenUnsatisfiable: DoNotSchedule
    podDisruptionBudget:
      maxUnavailable: 1
  template:
    spec:
      chainId: pacific-1
      image: ghcr.io/sei-protocol/sei:v6.3.0
      fullNode:
        peers:
          - ec2Tags:
              region: eu-central-1
              tags:
                ChainIdentifier: pacific-1
  networking:
    service:
      type: ClusterIP
      ports: [rpc, evm-rpc, evm-ws, grpc, metrics]
    gateway:
      parentRef:
        name: sei-gateway
        namespace: istio-system
      hostnames:
        - rpc.pacific-1.sei.io
```

## Deployment

### Phase 1

- All changes target `sei-node-controller-networking` (the production binary)
- SeiNode controller: `resources.go` (inject topology spread)
- SeiNodeGroup controller: new `pdb.go`, `controller.go` (add `reconcilePDB`, `orphanPDB`, `Owns` for PDB)
- Shared: `api/v1alpha1/constants.go` (exported label constants), `nodegroup/labels.go` (reference shared constants)
- No CRD regeneration required
- **Migration**: Existing StatefulSets created by this controller already have `sei.io/group` in their selector and pod template labels. Adding `TopologySpreadConstraints` only changes the pod template, which SSA updates cleanly. However, this triggers a RollingUpdate of existing pods. For seid pods with multi-hour startup, coordinate the rollout during a maintenance window.
- **Rollout**: Deploy updated controller image. Existing StatefulSets are updated via Server-Side Apply on next reconcile cycle (30s).

### Phase 2

- CRD changes: run `controller-gen` to regenerate `sei.io_seinodegroups.yaml` and `sei.io_seinodes.yaml`
- Requires CRD apply before controller deploy (new fields must exist before controller writes them)
- **Rollout order**: CRDs → controller image
- Backward compatible — `placement: nil` produces identical behavior to Phase 1 defaults
- During the brief rollout window, the SeiNodeGroup controller may write `spec.scheduling` before the SeiNode controller is updated to read it. This is safe — the old controller falls back to Phase 1 label inference.

## Deferred (Do Not Build)

| Feature | Rationale |
|---------|-----------|
| Custom `topologyKey` (e.g., `kubernetes.io/hostname` for per-node spread) | AZ spread covers the critical failure domain. Per-node spread is Karpenter's job via resource packing. |
| Pod affinity (co-locate with other workloads) | No business need identified. Sei-node pods run on dedicated node pools. |
| Custom tolerations / node selectors on CRD | Covered by `PlatformConfig` env vars. Per-group overrides add API surface without clear need. |
| Rack-aware topology | EKS doesn't expose rack topology. Revisit if moving to bare metal. |
| Weighted spread across zones | `MaxSkew=1` is sufficient. Weighted policies add complexity without proportional value. |
| Automatic PVC migration on zone failure | Fundamental Kubernetes limitation. Requires snapshot + restore workflow, not a scheduling feature. |
| Multi-cluster scheduling | Out of scope for single-cluster SeiNodeGroup. |
| Percentage-based PDB values | Ambiguous at small replica counts (50% of 3 = 1.5). Integer-only simplifies validation and avoids confusion. |

## Decision Log

| # | Decision | Rationale | Reversibility |
|---|----------|-----------|---------------|
| 1 | `ScheduleAnyway` as Phase 1 default | Pods stuck Pending is worse than imperfect spread for initial rollout. Users can opt into `DoNotSchedule` in Phase 2. | Two-way: can change default in Phase 2 without migration |
| 2 | PDB owned by SeiNodeGroup (not SeiNode) | PDB spans all pods in the group; creating per-SeiNode PDBs would be incorrect. Owner reference on the group provides cascade cleanup. | Two-way: ownership can be changed by deleting and recreating PDB |
| 3 | `placement` at SeiNodeGroup level, `scheduling` at SeiNode level | Clean separation: group-level intent (placement) vs. pod-level mechanics (scheduling). Mirrors K8s concepts: Deployment has strategy, Pod has scheduling. | One-way: CRD field names become API contract. Names chosen carefully. |
| 4 | Infer topology spread from `sei.io/group` label in Phase 1 | No CRD change, no migration, immediate value. Phase 2 makes it explicit. | Two-way: Phase 2 overrides the inference path |
| 5 | `maxUnavailable: 1` PDB default (not `minAvailable`) | Caps simultaneous disruptions at 1 regardless of group size. `minAvailable: 1` on a 5-replica group allows 4 simultaneous disruptions. | Two-way: config value |
| 6 | PDB orphaned under DeletionPolicy=Retain | Consistent with how SeiNodes and networking resources are orphaned. Retained pods need disruption protection. | Two-way: PDB can be manually deleted after group deletion |
| 7 | `apiequality.Semantic.DeepEqual` for scheduling field comparison | Handles nil vs empty slice equivalence correctly. `reflect.DeepEqual` treats `nil != []T{}`, causing spurious updates on every reconcile after JSON round-trip. | Two-way: implementation detail |
| 8 | Target `sei-node-controller-networking` for all changes | This is the production binary with `PodLabels` support. `sei-k8s-controller`'s `SeiNodeSpec` lacks `PodLabels`. | Two-way: can port to other repo later if needed |

## Cross-Review Resolution Log

### Blocking Concerns Resolved

| Source | Concern | Resolution |
|--------|---------|------------|
| kubernetes-specialist | `sei-k8s-controller` SeiNodeSpec has no PodLabels field — code won't compile | Retargeted all changes to `sei-node-controller-networking` where PodLabels already exists. Added to Dependencies and Deployment sections. |
| kubernetes-specialist | Existing StatefulSet selector mismatch on migration | Clarified that the networking controller already includes `sei.io/group` in selectors. No selector change needed — only pod template changes. Documented RollingUpdate side effect. |
| platform-engineer | PDB not handled in handleDeletion for Retain policy | Added `orphanPDB` to the Retain path in handleDeletion. Added test case `PDB_GroupDeletion_RetainOrphans`. |
| platform-engineer | `reflect.DeepEqual` nil vs empty slice causes spurious updates | Changed to `apiequality.Semantic.DeepEqual`. Added to Decision Log. |

### Non-Blocking Suggestions Incorporated

| Source | Suggestion | Disposition |
|--------|-----------|-------------|
| kubernetes-specialist | Change PDB from `minAvailable:1` to `maxUnavailable:1` | **Adopted**. Changed throughout. Better caps simultaneous disruptions. |
| kubernetes-specialist | Document Karpenter `do-not-disrupt` interaction | **Adopted**. Added note to Phase 1 Interface Specification. |
| kubernetes-specialist | WaitForFirstConsumer StorageClass prerequisite | **Adopted**. Added to Dependencies and Error Handling. |
| kubernetes-specialist | Scale-up transient PDB blocking | **Adopted**. Added note to PDB section and Error Handling table. |
| kubernetes-specialist | RetainOnDelete PVC zone pinning | **Adopted**. Added to Error Handling table. |
| kubernetes-specialist | Observability for zone spread violations | **Deferred to Phase 2**. Could emit events when actual pod distribution violates MaxSkew. |
| kubernetes-specialist | DoNotSchedule as Phase 2 default | **Not adopted**. Keeping ScheduleAnyway default with DoNotSchedule available. Operators who need hard guarantees opt in explicitly. |
| kubernetes-specialist | SSA triggers rolling restart | **Adopted**. Added to Deployment section with maintenance window guidance. |
| platform-engineer | Add ConditionPDBReady status condition | **Adopted**. Added to Interface Specification, State Model, all PDB test cases. |
| platform-engineer | Add `//nolint:staticcheck` on SSA Patch | **Adopted**. Added to PDB reconciliation code. |
| platform-engineer | Update labels.go to reference shared constants | **Adopted**. Added to Internal Design. |
| platform-engineer | Phase 1→2 transition test case | **Adopted**. Added `Phase1_To_Phase2_Transition_NoDuplicateConstraints` test. |
| platform-engineer | Document int-only PDB restriction | **Adopted**. Added to PDBConfig godoc and Deferred table. |
| platform-engineer | Sample manifest | **Adopted**. Added sample Kustomize manifest for Phase 2. |

## Phased Execution Plan

### Phase 1 — Ship immediately (no API change)

**Estimated effort**: 1-2 days

1. Add exported label constants to `api/v1alpha1/constants.go` (`LabelGroup`, `LabelGroupOrdinal`, `LabelNode`)
2. Update `nodegroup/labels.go` to reference shared constants
3. Modify `buildNodePodSpec` in `resources.go` to inject topology spread when `sei.io/group` is present in PodLabels
4. Add unit tests for topology spread injection (3 test cases)
5. Add `pdb.go` to `internal/controller/nodegroup/` with `reconcilePDB`, `deletePDB`, `orphanPDB`
6. Add `ConditionPDBReady` constant to status condition types
7. Wire `reconcilePDB` into `controller.go` reconcile loop, `orphanPDB` into `handleDeletion` Retain path
8. Add RBAC markers and `Owns(&policyv1.PodDisruptionBudget{})` to `SetupWithManager`
9. Add unit tests for PDB lifecycle (5 test cases including Retain orphan)
10. Add `apiequality.Semantic.DeepEqual` comparison for `Scheduling` field in `ensureSeiNode` (no-op in Phase 1)

### Phase 2 — Fast follow (CRD change)

**Estimated effort**: 2-3 days

1. Add `placement_types.go` with `PlacementConfig`, `TopologySpreadConfig`, `PDBConfig`
2. Add `Scheduling *SchedulingConfig` to `SeiNodeSpec`
3. Add `Placement *PlacementConfig` to `SeiNodeGroupSpec`
4. Update `generateSeiNode` to resolve placement → scheduling
5. Update `buildNodePodSpec` to check `node.Spec.Scheduling` FIRST, skip label inference when non-nil
6. Update `reconcilePDB` to read from placement config
7. Regenerate CRDs with `controller-gen`
8. Add webhook validation for `maxUnavailable` bounds
9. Add unit tests for all new paths (7 test cases including Phase 1→2 transition)

### Phase 3 — Future (deferred)

Full scheduling override: affinity, tolerations, node selector exposed on the CRD. Only build when a concrete use case requires per-group scheduling beyond topology spread.
