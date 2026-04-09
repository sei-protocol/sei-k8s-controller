# HardFork Deployment Simplification via Bootstrap

## Summary

Replace the current 6-task HardFork deployment plan with a 4-task plan that leverages the existing bootstrap Job mechanism. The halt-height coordination (SubmitHaltSignal + AwaitNodesAtHeight) is eliminated because the entrant node's bootstrap spec handles syncing to the correct height with the incumbent binary before the production StatefulSet starts with the new binary.

## Motivation

The current HardFork plan orchestrates two parallel concerns:
1. Getting the entrant to the right height (bootstrap Job already does this)
2. Halting the incumbent at the right height (fire-and-forget SIGTERM via sidecar)

These are redundant. The bootstrap mechanism already runs `seid start --halt-height` with a configurable image, exits cleanly at the target height, then hands off to the production StatefulSet. The entrant is self-contained — no coordination with incumbents needed.

## Current vs Proposed

### Current HardFork Plan (6 tasks)
```
CreateEntrantNodes → AwaitRunning → SubmitHaltSignal → AwaitNodesAtHeight → SwitchTraffic → TeardownIncumbents
```

### Proposed HardFork Plan (4 tasks)
```
CreateEntrantNodes → AwaitRunning → SwitchTraffic → TeardownIncumbents
```

Tasks removed:
- `SubmitHaltSignal` — no longer needed; incumbents run until teardown
- `AwaitNodesAtHeight` — no longer needed; `AwaitRunning` already gates on the full bootstrap→production sequence completing

## How It Works

### Entrant Node Spec

When the controller creates entrant SeiNodes during a HardFork deployment, it injects bootstrap config from the deployment strategy:

```yaml
# Entrant SeiNode spec (constructed by CreateEntrantNodes)
spec:
  image: "sei:v6.4.0"              # new binary (from updated template)
  fullNode:
    snapshot:
      bootstrapImage: "sei:v6.3.0"  # incumbent binary (auto-injected)
      s3:
        targetHeight: 49999999       # haltHeight - 1 (auto-calculated)
```

### Entrant Lifecycle

```
1. Bootstrap Job starts with sei:v6.3.0
   → Sidecar runs: snapshot-restore, configure-genesis, config-apply, 
     discover-peers, configure-state-sync, config-validate, mark-ready
   → seid starts with --halt-height 49999999
   → Syncs from snapshot, block syncs to height 49999999
   → Commits block, exits cleanly (SIGINT, code 130)

2. Controller detects Job complete → TeardownBootstrap
   → Deletes bootstrap Job + Service
   → PVC survives with state through block 49999999

3. StatefulSet starts with sei:v6.4.0
   → Sidecar runs post-bootstrap tasks: config-apply, discover-peers,
     config-validate, mark-ready
   → seid starts, processes block 50000000 (upgrade height)
   → Upgrade handler runs migrations
   → Node continues on v6.4.0

4. SeiNode reaches PhaseRunning
   → AwaitRunning task completes
```

### Incumbent Lifecycle

Incumbents keep running normally throughout. They are NOT halted — they continue serving traffic via the revision-pinned Service selector until `SwitchTraffic` flips the selector and `TeardownIncumbents` deletes them.

This is actually better than the current design — no SIGTERM means no risk of incumbents going down before entrants are ready.

## CRD Changes

### HardForkStrategy

```go
// Before:
type HardForkStrategy struct {
    HaltHeight int64 `json:"haltHeight"`
}

// After:
type HardForkStrategy struct {
    // HaltHeight is the block height at which the chain upgrade occurs.
    // The controller creates entrant nodes with bootstrapImage set to
    // the incumbent image and targetHeight set to HaltHeight - 1.
    // The entrant's production StatefulSet starts with the new image
    // and processes the upgrade at HaltHeight.
    HaltHeight int64 `json:"haltHeight"`
}
```

No structural CRD change needed — `HaltHeight` stays but its semantics shift from "tell incumbents to halt here" to "entrants bootstrap to here - 1, then new binary handles here."

### CEL Validation

Add validation that HardFork requires the template to have a snapshot source (bootstrap needs S3):

