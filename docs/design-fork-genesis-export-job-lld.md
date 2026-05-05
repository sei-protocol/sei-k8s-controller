# Design: Fork-Genesis Export — push the lifecycle into plan generation

**Status:** Revision 4.1 (post-coral cross-review)
**Date:** 2026-05-05
**Tracks:** sei-k8s-controller fork-genesis architectural fix
**Related:** `internal/task/fork_export.go`, `internal/task/bootstrap_resources.go`, `seictl/sidecar/tasks/export_state.go`, `internal/planner/group.go`
**Prior revisions:** sei-k8s-controller#169 (Rev 1, OneShot/PhaseBootstrapComplete), #170 (Rev 3, single-container export Job + `seictl upload` CLI), #173 (PR A, `BootstrapPodInputs` refactor — merged).

## Problem

`submit-export-state` fails on every fork-genesis ceremony with:

```
exec: "seid": executable file not found in $PATH
```

Root cause: `seictl/sidecar/tasks/export_state.go:85` does `exec.CommandContext(ctx, "seid", args...)` from inside the sei-sidecar container, which is distroless and ships only `seictl`. The `seid` binary lives in a separate container in the same pod. Pod containers share network and named volumes — not filesystems. The exec lookup is structurally impossible.

## Goals

1. Export-state phase succeeds end-to-end with no cross-container exec.
2. No new SeiNode CRD spec fields, no new lifecycle phases — branch on existing semantic information.
3. Reuse the bootstrap-Job pod-spec primitives (the seid-init + sei-sidecar + seid container shape) without forcing a single fused builder for both bootstrap and export.
4. Failure modes debuggable via `kubectl get jobs` + `kubectl logs` on the SND. Two log streams per export Job (seid + sei-sidecar) — same as bootstrap Jobs.
5. Status surface on the SND tells operators which Job ran the export.
6. Keep seictl's CLI surface focused on sei-chain-specific verbs. Generic file/S3 operations live in the sidecar's HTTP task registry.

## Non-goals

1. Generalizing the export pattern to non-fork use cases.
2. Changing the S3 artifact path or format. `assemble-genesis-fork` reads the same `<sourceChainId>/exported-state.json` it reads today.
3. Validator-side plan changes. Identity/gentx generation and `configure-genesis` polling stay identical.

## Architectural decisions

### A1. Exporter is not a SeiNode — SND owns Jobs and PVC directly

The `create-exporter` task creates a SeiNode CRD object that goes through the standard init plan (bootstrap Job → StatefulSet → Running). This design deletes that pattern. The SND-level fork-genesis sub-plan creates the PVC, the bootstrap Job, the export Job, and the headless Service directly — owner-referenced to the SND.

Why: the exporter's "lifecycle" only exists from the controller's point of view. The user never queries `kubectl get seinode <exporter>`. Modeling it as a SeiNode forces invariants (`Phase`, `Conditions`, `ResolvePlan` drift detection, NodeUpdate plans) that we then have to gate. Modeling it as Jobs avoids that ceremony entirely.

### A2. SND fork-genesis sub-plan

```
ensure-exporter-pvc       PVC <group>-exporter-data, owner-ref'd to SND. Size + storageClassName
                          copied from `Spec.Template.Spec.DataVolume`. RWO.

apply-bootstrap-job       Job <group>-exporter-bootstrap. Uses `GenerateBootstrapJob(BootstrapPodInputs)`.
                          seid-init + sei-sidecar + seid containers (bootstrap shape). seid runs
                          `seid start --halt-height N`.

await-bootstrap-job       Polls Job.status.succeeded == 1.

apply-export-job          Job <group>-exporter-export. Uses new `GenerateExportJob(ExportJobInputs)`.
                          Same three-container shape as the bootstrap Job. The sei-sidecar has the
                          `upload-file` HTTP task registered. The seid main container runs
                          `seid export > /sei/tmp/exported-state.json`, POSTs `upload-file` to
                          the sidecar over localhost, polls until terminal, exits 0 on success
                          and 1 on any failure.

await-export-job          Polls the same Job-status surface as await-bootstrap-job. On Failed,
                          stamps Reason=UploadFailed.

teardown-exporter         Deletes PVC + both Jobs + Service explicitly. K8s cascade-GC from SND
                          deletion is the abandonment-path safety net.

assemble-genesis-fork     Unchanged. Reads <sourceChainId>/exported-state.json from S3.

collect-and-set-peers     Unchanged.

await-nodes-running       Unchanged.
```

