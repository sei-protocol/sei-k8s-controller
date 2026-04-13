# InPlace Update Strategy for SeiNodeDeployment

## Summary

The InPlace update strategy is a lightweight, operator-driven deployment mode that propagates spec changes (image, sidecar image) directly to existing SeiNode resources without creating entrant nodes or performing blue-green traffic switching. The controller's role is confined to change propagation, health monitoring, and status reporting.

This design also formalizes `updateStrategy` as a required field on SeiNodeDeployment. The previous implicit nil path (fire-and-forget in-place updates with no tracking) is removed -- all deployments must declare an explicit strategy: `InPlace`, `BlueGreen`, or `HardFork`.

The critical technical challenge is the sidecar mark-ready gate: when a pod restarts after an image update, the sidecar starts fresh and returns 503 from `/v0/healthz`, blocking seid startup indefinitely. This design solves that by having the SeiNode controller's Running phase reconciler submit `mark-ready` unconditionally on every reconcile.

## Motivation

Today, the controller supports two explicit update strategies and one implicit path:

1. **BlueGreen** -- full blue-green with entrant nodes, catch-up wait, traffic switch, and incumbent teardown
2. **HardFork** -- blue-green variant with halt-height coordination for chain upgrades
3. **nil (no strategy)** -- `ensureSeiNode` patches child SeiNodes in-place with no orchestration and no status tracking

The nil path was a placeholder to avoid making a decision on in-place updates. It provided no rollout status, no per-node convergence tracking, and no way for an operator to answer "did the rollout succeed?" Now that InPlace is formalized, the nil path is removed.

InPlace is the right strategy when:

- The operator has already waited for the chain to reach upgrade height (or the change is non-disruptive, like a sidecar bump)
- Creating fresh nodes is unnecessary or wasteful (the existing PVCs hold valid state)
- The operator wants controller-assisted status reporting without the cost and complexity of blue-green

## Conditions-Driven Reconciliation

This design introduces a pattern that should generalize across all strategies: **conditions as the coordination layer between reconciliation and plan generation**.

### The Pattern

1. **Reconciler detects diffs.** The reconciliation loop compares the current spec against observed state and identifies actionable changes.
2. **Reconciler sets conditions.** Detected changes are expressed as Kubernetes conditions on the SeiNodeDeployment (e.g., `RolloutInProgress`).
3. **Plan generation reads conditions.** The planner inspects conditions to derive which task sequences need to execute.
4. **Conditions guard concurrent mutations.** A condition like `RolloutInProgress` prevents the reconciler from applying additional spec changes until the current rollout completes.

### Design Principles

- **Keep condition vocabulary small.** As BlueGreen and HardFork migrate to this pattern, do NOT add strategy-specific conditions. Let `RolloutStatus.Strategy` carry the variant. Every new condition type is a new alert rule for operators.
- **`RolloutStatus` is the real state machine; conditions are derived.** The `RolloutStatus` struct drives controller behavior. The `RolloutInProgress` condition is an API-surface convenience for `kubectl wait`, external consumers, and alerting. This matches how upstream controllers (Deployment, StatefulSet) use conditions as derived signals, not primary drivers.
- **Simultaneous rollout is intentional.** Chain upgrades are coordinated halts — all nodes must move to the new binary at the same height. Sequential rollout provides no safety benefit and creates split-brain risk. This deviates from StatefulSet's default `OrderedReady` but is correct for the domain.

### For InPlace

The InPlace strategy uses a single condition to coordinate:

- **`RolloutInProgress`** -- set when the reconciler detects a templateHash divergence with an InPlace strategy. This condition:
  - Signals that an image change was detected and needs to be actioned
  - Guards against concurrent spec changes to the child SeiNodes
  - Is cleared when all nodes converge to Running on the new image

For InPlace, the "plan" is simple: propagate the image change to child SeiNodes and monitor convergence. No task plan is created -- the condition itself drives the behavior. The reconciler checks `RolloutInProgress`, propagates changes if needed, and monitors health until it can clear the condition.

### Future Direction

This pattern extends naturally to BlueGreen and HardFork. Today those strategies write `DeploymentStatus` directly and use `PlanInProgress` as an informal guard. Over time, the deployment planner can be driven by conditions rather than status structs, making the reconciliation-to-planning boundary explicit and extensible.

