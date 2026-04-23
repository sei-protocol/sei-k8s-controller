# Sidecar Mark-Ready Re-Apply on Pod Replacement

## Summary

When a `seid` pod is replaced after the SeiNode reaches `Running` — by chaos, node drain, OOM, crash, kubelet reschedule, StatefulSet rollout, or any pod-level restart — the new pod's fresh sidecar returns 503 from `/v0/healthz` and blocks `seid`'s startup probe indefinitely. The controller does not observe this today. The pod stays alive but not serving, and an operator must `kubectl port-forward` to POST `mark-ready` manually. Issue #127.

This design fixes that by extending the existing `buildRunningPlan` drift detector on the SeiNode reconciler. Today `buildRunningPlan` checks one form of drift (image: `spec.image != status.currentImage`) and, if drift is detected, emits a short plan. We add a second form of drift (sidecar readiness: `Healthz() == 503`) that emits a one-task `MarkReady` plan. Both drift types flow through the exact same `buildRunningPlan → ResolvePlan → ExecutePlan` path that already handles steady-state work.

The fix is proactive and automatic. Every steady-state reconcile (30 s requeue on Running), the controller probes the sidecar. If 503, a plan is spawned. On success the plan completes and the node returns to steady state. No new controllers, no new reconcile loop, no new task type.

## Motivation

### The symptom

From issue #127:

> When a seid pod is replaced (by kubelet after chaos, node drain, OOM, crash, or any kind of reschedule), the new pod comes up with containers running but stuck:
> - `sei-sidecar`: Readiness probe failing with HTTP 503.
> - `seid`: Startup probe failing with HTTP 503.
>
> Pod stays in this state indefinitely. No block production by that validator. In dev release-6-5 we observed this for 2+ minutes on a pod targeted by chaos-mesh PodChaos (`action: pod-kill`) — would have stayed stuck forever without intervention.

The manual workaround — port-forward, `curl -X POST /v0/tasks -d '{"type":"mark-ready"}'` — is load-bearing operational knowledge that should not be. Grafana shows the validator rejoining consensus within ~20 s of the manual POST, so the fix value is high and the blast radius of the fix is small.

### Why the controller misses it today

The controller sends `mark-ready` exactly once per SeiNode lifecycle: as the last step of the initialization plan produced by `buildBasePlan` / `buildBootstrapPlan`. Once the init plan completes, `Status.Plan` is nilled and the node enters `Running`. From then on, steady-state reconciles call `buildRunningPlan`, which returns `nil` unless image drift is detected. Nothing watches the sidecar's readiness after init.

Pod replacement is silent to the SeiNode controller for this purpose: the StatefulSet it owns is unchanged (`ObservedGeneration == Generation`, `UpdatedReplicas == Replicas`), so `ObserveImage` logic sees no drift. The new pod boots, the sidecar starts fresh, `/v0/healthz` reports 503, and the controller has no signal that anything is wrong. Meanwhile `seid`'s startup probe — chained behind sidecar readiness via the `seid` wait-wrapper (see `internal/task/bootstrap_resources.go:197`) — blocks forever.

### Why this is first-class operational observability, not a special case

This failure mode is the intersection of three facts that are not going away:

1. The sidecar's mark-ready gate is correct design for initialization (it ensures config-apply / snapshot-restore completed before `seid` is allowed to boot) but wrong posture for restart (the PVC is already populated; there is nothing to block on).
2. Pods will be replaced in production. Chaos testing forces it. Spot/preemption forces it. Node drains force it. OOM forces it. This is not a rare edge case.
3. Sidecar self-detection ("if `$SEI_HOME/data/` is populated, skip the ready gate") is called out as a future sidecar improvement in `inplace-update-strategy.md` but requires a sidecar release coordinated with validator operators. The controller fix is independent and ships now.

Therefore the right place to handle this is exactly where steady-state drift is already handled: the SeiNode reconciler's running-phase planner.

## Studying the Current Pattern

This section is the motivation for the chosen design. The constraint from the requester is that the fix be **idiomatic to the existing plan-based pattern**. That requires understanding what the existing pattern actually is.