Validator-side init plans untouched.

### A3. Owner-refs to the SND (not a SeiNode)

`ctrl.SetControllerReference(group, job, scheme)` for each of: PVC, bootstrap Job, export Job, headless Service.

### A4. `BootstrapPodInputs` (merged in PR A — recorded for context)

PR A merged at #173 with this shape:

```go
type BootstrapPodInputs struct {
    Name                 string
    Namespace            string
    ChainID              string
    Image                string
    SidecarImage         string
    SidecarPort          int32
    SidecarResources     *corev1.ResourceRequirements
    Mode                 string  // "full" | "archive" | "validator"
    HaltHeight           int64
    ForbiddenSecretNames []string
}
```

`GenerateBootstrapJob(BootstrapPodInputs, platform.Config) (*batchv1.Job, error)` enforces the validator-identity invariant internally via `rejectForbiddenSecretMounts`. The per-SeiNode `nodeToBootstrapInputs(node, snap)` adapter populates `ForbiddenSecretNames` with the validator's signing-key and node-key Secret names. The SND fork-genesis path constructs `BootstrapPodInputs` directly with `ForbiddenSecretNames: nil` — there is no validator in scope and the builder still fails closed if any future change inadvertently adds one.

### A5. `GenerateExportJob` builder

The export Job's pod shape is structurally identical to the bootstrap Job's: `seid-init` + `sei-sidecar` (native sidecar with `restartPolicy: Always`) + `seid` (main). What differs is the seid container's command and which sidecar tasks the seid container triggers. The two builders stay separate; PR 2 extracts a shared `buildSidecarContainer(image, port, env, resources)` helper used by both.

```go
type ExportJobInputs struct {
    Name              string  // <group>-exporter-export
    Namespace         string
    ChainID           string  // = fork.SourceChainID
    SeidImage         string  // = fork.SourceImage
    SidecarImage      string  // resolved from platform.Config (same as bootstrap path)
    SidecarPort       int32   // resolved from platform.Config
    SidecarResources  *corev1.ResourceRequirements
    Mode              string  // "full" — drives nodepool selection and resource preset
    PVCClaimName      string  // = the bootstrap-stage PVC, mounted RW
    ExportHeight      int64
    GenesisBucket     string  // from platform.Config
    GenesisRegion     string  // from platform.Config
    GenesisKey        string  // = "<sourceChainId>/exported-state.json"
}

func GenerateExportJob(ExportJobInputs, platform.Config) (*batchv1.Job, error)
```

Pod-spec assembly mirrors `buildBootstrapPodSpec`:

- `ServiceAccountName: platformCfg.ServiceAccount` — required for IRSA so the sidecar's `transfermanager` can authenticate to S3.
- `RestartPolicy: Never`. K8s 1.29+ permits a per-container `restartPolicy: Always` on the sei-sidecar (native-sidecar GA — KEP-753); the kubelet sends SIGTERM to the sidecar when the seid main container exits, and the Job's `Status.Succeeded` is determined by the seid container's exit code.
- `TerminationGracePeriodSeconds: 600`. Longer than bootstrap's 120s so a cascade-GC mid-upload (operator deletes the SND) gives the sidecar's `transfermanager` time to finalize before SIGKILL.
- `BackoffLimit: 0`, `TTLSecondsAfterFinished: 3600`. The TTL must outlast `await-export-job`'s polling window so the controller can list the Pod by Job-selector and read the seid container's `Terminated.ExitCode`.
- Same Karpenter `do-not-disrupt` annotation, tolerations, and `karpenter.sh/nodepool` affinity as the bootstrap Job (resolved from `platformCfg.NodepoolForMode(Mode)`).
- `bootstrapResourcesForMode(Mode, platformCfg)` on the seid container — unbounded `seid export` on a multi-TiB chain will OOM-kill without requests.