## How It Works

### InPlace Rollout Lifecycle

1. **Engineer updates manifests.** The engineer changes `spec.template.spec.image` (or sidecar image) on the SeiNodeDeployment and applies the change (via GitOps push, `kubectl apply`, etc.). The engineer is responsible for timing -- the controller does not validate block height.

2. **templateHash diverges; condition set.** The SeiNodeDeployment controller's `reconcileSeiNodes` detects that the current `templateHash` differs from `status.templateHash`. With `updateStrategy.type == InPlace`, it sets the `RolloutInProgress` condition and writes a `RolloutStatus` to `status.rollout`.

3. **ensureSeiNode propagates changes.** The `ensureSeiNode` loop patches each child SeiNode's image. All nodes are updated simultaneously -- chain upgrades are coordinated halts where sequential rollout provides no safety benefit.

4. **SeiNode controller converges StatefulSets.** Each SeiNode's `reconcileRunning` calls `reconcileNodeStatefulSet`, which applies the updated StatefulSet spec via SSA. Kubernetes detects the pod template change and terminates the old pod, scheduling a new one with the updated image.

5. **Sidecar restarts fresh; controller submits mark-ready.** The new pod's sidecar starts clean. The `reconcileRunning` method submits `mark-ready` unconditionally (it is idempotent). The sidecar flips to ready, `/v0/healthz` returns 200, and seid starts via the wait wrapper.

6. **SeiNodeDeployment controller monitors convergence.** On each reconcile, the controller checks the rollout: for each node, it reads the child SeiNode's phase and pod readiness. When all nodes are Running with ready pods, the rollout is complete. The controller clears `RolloutInProgress`, clears `status.rollout`, and updates `status.templateHash`.

7. **Failure detection.** If a node's pod enters CrashLoopBackOff or the SeiNode transitions to Failed, the rollout status reflects this per-node. The controller does NOT auto-rollback -- blockchain rollback after a chain upgrade would leave the node unable to process new blocks. The engineer inspects the status and decides: push a fix, revert the image, or investigate.

## CRD Changes

### UpdateStrategy is now required

```go
type SeiNodeDeploymentSpec struct {
    // UpdateStrategy controls how changes to the template are rolled out
    // to child SeiNodes. Every deployment must declare an explicit strategy.
    // +kubebuilder:validation:Required
    UpdateStrategy UpdateStrategy `json:"updateStrategy"`
    // ...
}
```

### UpdateStrategyType enum

```go
// +kubebuilder:validation:Enum=InPlace;BlueGreen;HardFork
type UpdateStrategyType string

const (
    UpdateStrategyInPlace   UpdateStrategyType = "InPlace"
    UpdateStrategyBlueGreen UpdateStrategyType = "BlueGreen"
    UpdateStrategyHardFork  UpdateStrategyType = "HardFork"
)
```

### UpdateStrategy struct

```go
type UpdateStrategy struct {
    Type UpdateStrategyType `json:"type"`

    // HardFork configures blue-green deployment at a specific block height.
    // Required when type is HardFork.
    // +optional
    HardFork *HardForkStrategy `json:"hardFork,omitempty"`
}
```

InPlace requires no sub-config. The engineer owns timing entirely.

### Migration: zero-value handling

Making `updateStrategy` required won't break existing stored objects (CRD validation only fires on create/update). However, the next reconcile of an existing resource without the field will see a zero-value `UpdateStrategy{Type: ""}`. During the migration window, `detectDeploymentNeeded` treats an empty `Type` as `InPlace` and logs a warning:

```go
strategyType := group.Spec.UpdateStrategy.Type
if strategyType == "" {
    log.FromContext(ctx).Info("updateStrategy.type is empty, treating as InPlace — update the manifest")
    strategyType = seiv1alpha1.UpdateStrategyInPlace
}
```

This handler is removed in a subsequent release once all manifests have been updated.

### RolloutStatus (unified type replacing DeploymentStatus)

