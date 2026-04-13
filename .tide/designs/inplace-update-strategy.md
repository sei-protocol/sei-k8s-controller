# InPlace Update Strategy for SeiNodeDeployment

## Summary

The InPlace update strategy is a lightweight, operator-driven deployment mode that propagates spec changes (image, sidecar, entrypoint) directly to existing SeiNode resources without creating entrant nodes or performing blue-green traffic switching. The controller's role is confined to change propagation, health monitoring, and status reporting. Unlike the nil strategy (no `UpdateStrategy` set), InPlace provides explicit rollout tracking through a `RolloutStatus` on the SeiNodeDeployment, surfacing per-node convergence state so engineers can observe the rollout and detect failures.

The critical technical challenge is the sidecar mark-ready gate: when a pod restarts after an image update, the sidecar starts fresh and returns 503 from `/v0/healthz`, blocking seid startup indefinitely. This design solves that by having the SeiNode controller's Running phase reconciler submit `mark-ready` unconditionally on every reconcile, with a future path for the sidecar to self-resolve on restart.

## Motivation

Today, the controller supports three update paths:

1. **BlueGreen** -- full blue-green with entrant nodes, catch-up wait, traffic switch, and incumbent teardown
2. **HardFork** -- blue-green variant with halt-height coordination for chain upgrades
3. **nil (no strategy)** -- `ensureSeiNode` patches child SeiNodes in-place with no orchestration, no status tracking, and no health monitoring

The nil strategy is fire-and-forget. After pushing the change to child SeiNodes, the deployment controller moves on. There is no rollout status, no per-node convergence tracking, and no way for an operator to answer "did the rollout succeed?" without manually inspecting each SeiNode and its pod.

InPlace fills this gap. It is the right strategy when:

- The operator has already waited for the chain to reach upgrade height (or the change is non-disruptive, like a sidecar bump)
- Creating fresh nodes is unnecessary or wasteful (the existing PVCs hold valid state)
- The operator wants controller-assisted status reporting without the cost and complexity of blue-green

Formalizing InPlace also makes the nil path clearer: nil means "I don't want any deployment orchestration at all, not even tracking."

## How It Works

### InPlace Rollout Lifecycle

1. **Engineer updates manifests.** The engineer changes `spec.template.spec.image` (or sidecar image, entrypoint) on the SeiNodeDeployment and applies the change (via GitOps push, `kubectl apply`, etc.).

2. **templateHash diverges.** The SeiNodeDeployment controller's `reconcileSeiNodes` calls `detectDeploymentNeeded`, which computes the current `templateHash` and compares against `status.templateHash`. When the hashes differ and `spec.updateStrategy.type == InPlace`, the controller writes a `RolloutStatus` to `status.rollout` instead of a `DeploymentStatus`.

3. **ensureSeiNode propagates changes.** Because there is no plan in progress (InPlace does not use the plan/task machinery), the normal `ensureSeiNode` loop runs and patches each child SeiNode's spec fields (image, entrypoint, sidecar, labels). All nodes are updated simultaneously -- chain upgrades are coordinated halts where sequential rollout provides no safety benefit.

4. **SeiNode controller converges StatefulSets.** Each SeiNode's `reconcileRunning` calls `reconcileNodeStatefulSet`, which applies the updated StatefulSet spec via SSA. Kubernetes detects the pod template change and terminates the old pod, scheduling a new one with the updated image.

5. **Sidecar restarts fresh; controller submits mark-ready.** The new pod's sidecar starts clean. The `reconcileRunning` method submits `mark-ready` unconditionally (it is idempotent). The sidecar flips to ready, `/v0/healthz` returns 200, and seid starts via the wait wrapper.

6. **SeiNodeDeployment controller monitors convergence.** On each reconcile, the controller checks `status.rollout`: for each node, it reads the child SeiNode's phase and pod readiness. When all nodes are Running with ready pods, the rollout is complete. The controller clears `status.rollout` and updates `status.templateHash`.

7. **Failure detection.** If a node's pod enters CrashLoopBackOff or the SeiNode transitions to Failed, the rollout status reflects this per-node. The controller does NOT auto-rollback -- blockchain rollback after a chain upgrade would leave the node unable to process new blocks. The engineer inspects the status and decides: push a fix, revert the image, or investigate.

