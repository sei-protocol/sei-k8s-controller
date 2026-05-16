# Design: SeiNode — Sidecar TLS Toggle on Running Nodes (LLD)

**Status:** Draft / LLD
**Date:** 2026-05-15
**Tracks:** prod TLS rollout (arctic-1, atlantic-2, pacific-1)
**Related:** sei-protocol/platform#545 (per-chain internal CA issuers), seictl#165 (init-only TLS gap)

## 0. Background

`spec.sidecar.tls` provisions a kube-rbac-proxy fronting the sei-sidecar API on `:8443`. Today it is only emitted by the **init plan** (`internal/planner/planner.go:531-535`):

```go
if noderesource.SidecarTLSEnabled(node) {
    prog = append(prog, task.TaskTypeApplySidecarCert, task.TaskTypeApplyRBACProxyConfig)
}
```

The Running-state drift detector (`internal/planner/planner.go:708-716`) checks only image drift + sidecar re-approval:

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
```

Adding `spec.sidecar.tls` to a Running SeiNode is silently ignored. The current workaround is `kubectl delete seinode` per child to force the init plan via the parent SND — operationally hostile for fleet rollouts.

## 0.1 Design choice: extend NodeUpdate, do not introduce a parallel plan

The kube-rbac-proxy container shares a pod with `seid`. A TLS toggle requires the same pod cycle as an image rollout — `ApplyStatefulSet → ApplyService → ReplacePod` — differing only in pre-tasks (cert + configmap provisioning) and post-tasks (which `Observe*` stamps which `status.current*` field). There is no "cycle just the sidecar" path available today.

A parallel `SidecarReprovision` plan + a separate `SidecarTLSToggleInProgress` condition were considered and rejected:
- The two plans share the entire pod-cycle middle. Duplication, not specialization.
- When both image and TLS drift simultaneously, an "image takes precedence" rule resolves TLS implicitly via `ApplyStatefulSet` regeneration but never stamps `status.currentSidecarTLS` — next reconcile fires a redundant second pod cycle.
- The `SidecarReprovision` name was speculative scaffolding for future `spec.sidecar` subfields with no concrete consumer today (YAGNI).
- Distinguishing image rollout vs. TLS toggle for observability is what `condition.reason` is for.

Both drifts flow through one extended `buildNodeUpdatePlan`. One pod cycle. One condition. Reason discriminates cause.

## 1. CRD schema changes

One new optional status field. No spec changes — the trigger is already `spec.sidecar.tls`.

```go
// api/v1alpha1/seinode_types.go — SeiNodeStatus

// CurrentSidecarTLS captures the sidecar TLS configuration observed
// running on the owned StatefulSet. Updated when the NodeUpdate plan's
// ObserveSidecarTLS task completes. Compared against spec.sidecar.tls
// to detect drift.
// +optional
CurrentSidecarTLS *SidecarTLSStatus `json:"currentSidecarTLS,omitempty"`
```

```go
// api/v1alpha1/common_types.go

// SidecarTLSStatus is the observed sidecar TLS configuration. Nil means
// TLS is not enabled on the running pod.
type SidecarTLSStatus struct {
    IssuerName string `json:"issuerName"`
    IssuerKind string `json:"issuerKind"`
}
```

No new condition constants. `ConditionNodeUpdateInProgress` covers both image rollout and TLS toggle. Reasons:

| Reason                          | When                                       |
|---------------------------------|--------------------------------------------|
| `UpdateStarted`                 | image drift only                           |
| `TLSToggleStarted`              | TLS drift only                             |
| `UpdateAndTLSToggleStarted`     | both drifts present                        |
| `UpdateComplete` / `UpdateFailed` | existing terminal reasons (unchanged)    |

Doc-comment correction on `SidecarConfig.TLS` (`api/v1alpha1/common_types.go:195-198`) — drop the "Init-only" claim.

## 2. Drift detection

Extend `buildRunningPlan` to compute both drift flags and dispatch once:

```go
// internal/planner/planner.go

func buildRunningPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
    imageDrift := node.Spec.Image != node.Status.CurrentImage
    tlsDrift   := sidecarTLSDrift(node)
    if imageDrift || tlsDrift {
        return buildNodeUpdatePlan(node, imageDrift, tlsDrift)
    }
    if sidecarNeedsReapproval(node) {
        return buildMarkReadyPlan(node)
    }
    return nil, nil
}

// sidecarTLSDrift reports whether spec.sidecar.tls and
// status.currentSidecarTLS disagree on enablement or Issuer.
//
// Enable-only for this PR: returns true when spec is set and current is
// nil or has mismatched issuer. spec=nil + current=set (disable) returns
// false here and is tracked separately (see §6).
func sidecarTLSDrift(node *seiv1alpha1.SeiNode) bool {
    spec := node.Spec.Sidecar.GetTLS() // nil-safe accessor
    if spec == nil {
        return false
    }
    cur := node.Status.CurrentSidecarTLS
    if cur == nil {
        return true
    }
    return spec.IssuerName != cur.IssuerName || spec.IssuerKind != cur.IssuerKind
}
```

## 3. Plan shape — extended `buildNodeUpdatePlan`

```go
// internal/planner/planner.go

func buildNodeUpdatePlan(node *seiv1alpha1.SeiNode, imageDrift, tlsDrift bool) (*seiv1alpha1.TaskPlan, error) {
    setNodeUpdateCondition(node, metav1.ConditionTrue,
        nodeUpdateReason(imageDrift, tlsDrift),
        nodeUpdateMessage(node, imageDrift, tlsDrift))

    var prog []string
    if tlsDrift && noderesource.SidecarTLSEnabled(node) {
        prog = append(prog,
            task.TaskTypeApplySidecarCert,        // SSA Certificate
            task.TaskTypeWaitForSidecarTLSSecret, // poll until tls.crt populated
            task.TaskTypeApplyRBACProxyConfig,    // SSA ConfigMap
        )
    }
    prog = append(prog,
        task.TaskTypeApplyStatefulSet, // full-spec regeneration
        task.TaskTypeApplyService,     // adds :8443 when TLS enabled
        task.TaskTypeReplacePod,       // single pod cycle covers both
    )
    if imageDrift {
        prog = append(prog, task.TaskTypeObserveImage)
    }
    if tlsDrift {
        prog = append(prog, task.TaskTypeObserveSidecarTLS)
    }
    prog = append(prog, sidecar.TaskTypeMarkReady)

    planID := uuid.New().String()
    tasks := make([]seiv1alpha1.PlannedTask, len(prog))
    for i, taskType := range prog {
        t, err := buildPlannedTask(planID, taskType, i, paramsForTaskType(node, taskType, nil, nil))
        if err != nil {
            return nil, err
        }
        tasks[i] = t
    }
    return &seiv1alpha1.TaskPlan{
        ID:          planID,
        Phase:       seiv1alpha1.TaskPlanActive,
        Tasks:       tasks,
        TargetPhase: seiv1alpha1.PhaseRunning,
        // FailedPhase empty: failure retries, does not transition out of Running.
    }, nil
}
```

`nodeUpdateReason` returns `UpdateStarted` / `TLSToggleStarted` / `UpdateAndTLSToggleStarted` based on the flags. `nodeUpdateMessage` includes spec/current diffs for whichever drift(s) fired.

`WaitForSidecarTLSSecret` is **load-bearing**. Cert-manager issuance is async (seconds to minutes). Without the wait, `ApplyStatefulSet` schedules a pod whose proxy container references a not-yet-existing Secret; kubelet fails the mount, backoff escalates to 5min, the pod looks broken to operators. With the wait, the StatefulSet apply happens only when the Secret is present.

Co-drift case (image + TLS in the same edit): both blocks of pre-tasks (none for image) and post-tasks (`ObserveImage` + `ObserveSidecarTLS`) run in one plan. One pod cycle. Both `status.currentImage` and `status.currentSidecarTLS` get stamped before the plan terminates. No redundant second reconcile.

`classifyPlan` (`internal/planner/planner.go:209`) detects on `TaskTypeObserveImage` today. Extend it to also detect `TaskTypeObserveSidecarTLS` so plan-duration metrics get a `node-update-tls` or `node-update-tls+image` label rather than mislabeling TLS-only plans as `init`.

## 4. New task: `WaitForSidecarTLSSecret`

```go
// internal/task/wait_for_sidecar_tls_secret.go