```go
// RolloutStatus tracks an in-progress rollout. Used by all strategies
// to report per-node convergence state.
type RolloutStatus struct {
    // Strategy is the strategy type driving this rollout.
    Strategy UpdateStrategyType `json:"strategy"`

    // TargetHash is the templateHash being rolled out to.
    TargetHash string `json:"targetHash"`

    // StartedAt is when the rollout was first detected.
    StartedAt metav1.Time `json:"startedAt"`

    // Nodes reports per-node rollout state.
    // +listType=map
    // +listMapKey=name
    Nodes []RolloutNodeStatus `json:"nodes"`

    // IncumbentNodes lists the names of the currently active SeiNode
    // resources. Only populated for BlueGreen and HardFork strategies.
    // +optional
    IncumbentNodes []string `json:"incumbentNodes,omitempty"`

    // EntrantNodes lists the names of the new SeiNode resources being
    // created. Only populated for BlueGreen and HardFork strategies.
    // +optional
    EntrantNodes []string `json:"entrantNodes,omitempty"`

    // IncumbentRevision identifies the generation of the currently live nodes.
    // Only populated for BlueGreen and HardFork strategies.
    // +optional
    IncumbentRevision string `json:"incumbentRevision,omitempty"`

    // EntrantRevision identifies the generation of the new nodes.
    // Only populated for BlueGreen and HardFork strategies.
    // +optional
    EntrantRevision string `json:"entrantRevision,omitempty"`
}

// RolloutNodeStatus tracks a single node's convergence during a rollout.
type RolloutNodeStatus struct {
    // Name is the SeiNode resource name.
    Name string `json:"name"`

    // Ready is true when the node is Running with a ready pod.
    Ready bool `json:"ready"`

    // Phase is the SeiNode's current phase.
    Phase SeiNodePhase `json:"phase,omitempty"`
}
```

### SeiNodeDeploymentStatus changes

Replace `Deployment *DeploymentStatus` with `Rollout *RolloutStatus`:

```go
type SeiNodeDeploymentStatus struct {
    // ... existing fields ...

    // Rollout tracks an in-progress rollout across all strategy types.
    // Nil when no rollout is active.
    // +optional
    Rollout *RolloutStatus `json:"rollout,omitempty"`
}
```

The existing `DeploymentStatus` type and `Deployment` field are removed. BlueGreen and HardFork are migrated to use `RolloutStatus` with the `IncumbentNodes`, `EntrantNodes`, and `EntrantRevision` fields.

### Conditions

New condition type:

```go
const (
    // ConditionRolloutInProgress indicates a rollout is active.
    // Set when a templateHash divergence is detected. Cleared when
    // all nodes converge or the rollout is superseded.
    ConditionRolloutInProgress = "RolloutInProgress"
)
```

## Controller Changes

### `detectDeploymentNeeded` (nodedeployment/nodes.go)

Refactored to set the `RolloutInProgress` condition and write a unified `RolloutStatus`:

```go
func (r *SeiNodeDeploymentReconciler) detectDeploymentNeeded(group *seiv1alpha1.SeiNodeDeployment) {
    if hasConditionTrue(group, seiv1alpha1.ConditionRolloutInProgress) {
        return
    }
    if group.Status.TemplateHash == "" {
        return
    }

    currentHash := templateHash(&group.Spec.Template.Spec)
    if currentHash == group.Status.TemplateHash {
        return
    }

    // Set the condition — this is the signal for plan generation
    meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
        Type:    seiv1alpha1.ConditionRolloutInProgress,
        Status:  metav1.ConditionTrue,
        Reason:  "TemplateChanged",
        Message: fmt.Sprintf("templateHash changed from %s to %s", group.Status.TemplateHash, currentHash),
    })

    group.Status.Rollout = &seiv1alpha1.RolloutStatus{
        Strategy:   group.Spec.UpdateStrategy.Type,
        TargetHash: currentHash,
        StartedAt:  metav1.Now(),
        Nodes:      buildRolloutNodes(group),
    }

    // For BlueGreen/HardFork, also populate entrant/incumbent fields
    switch group.Spec.UpdateStrategy.Type {
    case seiv1alpha1.UpdateStrategyBlueGreen, seiv1alpha1.UpdateStrategyHardFork:
        group.Status.Rollout.IncumbentNodes = group.Status.IncumbentNodes
        group.Status.Rollout.EntrantNodes = planner.EntrantNodeNames(group)
        group.Status.Rollout.EntrantRevision = planner.EntrantRevision(group)
    }
}
```

### `ensureSeiNode` (nodedeployment/nodes.go)