## CRD Changes

### UpdateStrategyType enum

Add `InPlace` to the existing enum:

```go
// +kubebuilder:validation:Enum=BlueGreen;HardFork;InPlace
type UpdateStrategyType string

const (
    UpdateStrategyBlueGreen UpdateStrategyType = "BlueGreen"
    UpdateStrategyHardFork  UpdateStrategyType = "HardFork"
    UpdateStrategyInPlace   UpdateStrategyType = "InPlace"
)
```

### UpdateStrategy extension

```go
type UpdateStrategy struct {
    Type UpdateStrategyType `json:"type"`

    // HardFork configures blue-green deployment at a specific block height.
    // Required when type is HardFork.
    // +optional
    HardFork *HardForkStrategy `json:"hardFork,omitempty"`

    // InPlace configures in-place rollout behavior.
    // Optional when type is InPlace.
    // +optional
    InPlace *InPlaceStrategy `json:"inPlace,omitempty"`
}

// InPlaceStrategy configures an in-place rollout.
type InPlaceStrategy struct {
    // UpgradeHeight is the block height at which the chain upgrade occurs.
    // When set, the controller validates via the sidecar that the chain
    // has reached this height before patching the StatefulSet. This prevents
    // premature rollouts when manifests are pushed before the chain halts.
    // When omitted, the update is applied immediately (suitable for
    // non-upgrade changes like sidecar bumps or config tweaks).
    // +optional
    // +kubebuilder:validation:Minimum=1
    UpgradeHeight *int64 `json:"upgradeHeight,omitempty"`
}
```

### RolloutStatus (new type)

```go
// RolloutStatus tracks an in-progress in-place rollout. Unlike
// DeploymentStatus (which tracks entrant/incumbent node sets for
// blue-green), RolloutStatus tracks per-node convergence of the
// existing node set.
type RolloutStatus struct {
    // TargetHash is the templateHash being rolled out to.
    TargetHash string `json:"targetHash"`

    // StartedAt is when the rollout was first detected.
    StartedAt metav1.Time `json:"startedAt"`

    // Nodes reports per-node rollout state.
    // +listType=map
    // +listMapKey=name
    Nodes []RolloutNodeStatus `json:"nodes"`
}

// RolloutNodeStatus tracks a single node's convergence during an
// in-place rollout.
type RolloutNodeStatus struct {
    // Name is the SeiNode resource name.
    Name string `json:"name"`

    // Ready is true when the node is Running with a ready pod
    // on the new image.
    Ready bool `json:"ready"`

    // Phase is the SeiNode's current phase.
    Phase SeiNodePhase `json:"phase,omitempty"`
}
```

### SeiNodeDeploymentStatus addition

```go
type SeiNodeDeploymentStatus struct {
    // ... existing fields ...

    // Rollout tracks an in-progress in-place rollout.
    // Nil when no rollout is active. Mutually exclusive with Deployment.
    // +optional
    Rollout *RolloutStatus `json:"rollout,omitempty"`
}
```

### Validation

Add CEL rule for InPlace with upgradeHeight:

```
+kubebuilder:validation:XValidation:rule="self.type != 'InPlace' || !has(self.inPlace) || !has(self.inPlace.upgradeHeight) || self.inPlace.upgradeHeight > 0",message="inPlace.upgradeHeight must be > 0 when set"
```

## Controller Changes

### `detectDeploymentNeeded` (nodedeployment/nodes.go)

Branch on strategy type. InPlace creates a `RolloutStatus` instead of a `DeploymentStatus`:

```go
switch group.Spec.UpdateStrategy.Type {
case seiv1alpha1.UpdateStrategyInPlace:
    if group.Status.Rollout != nil {
        return
    }
    group.Status.Rollout = &seiv1alpha1.RolloutStatus{
        TargetHash: currentHash,
        StartedAt:  metav1.Now(),
        Nodes:      buildRolloutNodes(group),
    }
default: // BlueGreen, HardFork
    // ... existing deployment status creation ...
}
```

### `ensureSeiNode` (nodedeployment/nodes.go)

No changes needed. The existing in-place propagation at lines 156-177 already handles image, entrypoint, and sidecar updates. InPlace does not set `PlanInProgress`, so the `ensureSeiNode` loop runs normally.