```
// On UpdateStrategy:
+kubebuilder:validation:XValidation:rule="self.type != 'HardFork' || (has(self.hardFork) && self.hardFork.haltHeight > 0)",message="hardFork.haltHeight must be > 0"
```

This already exists. No new validations needed — the bootstrap planner's own validation (`NeedsBootstrap` checks) handles the rest.

## Planner Changes

### File: `internal/planner/deployment.go`

Replace `hardForkDeploymentPlanner.BuildPlan`:

```go
func (p *hardForkDeploymentPlanner) BuildPlan(
    group *seiv1alpha1.SeiNodeDeployment,
) (*seiv1alpha1.TaskPlan, error) {
    planID := uuid.New().String()
    incumbentNodes := group.Status.IncumbentNodes
    entrantNodes := EntrantNodeNames(group)
    entrantRevision := EntrantRevision(group)
    ns := group.Namespace

    prog := []struct {
        taskType string
        params   any
    }{
        {task.TaskTypeCreateEntrantNodes, &task.CreateEntrantNodesParams{
            GroupName:       group.Name,
            Namespace:       ns,
            EntrantRevision: entrantRevision,
            NodeNames:       entrantNodes,
        }},
        {task.TaskTypeAwaitNodesRunning, &task.AwaitNodesRunningParams{
            GroupName: group.Name,
            Namespace: ns,
            Expected:  len(entrantNodes),
            NodeNames: entrantNodes,
        }},
        {task.TaskTypeSwitchTraffic, &task.SwitchTrafficParams{
            GroupName:       group.Name,
            Namespace:       ns,
            EntrantRevision: entrantRevision,
        }},
        {task.TaskTypeTeardownNodes, &task.TeardownNodesParams{
            Namespace: ns,
            NodeNames: incumbentNodes,
        }},
    }

    tasks := make([]seiv1alpha1.PlannedTask, len(prog))
    for i, p := range prog {
        t, err := buildPlannedTask(planID, p.taskType, i, p.params)
        if err != nil {
            return nil, err
        }
        tasks[i] = t
    }
    return &seiv1alpha1.TaskPlan{ID: planID, Phase: seiv1alpha1.TaskPlanActive, Tasks: tasks}, nil
}
```

This is identical to the BlueGreen planner minus the `AwaitNodesCaughtUp` task.

## Task Changes

### CreateEntrantNodes — Inject Bootstrap Config

**File:** `internal/task/deployment_create.go`

When creating entrant nodes for a HardFork deployment, the task must inject the bootstrap config so the entrant uses the incumbent binary to sync:

```go
func (e *createEntrantNodesExecution) createNode(ctx context.Context, group, name string, ordinal int) error {
    // ... existing node creation logic ...
    
    // For HardFork: inject bootstrap config into entrant's snapshot source
    if e.params.HardFork != nil {
        snap := node.Spec.SnapshotSource()
        if snap != nil {
            snap.BootstrapImage = e.params.HardFork.IncumbentImage
            if snap.S3 != nil {
                snap.S3.TargetHeight = e.params.HardFork.HaltHeight - 1
            }
        }
    }
    // ...
}
```

Add fields to `CreateEntrantNodesParams`:

```go
type CreateEntrantNodesParams struct {
    GroupName       string            `json:"groupName"`
    Namespace       string            `json:"namespace"`
    EntrantRevision string            `json:"entrantRevision"`
    NodeNames       []string          `json:"nodeNames"`
    HardFork        *HardForkParams   `json:"hardFork,omitempty"`
}

type HardForkParams struct {
    IncumbentImage string `json:"incumbentImage"`
    HaltHeight     int64  `json:"haltHeight"`
}
```

The `IncumbentImage` comes from the group's current template hash baseline — the image that was running before the update. This needs to be captured in `detectDeploymentNeeded` and stored in `DeploymentStatus`:

```go
type DeploymentStatus struct {
    IncumbentRevision string   `json:"incumbentRevision"`
    EntrantRevision   string   `json:"entrantRevision"`
    EntrantNodes      []string `json:"entrantNodes,omitempty"`
    IncumbentImage    string   `json:"incumbentImage,omitempty"` // NEW
}
```

