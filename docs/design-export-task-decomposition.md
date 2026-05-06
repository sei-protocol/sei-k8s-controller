# Design: Decompose `exportTriggerScript` into controller-driven Go tasks

**Verdict:** NITS-ONLY (one BLOCKER-adjacent gap noted: see "Open question 0 — exporter headless Service").

**Status:** Draft
**Date:** 2026-05-05
**Tracks:** sei-k8s-controller#180 — replace the bash blob in `internal/task/export_resources.go::exportTriggerScript` with proper Plan / Task / Executor tasks.
**Related:** sei-k8s-controller#179 (introduced the bash), sei-k8s-controller#183 (added `--streaming`), seictl#136 (sidecar `upload-file` task).

## Problem

The fork-genesis export Job's seid container runs ~60 lines of bash:

1. Wait for the sidecar's `/v0/healthz` to reach 200 (5-min wall-clock bound).
2. `seid export --streaming --streaming-file /sei/tmp/exported-state.json`.
3. POST `/v0/tasks` with `{"type":"upload-file","params":{...}}` to the sidecar over `/dev/tcp` (HTTP/1.0).
4. Poll `/v0/tasks/<id>` with `grep`/`cut` until `completed`/`failed`, exit accordingly.

This reimplements in shell what the controller's `task.SidecarClient` already does in Go (`SubmitTask`, `GetTask`, `Healthz`). Steps 3 and 4 in particular are an in-pod hand-rolled HTTP client + state machine. Operator-visible failure signals are lossy: when the bash fails it's `kubectl logs job/...` parsing rather than a typed plan condition.

## Current mechanism

The export Job is a single pod with three containers — `seid-init` (initContainer), `sei-sidecar` (native sidecar with `restartPolicy: Always`, listening on `localhost:7777`), and `seid` (main) — all mounting the same data PVC at `/sei`. Two pod-scoped properties make the bash work:

- **Shared network namespace.** Containers reach each other via `localhost`. Sidecar listens, seid POSTs.
- **Shared PVC mount.** Both containers see `/sei` identically, so seid writes the export file and the sidecar reads from the same path.

The seid distroless image has no `curl`/`wget`/`jq`, hence HTTP/1.0 over `/dev/tcp` and JSON parsing via `grep -oE` + `cut`.

## Proposed decomposition

Replace the bash with controller tasks that use the existing `SidecarClient`. The controller talks to the export pod's sidecar over the cluster network (pod DNS), not localhost — so this path requires the exporter pod to be reachable via a headless Service matching its `Subdomain`. See "Open question 0" below.

Native-sidecar lifecycle is the gating constraint: the sidecar terminates when the seid main container exits, so any orchestration **after** export must run while seid is still alive. The seid container therefore parks on `sleep infinity` after a successful export, terminating only when `teardown-exporter` deletes the Job.

### Seid container command

```bash
# Pre-clean any stale completion artifact from a prior run; --streaming-file
# does not truncate-or-create with a different name, and a leftover .json
# from a previous Job (rare, since TTLSecondsAfterFinished reaps the Job, but
# possible if the PVC is reused) would falsely satisfy "export complete".
rm -f /sei/tmp/exported-state.json /sei/tmp/exported-state.json.part

seid export --home /sei --height $EXPORT_HEIGHT \
    --streaming --streaming-file /sei/tmp/exported-state.json.part && \
mv /sei/tmp/exported-state.json.part /sei/tmp/exported-state.json && \
sleep infinity
```

Two invariants:

- **Atomic completion signal.** `--streaming-file` writes incrementally; the file is partial throughout export. The `.part`/`.json` rename is atomic on POSIX within the same filesystem (`rename(2)`; both paths live on the same PVC), so observers see EITHER the partial `.part` OR the complete `.json` — never a partial `.json`. "`.json` exists" ≡ "export succeeded."
- **Failure-as-pod-exit.** `&&` short-circuits on `seid export` non-zero; container exits, native sidecar terminates, Job fails, the existing Job-failed path on `await-export-job` triggers plan failure. No special handling.