### Steady-state drift detection already exists

`internal/planner/planner.go:591`:

```go
// buildRunningPlan returns a plan for a Running node only when the controller
// recognizes a scenario that requires action. Returns nil when the node is
// in steady state (no drift detected).
//
// Currently detects image drift (spec.image != status.currentImage). This is
// the extension point for future drift types (config changes, peer changes).
func buildRunningPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
    if node.Spec.Image != node.Status.CurrentImage {
        return buildNodeUpdatePlan(node)
    }
    return nil, nil
}
```

Every mode planner (`full.go`, `archive.go`, `validator.go`, `replay.go`) dispatches to `buildRunningPlan` when `node.Status.Phase == Running`. The doc-string explicitly names itself as the extension point. The function is called from `ResolvePlan` on every reconcile.

When drift is detected, `buildNodeUpdatePlan` returns:

```
[ApplyStatefulSet, ApplyService, ObserveImage, MarkReady]
```

with `TargetPhase: Running` and `FailedPhase: ""` (empty). The empty `FailedPhase` is load-bearing: **a failure of a Running-phase plan retries on the next reconcile rather than transitioning the node out of Running.** This is exactly the semantics we want for a transient 503 → mark-ready → 200 loop.

### Plans are cheap

`TaskPlan` is a simple struct stored inline on `status.plan`. Plan IDs are UUID v4; task IDs within a plan are UUID v5 deterministic (`DeterministicTaskID(planID, taskType, planIndex)`). There is no plan queue, no external store, no controller-wide state. The reconcile loop either has a plan or it doesn't. Completed plans are nil'd from status by `handleTerminalPlan`.

A single-task plan is already a first-class thing: `buildRunningPlan` is allowed to return `nil`, but nothing forbids it from returning a plan with one task. The plan executor iterates `plan.Tasks` without caring how many there are.

### The sidecar client already exists on the executor

The plan executor's `ExecutionConfig.BuildSidecarClient` is wired in `cmd/main.go:166-168` to construct a per-node `sidecar.NewSidecarClient(planner.SidecarURLForNode(node))`. That client already exposes `Healthz(ctx) (bool, error)` in seictl (`sidecar/client/client.go:185`). 200 → `(true, nil)`. 503 → `(false, nil)`. Network error / other status → `(_, err)`. This is exactly the three-way signal we need.

### What a "new plan" path looks like

Look at `buildNodeUpdatePlan` for the full shape of adding a new Running-phase plan type:

1. Set a condition (`NodeUpdateInProgress = True`, reason `UpdateStarted`).
2. Build a `prog []string` of task types.
3. Walk it via `buildPlannedTask` to produce `[]PlannedTask`.
4. Return a `TaskPlan{Phase: Active, Tasks: ..., TargetPhase: Running}`.

`handleTerminalPlan` (called by `ResolvePlan` on every reconcile before building the next plan) clears the condition when the plan completes or fails. No separate teardown path, no separate watcher.

This pattern is small, symmetric, and already tested.

## Option Comparison

Three options were considered. The requester's guidance was to lean toward a plan (Option A) if it's viable, and to reject clearly wrong options rather than pad the doc.

### Option A: mark-ready is a plan unto itself

`buildRunningPlan` gains a second drift detector. When Healthz returns 503, it emits a one-task plan `[MarkReady]` with `TargetPhase: Running` and empty `FailedPhase`.

- The drift check is the only new code in the planner.
- The plan itself is two lines of existing machinery.
- Plan execution, status, conditions, metrics, events all come for free from the plan system.
- Failure retries are automatic (empty `FailedPhase`).
- Unit-testable via the existing `TestBuildRunningPlan_*` pattern.

### Option B: inline the mark-ready call in the reconciler, outside the plan system

The SeiNode reconciler grows a new method that probes Healthz and POSTs `mark-ready` directly, bypassing the plan executor.