Populated in `detectDeploymentNeeded`:

```go
group.Status.Deployment = &seiv1alpha1.DeploymentStatus{
    IncumbentRevision: planner.IncumbentRevision(group),
    EntrantRevision:   planner.EntrantRevision(group),
    EntrantNodes:      planner.EntrantNodeNames(group),
    IncumbentImage:    group.Spec.Template.Spec.Image, // capture BEFORE the spec change
}
```

Wait — by the time `detectDeploymentNeeded` runs, the spec already has the new image. The incumbent image is what was running before. We need to read it from an incumbent SeiNode:

```go
// In detectDeploymentNeeded, read incumbent image from an existing node
if len(group.Status.IncumbentNodes) > 0 {
    var incumbentNode seiv1alpha1.SeiNode
    if err := r.Get(ctx, types.NamespacedName{
        Name: group.Status.IncumbentNodes[0], Namespace: group.Namespace,
    }, &incumbentNode); err == nil {
        group.Status.Deployment.IncumbentImage = incumbentNode.Spec.Image
    }
}
```

### SubmitHaltSignal and AwaitNodesAtHeight — Remove or Deprecate

These tasks can be removed from the HardFork plan but should be kept in the codebase for potential future use (manual halt operations, testing). Remove the import from `deployment.go` but don't delete the files.

Alternatively, if we're certain they won't be needed, delete:
- `internal/task/deployment_signal.go`
- `internal/task/deployment_await.go` (keep `awaitNodesCaughtUpExecution` which BlueGreen uses)

Recommendation: **keep the files** but remove them from the HardFork plan. They're useful for manual operations.

## BlueGreen Convergence

The HardFork plan is now:
```
CreateEntrantNodes → AwaitRunning → SwitchTraffic → Teardown
```

The BlueGreen plan is:
```
CreateEntrantNodes → AwaitRunning → AwaitCaughtUp → SwitchTraffic → Teardown
```

The only difference is BlueGreen has an extra `AwaitCaughtUp` gate. This means:

- **BlueGreen** = entrant must be at chain tip before switching traffic (for non-upgrade deployments where the chain keeps producing blocks)
- **HardFork** = entrant just needs to be Running (which means its bootstrap completed and the upgrade handler ran — it IS at the chain tip because the chain halted at the upgrade height)

These could be unified into a single `Deployment` strategy with an optional `haltHeight` parameter. When `haltHeight` is set, bootstrap config is injected and no caught-up check is needed. When `haltHeight` is absent, it's a regular blue-green with caught-up gating.

**Recommendation for now:** Keep them as separate strategy types for clarity, but note they share the same code path and could be merged later.

## Traffic Switch Timing

The current design has a brief traffic gap between TeardownNodes (deleting incumbents) and plan completion (Service selector update). This design has the **same issue** since the plan structure is the same.

The fix (orthogonal to this design, but worth noting): call `reconcileNetworking` in `SwitchTraffic` instead of waiting for `completePlan`. This would make the selector flip immediate at task 3, before teardown at task 4.

## Failure Modes

### Bootstrap Job fails (wrong image, can't sync)
- Entrant SeiNode enters `PhaseFailed`
- `AwaitRunning` detects this and fails the plan immediately
- Group enters `Degraded` phase
- Incumbents continue running — no impact to production
- **Recovery:** Fix the image/config, delete orphaned entrant nodes, update the group spec to re-trigger