`set -euo pipefail` is preserved at the top of the command so unexpected `rm`/`mv` errors don't get swallowed.

### Plan task list (sub-plan changes)

| # | Task | Status | Notes |
|---|------|--------|-------|
| 1 | `ensure-exporter-pvc` | unchanged | |
| 2 | `apply-exporter-service` | **new** | Creates a headless Service named `{group}-exporter` so the controller can reach the export pod via DNS (`{group}-exporter-0.{group}-exporter.{ns}.svc...`). See Open question 0. |
| 3 | `apply-bootstrap-job` | unchanged | |
| 4 | `await-bootstrap-job` | unchanged | |
| 5 | `apply-export-job` | **simplified** | seid command becomes the one above; drops `SIDECAR_PORT`/`GENESIS_*` env from the seid container — those move to plan params on tasks 6–7. |
| 6 | `submit-export-upload` | **new** | Controller calls `SidecarClient.SubmitTask(UploadFileTask{File, Bucket, Key, Region, WaitTimeoutSec})` against the exporter pod's sidecar. Uses `DeterministicTaskID(planID, "submit-export-upload", planIndex)` so resubmission on reconcile is idempotent. |
| 7 | `await-export-upload` | **new** | Controller polls `SidecarClient.GetTask(id)` until `Completed`/`Failed`. |
| 8 | `await-export-job` | **kept** | Still useful: catches seid-container crash mid-export (Job goes Failed) before `submit-export-upload` returns a sidecar timeout. Order is a runtime *or* — the executor advances on whichever terminal state arrives first. <!-- TODO: Brandon — confirm we want to keep `await-export-job` parallel-equivalent vs deleting it. The doc previously deleted it; I'm reinstating because it's the cleanest signal for "seid crashed mid-export, the .json never appears". Cost is one extra plan task; benefit is failure-signal locality. --> |
| 9 | `teardown-exporter` | unchanged behavior | Job deletion now also reaps the `sleep infinity`-parked seid via SIGTERM. |

### Wait-then-upload in `upload-file`

The seictl `upload-file` task gains a bounded wait at the top: if `req.File` doesn't exist yet, it polls every 5s up to a `WaitTimeoutSec` before opening for upload. With the atomic-rename invariant, "file exists" ≡ "export complete," so this wait is the await-export-ready primitive — no new sidecar HTTP endpoint, no separate task type. The controller submits once, the sidecar self-handles the wait.

Trade-off considered: a separate `await-file` sidecar task. Rejected — it doubles the cross-pod RPC count for one ceremony, and the wait is causally coupled to the upload (we never want one without the other). The single-task formulation has the smaller surface and the operator-readability win is moot at 7 tasks vs. 8.

`WaitTimeoutSec` has no default — the controller passes it explicitly per call. The planner sets it to the same value as `exportStateTimeout` (currently 6h) so the bound matches the surrounding plan's existing timeout discipline.

### S3 path

The existing `upload-file` task uses `aws-sdk-go-v2/feature/s3/transfermanager` (`seictl/sidecar/s3/client.go`). transfermanager:

- **Auto-multipart.** Default `partSize` 8 MiB. Caller passes `*os.File` as `Body`; transfermanager handles chunking and parallel part uploads.
- **Streamed reads.** `Body` is `io.Reader`; transfermanager reads in `partSize`-bounded chunks per concurrent upload. Memory footprint is bounded by `partSize × concurrency`, independent of total file size.
- **Concurrency is configurable, not auto-scaled.** Library default is 5. <!-- TODO: Brandon — I dropped the "raise to 20" recommendation to its own open question. We have no in-tree S3 same-region throughput observation to ground a specific number, and the snapshotter upload path (which uploads multi-TB tarballs) doesn't tune this either. Default-of-5 is conservative-but-safe; a tuning pass after the first real prod run gives us a number anchored to data, not vibes. -->

Default `partSize` (8 MiB) is **not** raised. Larger parts (32–64 MiB) reduce request count but multiply the cost of a single part-failure retry; transfermanager retries on a per-part basis, and at 8 MiB a retry costs ~few seconds even on a slow link. Keep the default.