Sidecar env (`SEI_CHAIN_ID`, `SEI_HOME`, `SEI_GENESIS_BUCKET`, `SEI_GENESIS_REGION`, `SEI_SIDECAR_PORT`). `SEI_SNAPSHOT_BUCKET` / `SEI_SNAPSHOT_REGION` are deliberately omitted — the export Job's sidecar only needs the genesis bucket.

The seid container's args is a bash script defined as a `const` in `internal/task/export_resources.go` so unit tests pin the exact text:

```bash
set -euo pipefail

# Wait for the sidecar to come up. Bound by 5 min so we don't hang forever.
SECONDS=0
until (exec 3<>/dev/tcp/localhost/${SIDECAR_PORT} \
       && printf 'GET /v0/healthz HTTP/1.0\r\nHost: localhost\r\n\r\n' >&3 \
       && head -1 <&3 | grep -q ' 200 ') 2>/dev/null; do
    if [ "$SECONDS" -ge 300 ]; then
        echo "sidecar healthz never reached 200 after ${SECONDS}s" >&2
        exit 1
    fi
    sleep 5
done

# Run the export. Stderr stays on the container's stderr stream.
seid export --home /sei --height "${EXPORT_HEIGHT}" > /sei/tmp/exported-state.json

# POST upload-file task; capture task id.
BODY=$(printf '{"file":"/sei/tmp/exported-state.json","bucket":"%s","key":"%s","region":"%s"}' \
       "${GENESIS_BUCKET}" "${GENESIS_KEY}" "${GENESIS_REGION}")
RESP=$(post_task localhost ${SIDECAR_PORT} /v0/tasks/upload-file "${BODY}")
TASK_ID=$(echo "$RESP" | grep -oE '"id":"[^"]+"' | head -1 | cut -d'"' -f4)

# Poll task status. On terminal failure, echo the TaskResult.Error to stderr
# so kubectl logs surfaces the classified message, then exit 1.
while true; do
    RESP=$(get_task localhost ${SIDECAR_PORT} "${TASK_ID}")
    STATUS=$(echo "$RESP" | grep -oE '"status":"[^"]+"' | head -1 | cut -d'"' -f4)
    case "$STATUS" in
        complete) exit 0 ;;
        failed)
            echo "$RESP" | grep -oE '"error":"[^"]+"' | head -1 | cut -d'"' -f4 >&2
            exit 1 ;;
        *) sleep 5 ;;
    esac
done
```

`post_task` and `get_task` are bash functions (also in the const) that speak HTTP/1.0 over `/dev/tcp` — same primitive `bootstrapWaitCommand` already uses. JSON parsing is `grep`/`cut` because the seid distroless image has no `jq`. The shape matches the existing `engine.TaskResult` envelope (`status`, `error`) — no wire-format change needed in PR 1.

#### Differences from the bootstrap Job

Same three-container shape. The deltas, all in §A5:

- Sidecar env: no `SEI_SNAPSHOT_*`.
- Seid container command: `seid export → POST upload-file → poll → exit`, vs. bootstrap's `wait healthz → seid start --halt-height`.
- `TerminationGracePeriodSeconds: 600` vs. bootstrap's 120 — covers in-flight uploads on cascade-GC.
- No `ForbiddenSecretNames` to enforce — no validator in scope, builder still fails closed if any future change adds a Secret volume named like a signing key.
- Halt-height (bootstrap) → export-height (export). Same shape, different verb on seid.

### A6. Sidecar `upload-file` HTTP task (PR 1)

New sidecar HTTP task at `POST /v0/tasks/upload-file`. Body:

```json
{
  "file":   "/sei/tmp/exported-state.json",
  "bucket": "<bucket>",
  "key":    "<key>",
  "region": "<region>"
}
```