### New binary panics on upgrade height
- The StatefulSet pod crash-loops after bootstrap completes
- The SeiNode never reaches `PhaseRunning` (sidecar can't mark-ready because seid keeps crashing)
- `AwaitRunning` eventually times out (if we add a timeout) or sits indefinitely
- Incumbents continue running
- **Recovery:** Same as above — fix the binary, re-trigger

### Chain hasn't reached the target height yet
- Bootstrap Job starts seid with `--halt-height`, seid syncs and waits for the chain to produce blocks up to that height
- This is a normal wait — the Job has no timeout (86400 failure threshold on the startup probe = ~5 days)
- The deployment just takes longer
- **Not a failure** — it's expected when deploying ahead of the upgrade

### Entrant can't find peers
- Config-apply and discover-peers happen during bootstrap
- If peer discovery fails, the bootstrap plan fails, SeiNode enters PhaseFailed
- Same recovery path as above

## File-by-File Changes

| File | Change |
|------|--------|
| `api/v1alpha1/seinodedeployment_types.go` | Add `IncumbentImage string` to `DeploymentStatus` |
| `internal/planner/deployment.go` | Remove tasks 2-3 from `hardForkDeploymentPlanner.BuildPlan`, pass `HardForkParams` to `CreateEntrantNodesParams` |
| `internal/task/deployment.go` | Add `HardForkParams` struct, add `HardFork` field to `CreateEntrantNodesParams` |
| `internal/task/deployment_create.go` | Inject `bootstrapImage` and `targetHeight` from `HardForkParams` into entrant spec |
| `internal/controller/nodedeployment/nodes.go` | Capture incumbent image in `detectDeploymentNeeded` |
| `api/v1alpha1/zz_generated.deepcopy.go` | Regenerated |
| `manifests/sei.io_seinodedeployments.yaml` | Regenerated |

Files **not changed** (kept for future use):
- `internal/task/deployment_signal.go` — SubmitHaltSignal kept but unused by HardFork
- `internal/task/deployment_await.go` — AwaitNodesAtHeight kept but unused by HardFork

## Test Plan

### Unit Tests

1. **`TestHardForkPlan_FourTasks`** — verify the plan has exactly 4 tasks in order: CreateEntrantNodes, AwaitRunning, SwitchTraffic, TeardownNodes
2. **`TestHardForkPlan_NoHaltSignalOrAwaitHeight`** — verify SubmitHaltSignal and AwaitNodesAtHeight are NOT in the plan
3. **`TestCreateEntrantNodes_InjectsBootstrapConfig`** — when HardForkParams is set, verify entrant node spec has `bootstrapImage` = incumbent image and `s3.targetHeight` = haltHeight - 1
4. **`TestCreateEntrantNodes_NoBootstrapForBlueGreen`** — when HardForkParams is nil, verify no bootstrap injection
5. **`TestDetectDeployment_CapturesIncumbentImage`** — verify DeploymentStatus.IncumbentImage is populated from existing node

### Integration Test Scenario

1. Create a SeiNodeDeployment with `image: v1.0` and `updateStrategy.type: HardFork`
2. Wait for incumbent nodes to reach Running
3. Update `image: v2.0` and set `haltHeight: 100`
4. Verify entrant nodes are created with `bootstrapImage: v1.0` and `targetHeight: 99`
5. Verify entrant goes through bootstrap Job → StatefulSet lifecycle
6. Verify traffic switches to entrant after AwaitRunning
7. Verify incumbent nodes are torn down

## Implementation Order

1. Add `IncumbentImage` to `DeploymentStatus` + capture in `detectDeploymentNeeded`
2. Add `HardForkParams` to `CreateEntrantNodesParams`
3. Update `CreateEntrantNodes` task to inject bootstrap config
4. Simplify `hardForkDeploymentPlanner.BuildPlan` to 4 tasks
5. `make manifests generate`
6. Add tests
7. `make test && make lint`

## Open Questions

1. **Should we require an S3 snapshot source for HardFork?** The bootstrap mechanism needs `s3.targetHeight > 0` to activate. If the entrant template doesn't have an S3 snapshot config, `NeedsBootstrap` returns false and the entrant just state-syncs normally (which may or may not land at the right height). We could either require it via CEL validation or have the controller auto-inject a minimal S3 config.

2. **What if the entrant uses state sync instead of S3?** State sync lands at whatever snapshot height is available, which may not be exactly `haltHeight - 1`. This is fine if the snapshot is past the upgrade height (new binary handles everything). But if the snapshot is before the upgrade height, the new binary would need to block-sync through the upgrade — which works because the new binary has the upgrade handler. The bootstrap mechanism is more precise (lands at exactly the right height) but state sync is simpler for cases where exact height doesn't matter.

3. **Should the BlueGreen and HardFork strategies be merged?** They're now almost identical. A unified `Deployment` strategy with optional `haltHeight` would reduce code. Deferred to a follow-up.