No additional implementation needed for streaming or multipart — the library handles both.

## Deliverables

Each item is a discrete unit of work with a clear consumer. Implementation order is roughly top-to-bottom — earlier deliverables unblock later ones.

### 1. Headless Service for the exporter pod (`apply-exporter-service` task)

**What.** A new controller-side task that creates a headless `Service` named `{group}-exporter` matching the export pod's `Subdomain`. Mirrors `bootstrap_resources.go::GenerateBootstrapService`. **Why.** The bash today talks to the sidecar via `localhost:7777`; the new Go path talks via cluster DNS (`{group}-exporter-0.{group}-exporter.{ns}.svc...`), which requires a Service backing the `Subdomain`. **Nuance.** `teardown-exporter` already deletes Services by name, so cleanup is consistent. This is the load-bearing prerequisite — without it, every `submit-export-upload` would fail name resolution.

### 2. Seid container command rewrite

**What.** Replace the 60-line bash blob in `internal/task/export_resources.go::exportTriggerScript` with a four-line shell command: pre-clean stale files, run `seid export --streaming-file X.part`, atomic-rename to `X.json` on success, then `sleep infinity`. **Why.** The atomic rename is the controller's signal that export finished cleanly — `--streaming-file` writes incrementally, so plain "file exists" is meaningless during export. `sleep infinity` keeps the seid container (and therefore the native sidecar) alive after a successful export so the controller can drive the upload step. **Nuance.** `&&` short-circuits make export-failure self-terminating: container exits non-zero, native sidecar dies, Job is Failed, controller's existing Job-watch path triggers plan failure. No new failure-handling code needed. `set -euo pipefail` preserves shell discipline so `rm`/`mv` errors don't get swallowed.

### 3. `submit-export-upload` task

**What.** A new controller-side task that calls `SidecarClient.SubmitTask("upload-file", UploadFileTask{File, Bucket, Key, Region, WaitTimeoutSec})` against the exporter pod's sidecar. **Why.** Replaces the bash POST to `/v0/tasks` with a typed Go call; failures become structured plan conditions instead of `kubectl logs` parsing. **Nuance.** Uses `sidecar.NewSidecarClientFromPodDNS` directly inside the execution (same pattern as `deployment_await.go::sidecarClientForNode`) rather than `cfg.BuildSidecarClient`, because the latter is wired for SeiNode-local clients in `cmd/main.go` and can't reach the exporter pod. Deterministic task UUID derived from `(planID, "submit-export-upload", planIndex)` makes the call idempotent across reconciles — submitting twice returns the existing in-flight task ID, not a duplicate upload.

### 4. `await-export-upload` task

**What.** A new controller-side task that polls `SidecarClient.GetTask(id)` until terminal. **Why.** Replaces the bash poll loop with the same Go primitive every other sidecar-backed task uses; surfaces the sidecar's classified `engine.TaskError` (with `Retryable` and `Hint` fields) directly as plan-condition reasons rather than the lossy `grep -oE '"error"...'` path. **Nuance.** Mirrors `awaitNodesAtHeightExecution`'s shape — typed `Status()` polls and maps `sidecar.Completed → ExecutionComplete`, `sidecar.Failed → ExecutionFailed`. The task ID it polls is read from the upstream `submit-export-upload` task's stamped result.

### 5. `upload-file` task gains `WaitTimeoutSec` (seictl change)

**What.** Add a `WaitTimeoutSec` field to `UploadFileRequest` / `UploadFileTask`. When non-zero, the sidecar polls `os.Stat(req.File)` every 5s up to that bound before opening the file for upload. **Why.** With the atomic-rename invariant, "file exists" ≡ "export complete," so the wait + upload semantics are causally coupled — one task does both rather than splitting into a separate `await-file` primitive. This avoids doubling the controller's RPC count for one ceremony and keeps the sidecar's surface narrower. **Nuance.** Zero/missing preserves today's behavior (open immediately, error if missing), so existing snapshot-upload-style callers don't change semantics. Planner passes `exportStateTimeout` (6h) explicitly so the bound matches the surrounding plan's timeout discipline.