No changes needed. The existing in-place propagation already handles image and sidecar updates. When `RolloutInProgress` is true and the strategy is InPlace, `ensureSeiNode` runs normally (no plan blocks it). For BlueGreen/HardFork, the existing plan machinery takes over.

### Rollout status reconciliation (nodedeployment/status.go)

```go
func (r *SeiNodeDeploymentReconciler) reconcileRolloutStatus(
    group *seiv1alpha1.SeiNodeDeployment,
    nodes []seiv1alpha1.SeiNode,
) {
    if group.Status.Rollout == nil {
        return
    }
    if group.Status.Rollout.Strategy != seiv1alpha1.UpdateStrategyInPlace {
        return // BlueGreen/HardFork rollout convergence is tracked by the plan
    }

    nodePhaseMap := make(map[string]seiv1alpha1.SeiNodePhase, len(nodes))
    for i := range nodes {
        nodePhaseMap[nodes[i].Name] = nodes[i].Status.Phase
    }

    allReady := true
    for i := range group.Status.Rollout.Nodes {
        rn := &group.Status.Rollout.Nodes[i]
        phase := nodePhaseMap[rn.Name]
        rn.Phase = phase
        rn.Ready = phase == seiv1alpha1.PhaseRunning
        if !rn.Ready {
            allReady = false
        }
    }

    if allReady {
        group.Status.TemplateHash = group.Status.Rollout.TargetHash
        group.Status.ObservedGeneration = group.Generation
        group.Status.Rollout = nil
        meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
            Type:   seiv1alpha1.ConditionRolloutInProgress,
            Status: metav1.ConditionFalse,
            Reason: "RolloutComplete",
        })
        return
    }

    // Escalate stalled rollouts. If any node has been non-ready for longer
    // than the stall threshold, update the condition reason to Stalled.
    // This provides a durable signal for alerting (PagerDuty, Grafana)
    // unlike events which are ephemeral.
    const stallThreshold = 10 * time.Minute
    if time.Since(group.Status.Rollout.StartedAt.Time) > stallThreshold {
        meta.SetStatusCondition(&group.Status.Conditions, metav1.Condition{
            Type:    seiv1alpha1.ConditionRolloutInProgress,
            Status:  metav1.ConditionTrue,
            Reason:  "Stalled",
            Message: fmt.Sprintf("rollout stalled: not all nodes ready after %s", stallThreshold),
        })
    }
}
```

### `computeGroupPhase` (nodedeployment/status.go)

```go
if hasConditionTrue(group, seiv1alpha1.ConditionRolloutInProgress) {
    return seiv1alpha1.GroupPhaseUpgrading
}
```

## Sidecar Mark-Ready Resolution

### The Problem

The sidecar starts fresh on every pod restart. Its `/v0/healthz` endpoint returns 503 until a `mark-ready` task is submitted. The `mark-ready` task is part of the initialization plan, which only runs during the Initializing phase. Once a SeiNode reaches Running, the controller never re-submits `mark-ready`.

After an in-place image update, the StatefulSet rolls the pod. The new sidecar starts, binds its port, and returns 503 from `/v0/healthz`. The seid container's wait wrapper polls `/v0/healthz` and blocks forever.

### Solution: Controller Re-submits Mark-Ready

The `reconcileRunning` method submits `mark-ready` unconditionally on every reconcile when the sidecar is reachable. The `mark-ready` task is fire-and-forget and idempotent -- submitting it to an already-ready sidecar is a no-op.

```go
func (r *SeiNodeReconciler) reconcileRunning(ctx context.Context, node *seiv1alpha1.SeiNode) (ctrl.Result, error) {
    if err := r.reconcileNodeStatefulSet(ctx, node); err != nil {
        return ctrl.Result{}, fmt.Errorf("reconciling statefulset: %w", err)
    }
    if err := r.reconcileNodeService(ctx, node); err != nil {
        return ctrl.Result{}, fmt.Errorf("reconciling service: %w", err)
    }

    sc := r.buildSidecarClient(node)
    if sc == nil {
        sidecarUnreachableTotal.WithLabelValues(node.Namespace, node.Name).Inc()
        log.FromContext(ctx).Info("sidecar not reachable, will retry")
        return ctrl.Result{RequeueAfter: statusPollInterval}, nil
    }

    r.ensureMarkReady(ctx, node, sc)

    return r.reconcileRuntimeTasks(ctx, node, sc)
}

func (r *SeiNodeReconciler) ensureMarkReady(ctx context.Context, node *seiv1alpha1.SeiNode, sc task.SidecarClient) {
    req := sidecar.TaskRequest{Type: sidecar.TaskTypeMarkReady}
    if _, err := sc.SubmitTask(ctx, req); err != nil {
        log.FromContext(ctx).V(1).Info("mark-ready submission failed", "error", err)
    }
}
```