### Upgrade height gating (nodedeployment/nodes.go)

When `InPlaceStrategy.UpgradeHeight` is set, the controller checks the sidecar's last block height before allowing `ensureSeiNode` to propagate the image change:

```go
func (r *SeiNodeDeploymentReconciler) upgradeHeightReached(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) bool {
    strategy := group.Spec.UpdateStrategy
    if strategy == nil || strategy.InPlace == nil || strategy.InPlace.UpgradeHeight == nil {
        return true
    }
    // Check at least one node has reached the upgrade height
    // via sidecar status polling (existing sidecar client)
    // ...
}
```

When the height has not been reached, the controller sets a condition:

```yaml
- type: UpdateBlocked
  status: "True"
  reason: UpgradeHeightNotReached
  message: "Chain at height 49999800, waiting for 50000000"
```

### Rollout status reconciliation (nodedeployment/status.go)

```go
func (r *SeiNodeDeploymentReconciler) reconcileRolloutStatus(
    group *seiv1alpha1.SeiNodeDeployment,
    nodes []seiv1alpha1.SeiNode,
) {
    if group.Status.Rollout == nil {
        return
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
    }
}
```

### `computeGroupPhase` (nodedeployment/status.go)

```go
if group.Status.Rollout != nil {
    return seiv1alpha1.GroupPhaseUpgrading
}
```

## Sidecar Mark-Ready Resolution

### The Problem

The sidecar starts fresh on every pod restart. Its `/v0/healthz` endpoint returns 503 until a `mark-ready` task is submitted. The `mark-ready` task is part of the initialization plan, which only runs during the Initializing phase. Once a SeiNode reaches Running, the controller never re-submits `mark-ready`.

After an in-place image update, the StatefulSet rolls the pod. The new sidecar starts, binds its port, and returns 503 from `/v0/healthz`. The seid container's wait wrapper polls `/v0/healthz` and blocks forever.

### Primary Solution: Controller Re-submits Mark-Ready

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

### Future Improvement: Sidecar Self-Detection

As a follow-up, the sidecar could detect existing chain data on startup and skip the ready gate entirely. If the sidecar finds `$SEI_HOME/data/` populated (blockstore.db, state.db), it knows this is a restart, not a fresh init, and can serve 200 on `/v0/healthz` immediately. This eliminates the controller round-trip but requires a sidecar release.

### Why NOT persist ready state?

Persisting ready state to disk conflates "previously initialized" with "ready to serve." A sidecar that was ready before a crash is not necessarily ready after -- its in-memory task state is gone. Having the controller explicitly re-submit `mark-ready` is the correct ownership model.

### Why NOT transition back to Initializing?

Re-running the full initialization plan (snapshot restore, config apply, genesis configure, peer discovery) is wasteful and risky for an image update. The PVC already has valid data. `mark-ready` is the only task that needs to re-run.

## Status Reporting

### During rollout

```yaml
status:
  phase: Upgrading
  rollout:
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
  - type: NodesReady
    status: "True"
    reason: AllNodesReady
    message: "2/2 nodes ready"
```

### Events

| Event | Type | Reason | When |
|---|---|---|---|
| Rollout started | Normal | `InPlaceRolloutStarted` | `detectDeploymentNeeded` creates `RolloutStatus` |
| Rollout complete | Normal | `InPlaceRolloutComplete` | All nodes converge to Running |
| Update blocked | Warning | `UpdateBlocked` | `upgradeHeight` set but chain has not reached it |
| Node not converging | Warning | `NodeNotConverging` | A node has been non-ready for > 10 minutes during rollout |

## Failure Modes

### New image crashes (CrashLoopBackOff)

The pod enters CrashLoopBackOff. The rollout status shows `ready: false` for the affected node. The group phase stays `Upgrading`. The controller does NOT auto-rollback -- rolling back to the pre-upgrade binary after a chain upgrade means the node cannot process new blocks.

**Recovery:** Fix the image or config, push a new manifest. The controller detects a new hash divergence and updates the existing rollout's `targetHash`.

### Premature push (chain not at upgrade height)

If `upgradeHeight` is set, the controller blocks the update and surfaces `UpdateBlocked` condition. The existing pods continue running the old image.