### 6. transfermanager streaming + concurrency knob

**What.** The existing `aws-sdk-go-v2/feature/s3/transfermanager` already handles chunked reads and parallel multipart uploads — no library change. Expose `Concurrency` as a new field on `UploadFileRequest` so an operator can tune the parallelism. **Why.** Multi-GB exports cannot sit fully in memory, and the upload's wall-clock can dominate the ceremony's runtime. **Nuance.** `Body` is `io.Reader`; transfermanager reads `partSize`-bounded chunks (default 8 MiB) per concurrent upload — memory is bounded by `partSize × concurrency`, independent of total file size. Library default concurrency is 5; pacific-1's first prod run is the right place to tune (we have no in-tree S3 same-region throughput measurement to ground a specific number ahead of time). Default `partSize` stays at 8 MiB — larger parts (32–64 MiB) reduce request count but multiply the cost of a single part-failure retry.

### 7. Test surface replacement

**What.** Delete `TestExportTriggerScript_PinsContract` and `TestExportTriggerScript_PinsEngineStatusStrings` (those pinned bash substrings — the bash is gone). Add Go-level tests on the two new task executions using a mock `SidecarClient` (matches `bootstrap_resources_test.go` style), plus a focused `TestExportTriggerScript_PinsCompletionContract` asserting the simplified seid Args contain the `.part`/`mv`/`sleep infinity` rename pattern — those carry the atomic-completion invariant the controller now relies on. **Why.** The load-bearing invariants moved from bash text into Go behavior; tests follow. **Nuance.** Mock-`SidecarClient` pattern is already in tree (`executor_test.go`) — no new test infrastructure.

### 8. (Kept, possibly drop) `await-export-job` task

**What.** The existing "Job is Complete" task stays in the plan. **Why.** Catches "seid crashed mid-export" earlier and more directly than waiting for `submit-export-upload`'s `WaitTimeoutSec` to expire. **Nuance.** Today's plan executor is sequential; the failure mode is also caught (less directly) by `submit-export-upload` returning a wait-timeout. If we want to drop this for a leaner plan, we get a 6h-worst-case detection latency for that specific failure mode. Open question 5 for resolution.

## What gets deleted

- `internal/task/export_resources.go::exportTriggerScript` const (~60 lines of bash). The seid container Args becomes the short pre-clean + `seid export && mv && sleep infinity` script described above; `set -euo pipefail` is the only inherited shell discipline.
- `TestExportTriggerScript_PinsContract` and `TestExportTriggerScript_PinsEngineStatusStrings`. Replaced by:
  - Go-level tests on `submit-export-upload` / `await-export-upload` executions (mock `SidecarClient`, verify request shape and status mapping).
  - A `TestExportTriggerScript_PinsCompletionContract` that asserts the seid Args contain the `.part`-then-`mv` rename pattern and the `sleep infinity` park, since these now carry the atomic-completion invariant the controller relies on.
- The seid container's `SIDECAR_PORT`, `GENESIS_BUCKET`, `GENESIS_REGION`, `GENESIS_KEY` env vars (no longer read in-container).

## What gets added

- `seictl/sidecar/tasks/upload_file.go`: `WaitTimeoutSec` field on `UploadFileRequest`; bounded `os.Stat` poll loop before opening the file. Zero/missing `WaitTimeoutSec` preserves today's behavior (open immediately, error if missing) so existing callers don't change semantics.
- `seictl/sidecar/client/tasks.go`: `WaitTimeoutSec` field on `UploadFileTask` builder + validation that it's non-negative.
- `internal/task/fork_export.go`: `submit-export-upload` and `await-export-upload` task types + `TaskExecution` implementations. Patterns:
  - `submit-export-upload` uses `cfg.BuildSidecarClient` — but **the existing `BuildSidecarClient` in `ExecutionConfig` is a zero-arg factory wired in `cmd/main.go` for SeiNode-local clients**. Submission to the *exporter pod's* sidecar needs a different builder. Approach: add `BuildExporterSidecarClient func(group *SeiNodeDeployment) (SidecarClient, error)` to `ExecutionConfig` (or, equivalently, take a pod-DNS pair `(name, namespace, port)` from params and call `sidecar.NewSidecarClientFromPodDNS` directly inside the execution — same as `internal/task/deployment_await.go::sidecarClientForNode` does today). The latter is simpler — no new field on `ExecutionConfig`, no test wiring changes — and is the recommended path.
  - `await-export-upload` mirrors the `awaitNodesAtHeightExecution` pattern: deterministic task ID derived from plan ID + task type, `Status()` polls and maps `sidecar.Completed` → `ExecutionComplete`, `sidecar.Failed` → `ExecutionFailed` carrying the engine's error message.