**Rejected.** This breaks two invariants:
1. Plans are the unit of observability. The rollout status, the `NodeUpdateInProgress`-style condition, the plan-duration metric, and the `PhaseTransition` event all key off `status.plan`. Inline mutations skip every one of them.
2. It adds a second code path that talks to the sidecar from the reconciler proper. `inplace-update-strategy.md` explicitly preserves the single-writer invariant — the sidecar is driven through the plan executor, full stop. The `awaitNodesCaughtUpExecution` task in `deployment_await.go:138` is the template: controller-side polling task, executed by the plan executor, reachable via the same `buildSidecarClient` the executor already wires up.

### Option C: cheap probe inline, plan if needed (hybrid)

The probe runs inline on every Running-phase reconcile; when it returns 503, a one-task plan is spawned. Option A's plan shape plus an inline probe.

**This is Option A.** The `buildRunningPlan` call *is* the inline probe — it runs on every reconcile and decides whether to return a plan. Separating "probe" from "plan construction" is a distinction without a difference in this codebase. The probe is just a function call inside the planner.

### Choice: Option A

Option A is what the existing pattern already does for image drift, unchanged. The fix is "teach `buildRunningPlan` about a second kind of drift."

---

## Design

### Detection: sidecar Healthz as the drift signal

In `buildRunningPlan`, after the existing image-drift branch, add a sidecar-readiness branch. Because the planner must remain pure (no network calls during plan construction — it's called from `ResolvePlan` which is called with a pure in-memory node), the actual Healthz probe runs in the reconciler and stamps a `SidecarReady` condition onto the node. The planner reads that condition.

This is the same shape as image drift, where the planner reads `node.Status.CurrentImage` — a value that was stamped by the `ObserveImage` task (or, equivalently, by reconciler-side observation). The planner never touches the StatefulSet directly.

```
[reconciler]                         [planner]
  probeSidecarHealth(node)             buildRunningPlan(node)
    -> sets SidecarReady condition       reads node.Status.Conditions
                                         if SidecarReady == False:
                                            return buildMarkReadyPlan(node)
```

The probe is cheap: a single `GET /v0/healthz` with a tight timeout (2 s), issued at most once per 30 s reconcile interval per node. At 100 nodes per controller that's 100 GETs per 30 s = ~3 r/s of controller traffic. Not worth budgeting.

#### Probe outcomes

| Healthz response | Signal | Planner action |
|---|---|---|
| HTTP 200 | Sidecar ready | `SidecarReady = True`. No plan. |
| HTTP 503 | Sidecar reachable but gated | `SidecarReady = False, Reason=NotReady`. Build `MarkReady` plan. |
| Network error (DNS, connect refused) | Sidecar not reachable — likely still starting | `SidecarReady = Unknown, Reason=Unreachable`. **No plan.** |
| Unexpected status (4xx, other 5xx) | Sidecar is in an unexpected state | `SidecarReady = Unknown, Reason=ProbeError`. **No plan.** |

**The `Unknown` case is deliberate.** A pod that was just deleted and has not rescheduled yet, or a sidecar mid-startup, will return connection errors. Firing `mark-ready` at a non-existent HTTP endpoint is a no-op, but firing it at a misbehaving sidecar that might be in a partial state is not obviously harmless. The correct posture is: probe again next reconcile. If the sidecar really is stuck 503, the next probe will get 503 and the plan fires. If it was transiently unreachable, the next probe gets 200 and we do nothing.

This also means **Healthz is checked *after* the plan completes**, providing the round-trip verification that mark-ready actually flipped the gate. If it didn't, the next reconcile detects 503 again and re-plans. This is the self-healing loop.

### Plan: a one-task `MarkReady` plan

New planner function `buildMarkReadyPlan`:

```go
// buildMarkReadyPlan constructs a one-task plan for a Running node whose
// sidecar reports 503. The plan completes when the sidecar accepts the
// mark-ready submission (fire-and-forget: the task's Execute succeeds on
// 201/202 and transitions to Complete). Failure retries on next reconcile.
//
// FailedPhase is deliberately empty: a transient sidecar failure should
// not transition the node out of Running.
func buildMarkReadyPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
    setSidecarReadyCondition(node, metav1.ConditionFalse, "NotReady",
        "sidecar returned 503, re-issuing mark-ready")

    planID := uuid.New().String()
    t, err := buildPlannedTask(planID, TaskMarkReady, 0, &task.MarkReadyParams{})
    if err != nil {
        return nil, err
    }
    return &seiv1alpha1.TaskPlan{
        ID:          planID,
        Phase:       seiv1alpha1.TaskPlanActive,
        Tasks:       []seiv1alpha1.PlannedTask{t},
        TargetPhase: seiv1alpha1.PhaseRunning,
    }, nil
}
```

Updated `buildRunningPlan`:

```go
func buildRunningPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
    if node.Spec.Image != node.Status.CurrentImage {
        return buildNodeUpdatePlan(node)
    }
    if sidecarNeedsReapproval(node) {
        return buildMarkReadyPlan(node)
    }
    return nil, nil
}

// sidecarNeedsReapproval is true iff the SidecarReady condition is
// False with reason NotReady. Unknown and missing are not actionable —
// the reconciler will re-probe next interval.
func sidecarNeedsReapproval(node *seiv1alpha1.SeiNode) bool {
    cond := meta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionSidecarReady)
    return cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason == "NotReady"
}
```

Conflict policy with the node-update plan: if `spec.image != currentImage` (image drift) and Healthz returns 503 at the same moment, **image drift wins** because `buildRunningPlan` checks it first. This is correct: the image-update plan already ends with `MarkReady`, so solving image drift also solves the stale sidecar in the same plan. A separate mark-ready plan spawned concurrently would be redundant.

Conflict policy with the node-update plan during execution: `ResolvePlan` short-circuits if `node.Status.Plan != nil && Phase == Active`. If an image-update plan is running, the probe still runs (it's just a status update) and may flip `SidecarReady = False`, but no new plan is built until the current one terminates. When the image-update plan completes, `handleTerminalPlan` clears the plan; the next reconcile re-checks drift; `CurrentImage` is now stamped by `ObserveImage` so image drift is resolved; if Healthz is still 503 (unusual — the image update's own `MarkReady` task should have fixed it), a mark-ready plan fires. This is the right layering.

### Probe execution: in the reconciler, before `ResolvePlan`

In `internal/controller/node/controller.go`, add a new step in `Reconcile` before `ResolvePlan`:

```go
// Only probe when we expect the sidecar to be reachable — avoids noise
// during Initializing (where init tasks own the sidecar interaction).
if node.Status.Phase == seiv1alpha1.PhaseRunning {
    if dirty := r.probeSidecarHealth(ctx, node); dirty {
        statusDirty = true
    }
}
```

`probeSidecarHealth` uses the same sidecar-client construction path as the plan executor (`sidecar.NewSidecarClient(planner.SidecarURLForNode(node))`). It applies a 2 s context deadline, sets the condition, and returns true iff the condition changed.

The probe is **reconciler-side, not task-side** because:
- It runs on every reconcile, not as part of a plan.
- It feeds the planner (via a condition) rather than being fed by it.
- Task executions are constructed from plans stored in `status.plan`. The probe has no plan to live in — it's the thing that decides whether a plan should exist.

This is the same pattern as `reconcilePeers` (called in `Reconcile` before `ResolvePlan`, mutates status, feeds the planner).

### Idempotency and retry

- **Task ID is deterministic per plan:** `DeterministicTaskID(planID, "mark-ready", 0)`. The sidecar's ID-based dedup means re-submissions within a plan lifetime are idempotent. Because each plan has a fresh random `planID`, re-plans after plan completion get a fresh task ID — which is correct, since the new sidecar has no memory of the old plan's task.
- **Plan ID is fresh on each spawn:** `planID := uuid.New().String()`. If the first plan failed (network error in Execute), `handleTerminalPlan` clears it and the next reconcile builds a new one. No accumulation.
- **Retry cadence = reconcile cadence = 30 s.** `statusPollInterval = 30 * time.Second` (see `controller.go:33`). If a mark-ready plan fails, the next reconcile probes Healthz, observes 503 again, and spawns another plan.
- **No backoff escalation.** If the sidecar is wedged hard (e.g., broken binary), repeated `mark-ready` POSTs are still cheap and safe. A PrometheusRule alert fires on `SidecarReady=False` for > 5 min (see Observability) and paging picks it up.

### Observability

#### New SeiNode condition

```go
// api/v1alpha1/seinode_types.go
const (
    // ConditionSidecarReady reflects the last observed sidecar Healthz state.
    // True:  sidecar returned 200.
    // False: sidecar returned 503 (mark-ready gate closed; a mark-ready plan
    //        will be scheduled on the next reconcile).
    // Unknown: sidecar unreachable or returned an unexpected status; the
    //        controller will re-probe.
    ConditionSidecarReady = "SidecarReady"
)
```

Reasons: `Ready`, `NotReady`, `Unreachable`, `ProbeError`.

#### New metric

```go
// sidecar_health_probes_total{namespace, name, outcome}
// where outcome ∈ {"ready", "not_ready", "unreachable", "probe_error"}
```

Emitted by `probeSidecarHealth` on every probe. This gives operators:
- `sum by (outcome) (rate(sidecar_health_probes_total[5m]))` — probe outcomes.
- A `not_ready` rate > 0 sustained means the re-apply loop is firing repeatedly, which means the sidecar won't accept `mark-ready` successfully — a real alertable stuck state.

#### New event

| Event | Type | Reason | When |
|---|---|---|---|
| Stuck sidecar detected | Warning | `SidecarReadinessLost` | `SidecarReady` transitions True → False |
| Re-issued mark-ready | Normal | `MarkReadyReapplied` | `MarkReady` plan spawned |
| Sidecar recovered | Normal | `SidecarReadinessRestored` | `SidecarReady` transitions False → True |

Events are ephemeral but useful for `kubectl describe seinode` during an incident; the condition and metric are the durable signals.

#### Alerting (platform-side, not this PR)

The platform team's alert bundle (substrate `PrometheusRule`) will gain a rule equivalent to:

```yaml
- alert: SeiNodeSidecarReadinessLost
  expr: |
    max_over_time(
      (kube_seinode_condition_status{condition="SidecarReady", status="false"})[10m:1m]
    ) == 1
  for: 5m
  labels: { severity: warning, team: protocol }
  annotations:
    summary: "SeiNode {{$labels.namespace}}/{{$labels.name}} sidecar stuck on 503"
    runbook: "The controller is re-issuing mark-ready every 30s. If this fires, investigate the sidecar logs and pod state — see issue #127."
```

That rule goes in the platform repo's substrate overlay, not in this controller PR. Noted here so the reviewer knows it's coming.

### Status shape during a re-apply

Before (steady state, healthy):
```yaml
status:
  phase: Running
  plan: null
  conditions:
    - type: SidecarReady
      status: "True"
      reason: Ready
```

During (pod was just chaos-killed, sidecar 503 detected, plan spawned):
```yaml
status:
  phase: Running
  plan:
    id: <uuid>
    phase: Active
    targetPhase: Running
    tasks:
      - type: mark-ready
        id: <uuid>
        status: Pending
  conditions:
    - type: SidecarReady
      status: "False"
      reason: NotReady
      message: "sidecar returned 503, re-issuing mark-ready"
```

After (plan completed, next probe returned 200):
```yaml
status:
  phase: Running
  plan: null
  conditions:
    - type: SidecarReady
      status: "True"
      reason: Ready
```

## File-by-File Changes

| File | Change |
|---|---|
| `api/v1alpha1/seinode_types.go` | Add `ConditionSidecarReady` constant. |
| `internal/planner/planner.go` | Extend `buildRunningPlan` with second drift branch. Add `buildMarkReadyPlan`, `sidecarNeedsReapproval`, `setSidecarReadyCondition`. |
| `internal/planner/planner_test.go` | Unit tests: `TestBuildRunningPlan_SidecarNotReady_ReturnsMarkReadyPlan`, `TestBuildRunningPlan_ImageDriftWins`, `TestBuildRunningPlan_SidecarUnknown_NoPlan`. |
| `internal/controller/node/controller.go` | Add `probeSidecarHealth` method, called from `Reconcile` before `ResolvePlan` when `Phase == Running`. |
| `internal/controller/node/metrics.go` | Register `sidecar_health_probes_total` counter. |
| `internal/controller/node/reconciler_test.go` | Unit tests for probe outcomes and condition transitions. |
| `internal/controller/node/plan_execution_integration_test.go` | Integration test: pod-replacement round-trip (kill pod → probe 503 → plan → probe 200). |

No CRD schema changes beyond a new condition type (which is a string constant, not a field). No `make manifests` regeneration needed for new logic.

## Test Plan

### Unit tests (planner)

| Test | Assertion |
|---|---|
| `TestBuildRunningPlan_SidecarReady_NoPlan` | `SidecarReady=True` → returns `nil`. |
| `TestBuildRunningPlan_SidecarNotReady_ReturnsMarkReadyPlan` | `SidecarReady=False, Reason=NotReady` → returns single-task `MarkReady` plan with `TargetPhase=Running`, empty `FailedPhase`. |
| `TestBuildRunningPlan_SidecarUnknown_NoPlan` | `SidecarReady=Unknown` → returns `nil` (no plan — will re-probe). |
| `TestBuildRunningPlan_ImageDriftWins` | Both image drift and `SidecarReady=False` → returns image-update plan (not mark-ready plan). |
| `TestBuildMarkReadyPlan_SetsCondition` | Building the plan sets `SidecarReady=False, Reason=NotReady`. |
| `TestResolvePlan_MarkReadyPlan_Completes_ClearsPlan` | Simulated plan completion → `handleTerminalPlan` clears `status.plan`. (Condition is cleared by the next probe, not by `handleTerminalPlan`, since the *observation* lives on the reconciler side.) |

### Unit tests (reconciler)

| Test | Assertion |
|---|---|
| `TestProbeSidecarHealth_200_SetsReady` | Mock sidecar returns 200 → condition `SidecarReady=True, Reason=Ready`. |
| `TestProbeSidecarHealth_503_SetsNotReady` | Mock sidecar returns 503 → condition `SidecarReady=False, Reason=NotReady`. |
| `TestProbeSidecarHealth_NetworkError_SetsUnknown` | Mock client returns error → condition `SidecarReady=Unknown, Reason=Unreachable`. |
| `TestProbeSidecarHealth_Initializing_Skipped` | Phase=Initializing → probe not called. |
| `TestProbeSidecarHealth_MetricEmitted` | Probe emits `sidecar_health_probes_total` with correct outcome label. |

### Integration test (envtest + fake sidecar)

`plan_execution_integration_test.go`:

```
TestReconcile_PodReplacement_ReissuesMarkReady
  1. Stand up envtest with a SeiNode at Running, sidecar mock returning 200.
  2. Reconcile → SidecarReady=True, no plan.
  3. Flip sidecar mock to 503.
  4. Reconcile → SidecarReady=False, plan spawned with MarkReady task.
  5. Flip sidecar mock back to 200 (simulating mark-ready landing).
  6. Reconcile → plan completes, handleTerminalPlan clears status.plan.
  7. Reconcile → probe returns 200, condition restored to True.
  8. Assert: mock received exactly one SubmitTask(mark-ready) call.
```

This mirrors the existing integration-test pattern in `plan_execution_integration_test.go` for image-update plans — there is no real cluster, just an envtest API server plus a mock `SidecarClient`. Everything is deterministic.

### What is NOT tested automatically

- Actual pod-kill via chaos-mesh. This is verified once in the platform repo's chaos test harness (the test already exists — see issue #127's `Test that should pass after fix` section, and dev release-6-5 chaos test 04).