const TaskTypeWaitForSidecarTLSSecret = "wait-for-sidecar-tls-secret"

type WaitForSidecarTLSSecretParams struct {
    NodeName  string `json:"nodeName"`
    Namespace string `json:"namespace"`
}

// Execute polls until the cert-manager-managed Secret exists and carries
// non-empty tls.crt. Returns ErrTransient until ready; the executor
// retries with the standard backoff. Same task is reusable for future
// cert-rotation flows.
```

Idempotent. Cheap. Reads only — no mutations. Registered in `internal/task/task.go` deserializer map.

## 5. New task: `ObserveSidecarTLS`

Symmetric with `ObserveImage` (which stamps `status.currentImage`). Verifies the live pod's containers include `kube-rbac-proxy` when TLS is enabled, then stamps `status.currentSidecarTLS` to match `spec.sidecar.tls`. `handleTerminalPlan` clears `ConditionNodeUpdateInProgress` on plan completion (existing path — no change).

```go
// internal/task/observe_sidecar_tls.go

const TaskTypeObserveSidecarTLS = "observe-sidecar-tls"
```

Registered in `internal/task/task.go` deserializer map.

## 6. Out of scope (deferred, tracked)

- **Disable path** (`tls: set → nil`): adds `DeleteSidecarCert` + `DeleteRBACProxyConfig` cleanup tasks and an additional `sidecarTLSDrift` branch for the `spec=nil, current!=nil` case. ~30 LOC. Deferred: immediate prod use case is enable-only (arctic-1/atlantic-2/pacific-1 onto the new per-chain CAs). Risk: `spec.sidecar.tls = nil` on a TLS-enabled Running node is currently silently ignored (drift detector skips it); the Cert + ConfigMap persist until SeiNode delete cascades them via owner-ref. Tracked as follow-up issue.
- **Generalize drift detector to other `spec.sidecar` subfields** (Image, Port, Resources): today none triggers any drift handler. Adding them is a `tlsDrift`-style flag pattern — no plan-shape changes needed.
- **Issuer swap**: covered organically by §2 (`IssuerName != cur.IssuerName` triggers a NodeUpdate plan). Tested but not foregrounded.

## 7. Cross-cutting (call out, not in this PR)

- **SND fleet blast radius**: enabling TLS on an SND template triggers N concurrent SeiNode reconciles. The SND's existing `maxUnavailable` orchestration applies — this design doesn't change that surface. Confirm prod archive SND has appropriate maxUnavailable before fleet rollout.
- **cert-manager throughput**: N pods issuing certs from one CA Issuer is fine (per-chain CA Issuer is local, instant). Would matter for ACME, not for the design landed in platform#545.

## 8. Verification

- Unit: `sidecarTLSDrift` matrix over `(spec=nil/set, current=nil/set, issuer match/mismatch)`; spec-nil rows return false (disable deferred).
- Unit: plan ordering — `WaitForSidecarTLSSecret` between `ApplySidecarCert` and `ApplyStatefulSet`; `ObserveImage` / `ObserveSidecarTLS` correctly gated on flags.
- Unit: co-drift case — `buildNodeUpdatePlan(node, true, true)` emits cert pre-tasks, both observers, single pod cycle.
- Unit: condition `reason` discrimination across the three drift combinations.
- Unit: idempotency — re-entering the plan mid-flight after `ApplyStatefulSet` is safe (tasks are SSA).
- Unit: `classifyPlan` correctly labels TLS-only and TLS+image plans for metrics.
- envtest: full reconcile from spec edit → plan persisted → tasks executed → pod recreated with proxy container → `currentSidecarTLS` stamped → condition cleared.
- End-to-end: enable TLS on a harbor test chain's running SeiNode via spec edit; assert pod cycles cleanly and serves on `:8443`; assert subsequent reconciles are no-ops.