- `internal/task/exporter_service.go`: headless Service generator + `apply-exporter-service` task. Mirrors `bootstrap_resources.go::GenerateBootstrapService`.
- `internal/planner/group.go`: emit the new task list in `BuildPlan`.

## Open questions

0. **Exporter headless Service.** [BLOCKER-adjacent] The pod spec sets `Subdomain: <root>` but no Service is ever created. With bash-over-`localhost` this works; with controller→pod-DNS it does not — `NewSidecarClientFromPodDNS` requires a headless Service named `<root>` in the namespace for `<root>-0.<root>.<ns>.svc...` to resolve. The plan above adds an `apply-exporter-service` task; `teardown-exporter` already deletes a service by name, so cleanup is consistent. Confirm before implementation. <!-- TODO: Brandon — I added apply-exporter-service to the task table; if you'd rather sidestep this entirely (e.g., reuse the SND's existing internal Service if there is one, or do an HTTP call against the Pod IP via a `kubectl get pod` lookup), call it out. The headless-Service path is the cheapest and matches the bootstrap Job's existing pattern. -->

1. **Multipart resumability.** transfermanager supports it; we don't configure it today. Should `upload-file` save the multipart upload ID to a marker file on the PVC so a controller restart can resume? Defer until we observe an actual mid-upload failure on a real ceremony.

2. **Upload concurrency tuning.** Library default is 5. Pacific-1 export is multi-TB; same-region EBS-to-S3 likely line-rate-bounded long before per-prefix throttling kicks in (3,500 PUT/s per prefix × 8 MiB ≈ 28 GiB/s — not the bound). Expose `Concurrency` on `UploadFileRequest` so a chain-specific override is possible, but pick the default after observing the first prod run. <!-- TODO: Brandon — fpsync findings (workers=180 for gp3 saturation, per memory) suggest S3 upload concurrency could likewise need to be much higher than 5, but uploads are HTTPS-not-NVMe so the analogy is loose. Defer to first-real-run measurement. -->

3. **Upload-task wait timeout.** `WaitTimeoutSec` is per-call; planner passes `exportStateTimeout` (6h). If a future chain takes longer, raise via the existing platform config knob — no plan-task surface change.

4. **`Status.Fork.ExportUploadTaskID` field.** Adds API surface for one ceremony's worth of operator-visible context (the sidecar task UUID currently uploading). Nice-to-have but not strictly necessary — `kubectl describe job/<exporter>` plus the controller logs reach the same fact. Defer; revisit if operators report friction.

5. **`await-export-job` ordering.** Kept in the plan (see table note). Open: should it run as a parallel guard rather than sequentially after `await-export-upload`? The current plan executor is sequential; making this concurrent is a separate refactor. For now, sequential is fine — the failure mode it catches (seid crash mid-export) also surfaces as `submit-export-upload` timing out on `WaitTimeoutSec`, just less directly.

## References

- Bash being replaced: `internal/task/export_resources.go::exportTriggerScript` (~60 lines).
- `task.SidecarClient` interface: `internal/task/task.go::SidecarClient`.
- Pod-DNS sidecar client builder: `sidecar.NewSidecarClientFromPodDNS` (used by `internal/task/deployment_await.go::sidecarClientForNode`).
- transfermanager: `aws-sdk-go-v2/feature/s3/transfermanager`.
- LLD context: `docs/design-fork-genesis-export-job-lld.md` (the parent design that introduced the export Job shape).