This approach:
- Requires no sidecar changes
- Solves the problem for ALL strategies, not just InPlace
- Keeps the sidecar stateless by design
- Is safe because mark-ready is idempotent

### Open Question: Block Height Sourcing

The controller currently has no reliable way to source block height from running nodes. The sidecar does not track block progress, and seid panics rather than gracefully reporting it has reached an upgrade height. Until seid supports a graceful halt-at-height that reports its state (rather than panicking), any mechanism that relies on the controller knowing block height is unreliable.

This means:
- No upgrade height gating for InPlace (the engineer owns timing)
- No height-based validation before applying changes
- The `ensureMarkReady` approach has an inherent race: the sidecar marks ready, seid starts, and if seid hits an upgrade height it cannot process, it crashes. This is acceptable because the controller will observe the crash loop and report it via rollout status.

A future improvement would be seid supporting `--halt-height` with a graceful exit (exit code 0, writes state) rather than a panic. Combined with sidecar height reporting, this would enable controller-side gating. This is out of scope for this design.

### Future Improvement: Sidecar Self-Detection

As a follow-up, the sidecar could detect existing chain data on startup and skip the ready gate entirely. If the sidecar finds `$SEI_HOME/data/` populated, it knows this is a restart, not a fresh init, and can serve 200 on `/v0/healthz` immediately. This eliminates the controller round-trip but requires a sidecar release.

### Why NOT transition back to Initializing?

Re-running the full initialization plan (snapshot restore, config apply, genesis configure, peer discovery) is wasteful and risky for an image update. The PVC already has valid data. `mark-ready` is the only task that needs to re-run.

## Status Reporting

### During rollout

```yaml
status:
  phase: Upgrading
  rollout:
    strategy: InPlace
    targetHash: "a1b2c3d4e5f67890"
    startedAt: "2026-04-13T10:00:00Z"
    nodes:
    - name: syncer-0-0
      ready: true
      phase: Running
    - name: archive-0-0
      ready: false
      phase: Running
  conditions:
  - type: RolloutInProgress
    status: "True"
    reason: TemplateChanged
    message: "templateHash changed from abc123 to a1b2c3d4e5f67890"
  - type: NodesReady
    status: "False"
    reason: NodesInitializing
    message: "1/2 nodes ready (1 initializing)"
```

### After completion

```yaml
status:
  phase: Ready
  templateHash: "a1b2c3d4e5f67890"
  rollout: null
  conditions:
  - type: RolloutInProgress
    status: "False"
    reason: RolloutComplete
  - type: NodesReady
    status: "True"
    reason: AllNodesReady
    message: "2/2 nodes ready"
```

### Events

| Event | Type | Reason | When |
|---|---|---|---|
| Rollout started | Normal | `RolloutStarted` | `RolloutInProgress` condition set to True |
| Rollout complete | Normal | `RolloutComplete` | All nodes converge, condition cleared |
| Rollout stalled | Warning | `RolloutStalled` | `RolloutInProgress` reason transitions to `Stalled` (> 10 min with non-ready nodes) |

## Failure Modes

### New image crashes (CrashLoopBackOff)

The pod enters CrashLoopBackOff. The rollout status shows `ready: false` for the affected node. The group phase stays `Upgrading`. The controller does NOT auto-rollback -- rolling back to the pre-upgrade binary after a chain upgrade means the node cannot process new blocks.

**Recovery:** Fix the image or config, push a new manifest. The controller detects a new hash divergence and updates the existing rollout's `targetHash`.

### Sidecar cannot start

The sidecar init container's `RestartPolicy: Always` causes Kubernetes to restart it. The controller's `buildSidecarClient` returns nil until reachable, and `reconcileRunning` requeues.

### Partial rollout

The rollout status shows per-node state. The group phase is `Upgrading` as long as any node is not ready. The `NodesReady` condition provides a summary.