If `upgradeHeight` is not set, the engineer takes full responsibility for timing.

### Sidecar cannot start

The sidecar init container's `RestartPolicy: Always` causes Kubernetes to restart it. The controller's `buildSidecarClient` returns nil until reachable, and `reconcileRunning` requeues.

### Partial rollout

The rollout status shows per-node state. The group phase is `Upgrading` as long as any node is not ready. The `NodesReady` condition provides a summary.

### Concurrent spec change during rollout

If the operator changes the image again while a rollout is active, `detectDeploymentNeeded` compares against `status.templateHash` (pre-rollout). The controller updates the existing rollout's `targetHash` and `ensureSeiNode` pushes the latest spec to all children. No plan state to corrupt.

## File-by-File Changes

| File | Change |
|------|--------|
| `api/v1alpha1/seinodedeployment_types.go` | Add `UpdateStrategyInPlace` to enum. Add `InPlaceStrategy`, `RolloutStatus`, `RolloutNodeStatus` types. Add `Rollout` and `InPlace` fields. |
| `api/v1alpha1/zz_generated.deepcopy.go` | Regenerated |
| `manifests/crd/bases/sei.io_seinodedeployments.yaml` | Regenerated |
| `internal/controller/nodedeployment/nodes.go` | `detectDeploymentNeeded`: branch on strategy type, create `RolloutStatus` for InPlace. Add `buildRolloutNodes`, `upgradeHeightReached`. |
| `internal/controller/nodedeployment/status.go` | Add `reconcileRolloutStatus`. Extend `computeGroupPhase` for active rollout. |
| `internal/controller/node/controller.go` | `reconcileRunning`: add `ensureMarkReady` call before `reconcileRuntimeTasks`. |
| `internal/controller/node/plan_execution.go` | Add `ensureMarkReady` method. |

## Test Plan

### Unit Tests

| Test | File | Description |
|------|------|-------------|
| `TestDetectDeploymentNeeded_InPlace` | `nodes_test.go` | Hash divergence with InPlace creates `RolloutStatus`, not `DeploymentStatus` |
| `TestDetectDeploymentNeeded_InPlace_AlreadyActive` | `nodes_test.go` | Existing rollout is not duplicated on re-reconcile |
| `TestDetectDeploymentNeeded_BlueGreen_Unchanged` | `nodes_test.go` | BlueGreen/HardFork still create `DeploymentStatus` |
| `TestBuildRolloutNodes` | `nodes_test.go` | Creates entries for each incumbent node |
| `TestReconcileRolloutStatus_AllReady` | `status_test.go` | Rollout cleared and templateHash updated when all nodes Running |
| `TestReconcileRolloutStatus_Partial` | `status_test.go` | Rollout persists with mixed ready/not-ready |
| `TestReconcileRolloutStatus_WithFailedNode` | `status_test.go` | Shows `ready: false, phase: Failed` |
| `TestComputeGroupPhase_RolloutActive` | `status_test.go` | Returns `Upgrading` when rollout non-nil |
| `TestEnsureMarkReady` | `reconciler_test.go` | Submits mark-ready task via sidecar client |
| `TestReconcileRunning_SubmitsMarkReady` | `reconciler_test.go` | mark-ready called before runtime tasks |
| `TestUpgradeHeightGating` | `nodes_test.go` | Update blocked when chain below upgradeHeight |

## Implementation Order

1. **CRD types.** Add enum value, `InPlaceStrategy`, `RolloutStatus` types. `make manifests generate`.
2. **ensureMarkReady.** Add to SeiNode controller's `reconcileRunning`. This unblocks pod restarts for all strategies and can ship independently.
3. **detectDeploymentNeeded.** Branch on InPlace, create `RolloutStatus`.
4. **Rollout status reconciliation.** Add `reconcileRolloutStatus`, extend `computeGroupPhase`.
5. **Upgrade height gating.** Add `upgradeHeightReached` check.
6. **Events.** Emit `InPlaceRolloutStarted`, `InPlaceRolloutComplete`, `UpdateBlocked`.
7. **Tests.** Unit tests for each step, integration test for end-to-end rollout.

Step 2 is the highest-priority standalone fix -- it resolves the sidecar restart problem that currently blocks all image updates on Running nodes.