Returns the standard sidecar task envelope: `{ "id": "<task-id>" }`. The task runs asynchronously inside the sidecar process; clients poll `GET /v0/tasks/<task-id>` for state. On failure the response carries the standard `engine.TaskResult` with `status: "failed"` and `error: "<formatted TaskError>"` — the seid container's bash echoes that string to stderr before exiting 1, and the operator reads the classified message ("S3 throttled" / "access denied" / etc.) via `kubectl logs`.

Implementation reuses `seis3.UploaderFactory` and `seis3.ClassifyS3Error` — same helpers the existing `snapshot-upload` task uses. Streams the file through `transfermanager.UploadObjectInput` so multi-GB exports don't sit in memory. **No `engine.TaskResult` schema change.** Earlier drafts of this design proposed a `Retryable bool` on the wire to drive a 10/11 exit-code split; we collapsed that to exit 0/1 because nothing downstream consumed the bit (the controller doesn't auto-retry; operators read the classified message from `kubectl logs` directly).

#### Why a sidecar task and not a `seictl upload` CLI

A `seictl upload --file ... --bucket ...` CLI was the previous shape. We rejected it because seictl's CLI surface is for sei-chain-specific operations (config, genesis, patch, await, report); a generic file-uploader bloats the surface. The sidecar already runs in every Job pod, already has `seis3` helpers, and `upload-file` is a natural neighbor of the existing `snapshot-upload` task. Tradeoff: the bash POST/poll script in §A5 is ~30 lines (modeled on `bootstrapWaitCommand`). A future debug-pod use case where a human wants to upload a one-off file would benefit from the CLI shape — accepted as a follow-up trigger if anyone hits it.

### A7. SND status surface

New conditions on `SeiNodeDeployment.Status.Conditions`:

- `ForkBootstrapComplete` — set after `await-bootstrap-job` succeeds. Reasons: `HaltHeightReached`, `SnapshotRestoreFailed`, `BootstrapJobFailed`.
- `ForkExportComplete` — set after `await-export-job` succeeds. Reasons: `Uploaded`, `ExportFailed`, `UploadFailed`.
- `ForkGenesisReady` — set after `assemble-genesis-fork` lands the final `<chainId>/genesis.json`.

One new field on `SeiNodeDeployment.Status.Fork`:

- `ExportJobRef` — namespaced name of the export Job, so operators can `kubectl logs job/<ref>` directly from `kubectl describe seinodedeployment`. Populated when `apply-export-job` creates the Job; remains after `teardown-exporter` deletes it (historical breadcrumb).

`Status.Fork.ExportJobRef` populated AND `ForkExportComplete` not yet `True` is the in-progress signal. We don't add a transitional "Uploading" condition.

`ExportArtifactURL` (deterministic from `s3://${GENESIS_BUCKET}/${SourceChainID}/exported-state.json`) and `ExportedHeight` (= `Spec.Genesis.Fork.SourceHeight`) are intentionally not status fields — both are derivable from spec; stamping them on status is duplication.

### A8. Karpenter and PVC scheduling

Both Jobs reuse the bootstrap Job's scheduling annotations (`karpenter.sh/do-not-disrupt`, tolerations, nodepool affinity). The PVC is RWO and bound to a single AZ once the bootstrap Job's pod attaches it; Karpenter respects `topology.kubernetes.io/zone` on the bound PV, so the export Job's pod schedules into the same AZ or fails to schedule (a `Pending` pod with a `Multi-Attach`/`affinity` event — surfaces cleanly, no silent corruption). We don't additionally set explicit nodeAffinity on the export Job.

## Plan-shape diff

| Before (current main) | After |
|---|---|
| `create-exporter` (creates SeiNode) | `ensure-exporter-pvc` |
| (SeiNode init plan: PVC + bootstrap Job + StatefulSet) | `apply-bootstrap-job` |
| `await-exporter-running` (polls SeiNode phase) | `await-bootstrap-job` (polls Job) |
| `submit-export-state` (broken: sidecar exec seid) | `apply-export-job` |
| — | `await-export-job` |
| `teardown-exporter` (deletes SeiNode) | `teardown-exporter` (deletes PVC + Jobs + Service) |
| `assemble-genesis-fork` | unchanged |

