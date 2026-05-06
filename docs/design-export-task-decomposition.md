# Design: Decompose `exportTriggerScript` into controller-driven Go tasks

**Status:** Draft
**Date:** 2026-05-05
**Tracks:** sei-k8s-controller#180 — replace the bash blob in `internal/task/export_resources.go::exportTriggerScript` with proper Plan / Task / Executor tasks.
**Related:** PR sei-k8s-controller#179 (introduced the bash), PR sei-k8s-controller#183 (added `--streaming`), PR seictl#136 (sidecar `upload-file` task).

## Problem

The fork-genesis export Job's seid container runs ~60 lines of bash:

1. Wait for the sidecar's `/v0/healthz` to reach 200 (5-min wall-clock bound).
2. `seid export --streaming --streaming-file /sei/tmp/exported-state.json`.
3. POST `/v0/tasks` with `{"type":"upload-file","params":{...}}` to the sidecar over `/dev/tcp` (HTTP/1.0).
4. Poll `/v0/tasks/<id>` with `grep`/`cut` until `completed`/`failed`, exit accordingly.

This reimplements in shell what the controller's `task.SidecarClient` already does in Go (`SubmitTask`, `GetTask`, `Healthz`). Steps 3 and 4 in particular are an in-pod hand-rolled HTTP client + state machine. Operator-visible failure signals are lossy: when the bash fails it's `kubectl logs job/...` parsing rather than a typed plan condition.

## Current mechanism

Export Job is a single pod with three containers — `seid-init` (initContainer), `sei-sidecar` (native sidecar with `restartPolicy: Always`, listening on `localhost:7777`), and `seid` (main) — all mounting the same data PVC at `/sei`. Two pod-scoped properties make the bash work:

- **Shared network namespace.** Containers reach each other via `localhost`. Sidecar listens, seid POSTs.
- **Shared PVC mount.** Both containers see `/sei` identically, so seid writes the export file and the sidecar reads from the same path.

The seid distroless image has no `curl`/`wget`/`jq`, hence HTTP/1.0 over `/dev/tcp` and JSON parsing via `grep -oE` + `cut`.

## Proposed decomposition

Replace the bash with controller tasks that use the existing `SidecarClient`. Native-sidecar lifecycle is the gating constraint: the sidecar terminates when the seid main container exits, so any orchestration **after** export must run while seid is still alive. The seid container therefore parks on `sleep infinity` after a successful export, terminating only when `teardown-exporter` deletes the Job.

### Seid container command

```bash
seid export --home /sei --height $EXPORT_HEIGHT \
    --streaming --streaming-file /sei/tmp/exported-state.json.part && \
mv /sei/tmp/exported-state.json.part /sei/tmp/exported-state.json && \
sleep infinity
```

Two invariants:

- **Atomic completion signal.** `--streaming-file` writes incrementally; the file is partial throughout export. The `.part`/`.json` rename is atomic on POSIX within the same filesystem (the PVC), so observers see EITHER the partial `.part` OR the complete `.json` — never a partial `.json`. "`.json` exists" ≡ "export succeeded."
- **Failure-as-pod-exit.** `&&` short-circuits on `seid export` non-zero; container exits, native sidecar terminates, Job fails, controller's existing Job-failed path triggers plan failure. No special handling.

### Plan task list (sub-plan changes)

| # | Task | Status | Notes |
|---|------|--------|-------|
| 1 | `ensure-exporter-pvc` | unchanged | |
| 2 | `apply-bootstrap-job` | unchanged | |
| 3 | `await-bootstrap-job` | unchanged | |
| 4 | `apply-export-job` | **simplified** | seid command becomes the one above; drops `SIDECAR_PORT`/`GENESIS_*` env from seid container — those move to plan params on tasks 5–6. |
| 5 | `submit-export-upload` | **new** | Controller calls `SidecarClient.SubmitTask("upload-file", UploadFileParams{File: "/sei/tmp/exported-state.json", Bucket, Key, Region})`. Stashes returned task UUID in plan-task `result`. |
| 6 | `await-export-upload` | **new** | Controller polls `SidecarClient.GetTask(id)` until `Completed`/`Failed`. |
| 7 | `teardown-exporter` | unchanged behavior | Job deletion now also reaps the `sleep infinity`-parked seid via SIGTERM. |