### Concurrent spec change during rollout

If the operator changes the image again while a rollout is active, `RolloutInProgress` is already true so `detectDeploymentNeeded` is a no-op. However, `ensureSeiNode` still runs and pushes the latest spec. The rollout's `targetHash` is updated to reflect the newest hash on the next detection cycle after the current rollout completes. This is safe because InPlace is purely pass-through.

## File-by-File Changes

| File | Change |
|------|--------|
| `api/v1alpha1/seinodedeployment_types.go` | Add `UpdateStrategyInPlace` to enum. Replace `DeploymentStatus` with unified `RolloutStatus`. Make `UpdateStrategy` required. Add `ConditionRolloutInProgress`. Remove `Deployment` field, add `Rollout` field. |
| `api/v1alpha1/zz_generated.deepcopy.go` | Regenerated |
| `manifests/crd/bases/sei.io_seinodedeployments.yaml` | Regenerated |
| `internal/controller/nodedeployment/nodes.go` | `detectDeploymentNeeded`: set `RolloutInProgress` condition, create unified `RolloutStatus`. Migrate BlueGreen/HardFork to use `RolloutStatus`. |
| `internal/controller/nodedeployment/status.go` | Add `reconcileRolloutStatus`. Extend `computeGroupPhase` for `RolloutInProgress` condition. |
| `internal/controller/nodedeployment/plan.go` | Read `RolloutStatus` instead of `DeploymentStatus` for BlueGreen/HardFork plan generation. |
| `internal/planner/deployment.go` | Read entrant/incumbent from `RolloutStatus` instead of `DeploymentStatus`. |
| `internal/controller/node/controller.go` | `reconcileRunning`: add `ensureMarkReady` call before `reconcileRuntimeTasks`. |
| `internal/controller/node/plan_execution.go` | Add `ensureMarkReady` method. |

## Test Plan

### Unit Tests

| Test | File | Description |
|------|------|-------------|
| `TestDetectDeploymentNeeded_InPlace` | `nodes_test.go` | Hash divergence with InPlace sets `RolloutInProgress` and creates `RolloutStatus` |
| `TestDetectDeploymentNeeded_InPlace_AlreadyActive` | `nodes_test.go` | `RolloutInProgress=True` prevents duplicate detection |
| `TestDetectDeploymentNeeded_BlueGreen_MigratedToRollout` | `nodes_test.go` | BlueGreen creates `RolloutStatus` with entrant/incumbent fields |
| `TestBuildRolloutNodes` | `nodes_test.go` | Creates entries for each incumbent node |
| `TestReconcileRolloutStatus_AllReady` | `status_test.go` | Rollout cleared, templateHash updated, `RolloutInProgress` set to False |
| `TestReconcileRolloutStatus_Partial` | `status_test.go` | Rollout persists with mixed ready/not-ready |
| `TestReconcileRolloutStatus_WithFailedNode` | `status_test.go` | Shows `ready: false, phase: Failed` |
| `TestComputeGroupPhase_RolloutInProgress` | `status_test.go` | Returns `Upgrading` when `RolloutInProgress` is True |
| `TestEnsureMarkReady` | `reconciler_test.go` | Submits mark-ready task via sidecar client |
| `TestReconcileRunning_SubmitsMarkReady` | `reconciler_test.go` | mark-ready called before runtime tasks |

## Implementation Order

1. **ensureMarkReady.** Add to SeiNode controller's `reconcileRunning`. Ships independently -- unblocks pod restarts for all strategies. Highest priority.
2. **CRD types.** Add `InPlace` enum, unified `RolloutStatus`, `ConditionRolloutInProgress`. Make `updateStrategy` required. Remove `DeploymentStatus`. `make manifests generate`.
3. **detectDeploymentNeeded refactor.** Set `RolloutInProgress` condition, write unified `RolloutStatus`. Migrate BlueGreen/HardFork.
4. **Rollout status reconciliation.** Add `reconcileRolloutStatus`, extend `computeGroupPhase`.
5. **Events.** Emit `RolloutStarted`, `RolloutComplete`.
6. **Tests.** Unit tests for each step.

Step 1 is the highest-priority standalone fix -- it resolves the sidecar restart problem that currently blocks all image updates on Running nodes.