## Implementation outline

### PR A — `BootstrapPodInputs` refactor

Merged at #173.

### PR 1 — Sidecar `upload-file` HTTP task (seictl, ~120 LoC + tests)

- New file `seictl/sidecar/tasks/upload_file.go`:
  - `UploadFileTask` implementing the sidecar `engine.Task` interface.
  - Params struct: `{ File, Bucket, Key, Region string }`. Param validation (any empty → 400) before submission.
  - Reuses `seis3.UploaderFactory` (default + injectable for tests) and `seis3.ClassifyS3Error`.
  - Returns `engine.TaskError` from the run path; the sidecar's HTTP layer formats it into `TaskResult.Error` per the existing wire contract.
- Register the task type in `seictl/sidecar/serve.go`.
- Tests modeled on `snapshot_upload_test.go`: mock `seis3.Uploader`, table-driven over success / `AccessDenied` / `SlowDown` / file-not-found. 100% coverage of the task's branches. One additional contract test pins the JSON request shape against PR 2's bash script (the bash POSTs `{"file":"...","bucket":"...","key":"...","region":"..."}` and the test asserts those keys are exactly what `UploadFileTask`'s param decoder accepts).

### PR 2 — Fork-genesis SND-driven plan (controller, ~280 LoC net new + ~350 LoC deleted)

Files added:
- `internal/task/fork_exporter.go` — replaces `fork_export.go`. Task types and executions for `ensure-exporter-pvc`, `apply-bootstrap-job` (SND), `await-bootstrap-job` (SND, parameterized by JobName), `apply-export-job`, `await-export-job` (SND), `teardown-exporter` (SND). Helpers: `ExporterPVCName(group)`, `ExporterBootstrapJobName(group)`, `ExporterExportJobName(group)`, `ExporterServiceName(group)`.
- `internal/task/export_resources.go` — `GenerateExportJob(ExportJobInputs, platform.Config)`. The seid container's bash script lives as a `const` here so unit tests can pin the exact text.

Files modified:
- `internal/task/bootstrap_resources.go`: extract `buildSidecarContainer(image, port, env, resources)` shared with the export builder. Leave `seid-init` inline in each builder (two lines each — duplication isn't worth a helper).
- `internal/planner/group.go` (or wherever the fork sub-plan is generated): emit the new task list above.
- `internal/task/registry.go`: register new task types; remove `TaskTypeCreateExporter`, `TaskTypeAwaitExporterRunning`, `TaskTypeSubmitExportState`.
- `go.mod`: bump seictl to PR 1's release.
- `api/v1alpha1/seinodedeployment_types.go`: add `ExportJobRef` field on `Status.Fork`; add the three new conditions.

Files deleted:
- `internal/task/fork_export.go` (entire file — the four old task types).

`apply-export-job` Execute is idempotent at the Job-apply layer (server-side apply, fieldOwner). The sidecar's `upload-file` task is fire-and-forget non-idempotent — but since `apply-export-job` produces exactly one Job and the seid container POSTs exactly once per Pod lifecycle (and `restartPolicy: Never` + `backoffLimit: 0` prevent Pod restart), there's no path that produces duplicate uploads.

### PR 3 — seictl: delete `export-state` HTTP task

- Delete `seictl/sidecar/tasks/export_state.go` + its test.
- Remove `engine.TaskExportState` and `client.TaskTypeExportState`.
- Remove the registration in `serve.go`.

Wait until PR 2 is in a deployed controller image before merging — once PR 3 ships in a seictl release, any controller still running old `submit-export-state` against new sidecar images breaks.

## Test plan

Unit tests (PR A — already merged):
- `nodeToBootstrapInputs` table-driven over Mode and snapshot states.
- `GenerateBootstrapJob` validation table.
- `rejectForbiddenSecretMounts` negative test.

Unit tests (PR 1):
- `UploadFileTask` happy path with mock `seis3.Uploader`.
- Failure paths: `AccessDenied`, `NoSuchBucket`, `SlowDown`, simulated network error, file-not-found. Each asserts the `TaskError.Error()` rendering contains the classified message.
- Param validation: missing `File`/`Bucket`/`Key`/`Region` → 400.
- Contract test: the JSON keys PR 2's bash POSTs exactly match `UploadFileTask`'s param decoder.

Unit tests (PR 2):
- `GenerateExportJob(ExportJobInputs{...})` produces correct pod spec — table-driven; pin the seid bash script as a string assertion.
- `apply-bootstrap-job` and `apply-export-job` Execute is idempotent.
- `await-bootstrap-job` and `await-export-job` Status table cases: not-yet-succeeded → Running; succeeded → Complete; failed (with various exit codes) → Failed with stamped Reason.
- Planner: `buildForkPlan` for an SND with `Genesis.Fork` set emits the new task list; without it, no fork-export tasks appear.
- Planner: `Status.Fork.ExportJobRef` is stamped when `apply-export-job` creates the Job and is preserved across `teardown-exporter`.

Integration smoke (post-PR-2 merge, on harbor):
- Reconstitute the `eng-bdchatham` cell from git history. Apply. Watch the new flow run from `ensure-exporter-pvc` through `assemble-genesis-fork`. Verify `kubectl describe seinodedeployment` shows `ForkExportComplete=True` and `Status.Fork.ExportJobRef` populated.
- Confirm both seid and sei-sidecar logs are reachable for the export Job.

## Execution plan

| PR | Repo | Scope | Reviewers | Status |
|---|---|---|---|---|
| **A** | sei-k8s-controller | `BootstrapPodInputs` refactor | k8s-spec | merged at #173 |
| **1** | seictl | Sidecar `upload-file` HTTP task | k8s-spec | pending |
| **2** | sei-k8s-controller | New SND-driven fork-genesis plan tasks. Uses `BootstrapPodInputs` directly. Adds `GenerateExportJob` + extracts `buildSidecarContainer`. Bumps seictl module. Adds `Status.Fork.ExportJobRef` + three conditions. Deletes `fork_export.go` + four old task types. | k8s-spec + platform-eng | pending |
| **3** | seictl | Delete `export-state` HTTP task | k8s-spec | pending |

Each PR gets coral cross-review before user-tagging. PR 1 and PR 2 can land in parallel, but PR 2 must bump to a seictl release that includes PR 1.

Decisions captured here that would otherwise be open questions:
- `activeDeadlineSeconds`: 6h on both bootstrap and export Jobs (matches the previous `exportStateTimeout`). Per-Job differentiation is a follow-up if a real failure motivates it.
- Sidecar task retry: single-attempt. Auto-retry would be a planner-level `MaxRetries` decision, not a runtime classification.
- `ExportJobRef` after teardown: kept as a historical breadcrumb. `kubectl logs` against a deleted Job 404s but the operator still knows which Job ran.
- Inline bash vs. external `trigger-task.sh`: inline `const` in `export_resources.go` (mirrors `bootstrapWaitCommand`). External script would create a cross-image version-skew failure mode the design doesn't address.

## References

- PR sei-k8s-controller#169 (Rev 1, superseded), #170 (Rev 3, force-pushed and superseded by Rev 4 force-push), #173 (PR A merged).
- Bootstrap-Job builder (post-PR-A): `internal/task/bootstrap_resources.go` — `BootstrapPodInputs`, `GenerateBootstrapJob`, `GenerateBootstrapService`, `rejectForbiddenSecretMounts`, `bootstrapWaitCommand` (the `/dev/tcp` healthcheck primitive PR 2's seid script extends).
- Existing snapshot-upload sidecar task (PR 1's reference implementation): `seictl/sidecar/tasks/snapshot_upload.go` + `_test.go`.
- Existing fork-export tasks (deleted in PR 2): `internal/task/fork_export.go`.
- `engine.TaskResult` wire envelope: `seictl/sidecar/engine/types.go`.
- K8s native sidecar GA (KEP-753): permits per-container `restartPolicy: Always` on an initContainer alongside Pod-level `restartPolicy: Never`.