`await-export-job` (the existing "Job is Complete" task) is removed — Job is intentionally never `Complete` on the happy path; teardown is the terminal state.

### Wait-then-upload in `upload-file`

The seictl `upload-file` task gains a bounded wait at the top: if `req.File` doesn't exist yet, it polls every 5s up to a `WaitTimeoutSec` (default 10 min) before opening for upload. With the atomic-rename invariant, "file exists" ≡ "export complete," so this wait is the await-export-ready primitive — no new sidecar HTTP endpoint, no separate task type. Submit happens once; the task self-handles the wait.

### S3 path

The existing `upload-file` task uses `aws-sdk-go-v2/feature/s3/transfermanager` (`seictl/sidecar/s3/client.go`). transfermanager:

- **Auto-multipart.** Default `partSize` 8 MiB. Caller passes `*os.File` as `Body`; transfermanager handles chunking and parallel part uploads.
- **Streamed reads.** `Body` is `io.Reader`; transfermanager reads in `partSize`-bounded chunks per concurrent upload. Memory footprint is bounded by `partSize × concurrency`, independent of total file size.
- **Concurrency is configurable, not auto-scaled.** Library default is 5 (conservative). Recommend raising to **20** for the export-Job upload — at 8 MiB parts × 20-way concurrency the in-flight memory is 160 MiB (sidecar pod has plenty), wire pressure is well under S3's 3,500 req/s per-prefix limit, and same-region EBS-to-S3 bandwidth is typically the bound rather than connection count. Expose as `Concurrency` field on `UploadFileRequest` so a chain-specific override is possible without a code change.
- **Resumability.** Not configured today. transfermanager supports it; out of scope for this design — if a multi-hour upload fails partway, today we restart from byte 0. Captured as an open question.

No additional implementation needed for streaming or multipart — the library handles both.

## What gets deleted

- `internal/task/export_resources.go::exportTriggerScript` const (~60 lines of bash).
- `TestExportTriggerScript_PinsContract` and `TestExportTriggerScript_PinsEngineStatusStrings` (replaced by Go-level tests on the new tasks + a string assertion on the simplified seid container args).
- The seid container's `SIDECAR_PORT`, `GENESIS_BUCKET`, `GENESIS_REGION`, `GENESIS_KEY` env vars (no longer needed in-container).
- The `await-export-job` task type if it has no other consumer.

## What gets added

- `seictl/sidecar/tasks/upload_file.go`: `WaitTimeoutSec` field on `UploadFileRequest`; bounded `os.Stat` poll loop before opening the file.
- `internal/task/fork_export.go`: `submit-export-upload` and `await-export-upload` task types + executions, modeled on the bootstrap-stage `applyBootstrapJobExecution` / `awaitJobExecution` shapes.
- `internal/planner/group.go`: emit the new task list in `BuildPlan`.
- API `Status.Fork.ExportUploadTaskID` (optional — operator visibility into which sidecar task is running). Decide post-coral.

## Open questions

1. **Multipart resumability.** transfermanager supports it but we don't configure it today. Should the upload task save the multipart upload ID to a marker file on the PVC so a controller restart can resume? Defer until we observe an actual mid-upload failure on a real ceremony.
2. **Wait timeout.** 10 min default for `WaitTimeoutSec` covers small chains. Pacific-1's actual export takes hours. Per-call override via params, or config the default higher? Default to per-call override; planner can pass `6h` matching the existing `exportStateTimeout`.
3. **`Status.Fork.ExportUploadTaskID` field.** Adds API surface for one ceremony's worth of operator-visible context. Worth it, or redundant with `kubectl describe` of the Pod?
4. **`await-export-job` deletion.** Is anything else in the codebase referencing it post-decomposition? Need to grep before deleting.

## References

- Bash being replaced: `internal/task/export_resources.go::exportTriggerScript` (~60 lines).
- `task.SidecarClient` interface: `internal/task/task.go::SidecarClient`.
- Pod-DNS sidecar client builder: `sidecar.NewSidecarClientFromPodDNS` (used by `internal/task/deployment_await.go`).
- transfermanager docs: `aws-sdk-go-v2/feature/s3/transfermanager`.
- LLD context: `docs/design-fork-genesis-export-job-lld.md` (the parent design that introduced the export Job shape).