## Rollback and Safety

### What if the probe logic is wrong?

The probe is an HTTP GET with a 2 s timeout. Worst case it returns the wrong answer:

- **False positive (probe says 503, sidecar is actually fine):** Controller spawns a mark-ready plan. The sidecar receives a mark-ready for a task it has already processed. The sidecar's ID-based dedup short-circuits — task ID was already submitted, returns 202/already-completed, plan completes normally. **No harm.**
- **False negative (probe says 200, sidecar is actually stuck):** Controller does not spawn a plan. The node stays stuck — which is the pre-fix state. **No regression.**
- **Probe hangs:** 2 s context deadline caps it. Reconcile proceeds with `SidecarReady=Unknown`. **No harm.**

The sidecar side already accepts idempotent re-submissions of `mark-ready` — this is empirically verified by the manual workaround in #127, which is just `curl -X POST .../tasks`. The controller doing it on a 30 s cadence is the same operation, automated.

### What if the SidecarClient interface needs widening?

Currently `internal/task/task.go:137` defines:

```go
type SidecarClient interface {
    SubmitTask(ctx context.Context, req sidecar.TaskRequest) (uuid.UUID, error)
    GetTask(ctx context.Context, id uuid.UUID) (*sidecar.TaskResult, error)
}
```

The reconciler-side probe uses `SidecarClient.Healthz(ctx) (bool, error)` directly — but the `task.SidecarClient` interface doesn't expose it. Two options:

- **A.** Add `Healthz(ctx) (bool, error)` to `task.SidecarClient`. All mocks in `plan_execution_test.go` and `reconciler_test.go` get a two-line `Healthz` method.
- **B.** Bypass the narrow interface: the reconciler holds a factory `BuildSidecarClientForProbe func(*SeiNode) (*sidecar.SidecarClient, error)` that returns the concrete type (which has `Healthz`).

**Pick A.** The probe is a first-class sidecar interaction and deserves to be on the interface. The mock test-doubles are a few lines per test; the alternative (two parallel client types) is worse.

### Kill-switch

If the probe ever misbehaves in production, it can be disabled with zero code change by raising `statusPollInterval` or adding a `SEI_DISABLE_SIDECAR_PROBE=1` env flag guarded by the reconciler. A hard kill-switch is cheap to add; we can wait until it's needed rather than add it speculatively. The blast radius of a bad probe is a controller-originated `GET /v0/healthz` per 30 s per node — not enough to warrant a pre-emptive kill-switch.

## Non-Goals

- **Sidecar self-detection of existing chain data.** Called out in `inplace-update-strategy.md` as a future sidecar improvement. Independent of this fix; this fix works with or without it.
- **Handling stuck sidecar recovery (e.g., sidecar binary broken).** If the sidecar itself is wedged, `mark-ready` submissions will either timeout or be rejected. The alert rule will fire. An operator investigates. This PR does not try to auto-recover from a broken sidecar — that's not a drift, it's a failure, and the right response is paging.
- **Extending this pattern to SeiNodeDeployment-level orchestration.** The deployment controller already has its own readiness plumbing through `AwaitNodesCaughtUp` and (in `inplace-update-strategy.md`) the `ReadinessApproved` condition. This fix is scoped to the SeiNode reconciler.
- **Replacing the init-plan's terminal `MarkReady` task.** That task is still correct for initialization. This fix adds a second path for the post-init case.

## Implementation Order

1. `ConditionSidecarReady` constant in `api/v1alpha1/seinode_types.go`.
2. `Healthz` method added to `task.SidecarClient` interface, mocks updated.
3. `probeSidecarHealth` on the SeiNode reconciler, metric registered, called before `ResolvePlan` when `Phase == Running`.
4. `buildRunningPlan` extended with sidecar-readiness branch; `buildMarkReadyPlan` added.
5. Unit tests for planner branches and reconciler probe outcomes.
6. Integration test for full round-trip.
7. Events emitted on `SidecarReady` transitions.

Steps 1-3 are shippable as a status-only observation (metric + condition, no plan spawned). This is a useful intermediate state: operators can see stuck sidecars without yet auto-remediating. Step 4 turns on the auto-remediation. Splitting the PR this way is optional; the full fix is small enough to land as one PR.
