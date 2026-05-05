# Design: Fork-Genesis Export — push the lifecycle into plan generation

**Status:** Revision 4 (post-PR-A merge; pivots PR 1 from CLI to sidecar task)
**Date:** 2026-05-05
**Tracks:** sei-k8s-controller fork-genesis architectural fix
**Related:** `internal/task/fork_export.go`, `internal/task/bootstrap_resources.go`, `seictl/sidecar/tasks/export_state.go`, `internal/planner/group.go`

## Problem

`submit-export-state` fails on every fork-genesis ceremony with:

```
exec: "seid": executable file not found in $PATH
```

Root cause: `seictl/sidecar/tasks/export_state.go:85` does `exec.CommandContext(ctx, "seid", args...)` from inside the sei-sidecar container, which is distroless and ships only `seictl`. The `seid` binary lives in a separate container in the same pod. Pod containers share network and named volumes — not filesystems. The exec lookup is structurally impossible.

## How this design evolved (Rev 1 → Rev 4)

- **Rev 1 (merged at #169)** added `Spec.OneShot bool` + `PhaseBootstrapComplete` to opt the exporter SeiNode out of the StatefulSet phase.
- **Rev 2** dropped `Spec.OneShot` after user feedback ("adding fields into the spec is a really bad smell"). Exporter no longer wrapped as a SeiNode — SND-level plan owns Jobs and PVC directly. Branch on `Spec.Genesis.Fork != nil` (already in spec). Proposed a fused `await-export-finished-and-upload` task with a sidecar in the export Job.
- **Rev 3 (merged at #170)** dropped the sidecar from the export Job. Export Job became single-container with a `seictl-copy` initContainer; seid container chained `seid export > file && seictl upload`. `BootstrapPodInputs` refactor (PR A) landed standalone.
- **Rev 4 (this revision)** drops the `seictl upload` CLI subcommand and the `seictl-copy` initContainer. The export Job re-introduces a native `sei-sidecar` container; the seid container triggers a `upload-file` sidecar HTTP task and polls for completion. Reasoning: a generic file-upload CLI is outside seictl's scope. seictl's CLI is for sei-chain-specific operations (config, genesis, patch, await, report); generic ops belong as sidecar HTTP tasks alongside the existing `snapshot-upload` family. The cost is a longer seid-container shell script that POSTs and polls via bash `/dev/tcp` — same primitive the existing `bootstrapWaitCommand` already uses for healthchecks. PR A merged unchanged; only PR 1 and PR 2 are reshaped.

## Goals

1. Export-state phase succeeds end-to-end with no cross-container exec.
2. No new SeiNode CRD spec fields, no new lifecycle phases — branch on existing semantic information.
3. Reuse the bootstrap-Job pod-spec primitives (the seid-init + sei-sidecar + seid container shape) without forcing a single fused builder for both bootstrap and export.
4. Failure modes debuggable via `kubectl get jobs` + `kubectl logs` on the SND. Two log streams per export Job (seid + sei-sidecar) — same as bootstrap Jobs.
5. Status surface on the SND tells operators whether export succeeded, where the artifact lives, and which Job to read logs from.
6. Keep seictl's CLI surface focused on sei-chain-specific verbs. Generic file/S3 operations live in the sidecar's HTTP task registry.

## Non-goals

1. Generalizing the export pattern to non-fork use cases.
2. Changing the S3 artifact path or format. `assemble-genesis-fork` reads the same `<sourceChainId>/exported-state.json` it reads today.
3. Validator-side plan changes. Identity/gentx generation and `configure-genesis` polling stay identical.

## Architectural decisions

### A1. Exporter is not a SeiNode — SND owns Jobs and PVC directly

The current `create-exporter` task creates a SeiNode CRD object that goes through the standard init plan (bootstrap Job → StatefulSet → Running). Rev 3 deletes that pattern. The SND-level fork-genesis sub-plan creates the PVC, the bootstrap Job, the export Job, and the headless Service directly — owner-referenced to the SND.

Why: the exporter's "lifecycle" only exists from the controller's point of view. The user never queries `kubectl get seinode <exporter>`. Modeling it as a SeiNode forces invariants (`Phase`, `Conditions`, `ResolvePlan` drift detection, NodeUpdate plans) that we then have to gate. Modeling it as Jobs avoids that ceremony entirely.

### A2. SND fork-genesis sub-plan

```
ensure-exporter-pvc       PVC <group>-exporter-data, owner-ref'd to SND. Size + storageClassName
                          copied from `Spec.Template.Spec.DataVolume`. RWO.

apply-bootstrap-job       Job <group>-exporter-bootstrap. Uses `GenerateBootstrapJob(BootstrapPodInputs)`
                          — the helper merged in PR A. seid-init + sei-sidecar + seid containers
                          (bootstrap shape). seid runs `seid start --halt-height N`.

await-bootstrap-job       Polls Job.status.succeeded == 1.

apply-export-job          Job <group>-exporter-export. Uses new `GenerateExportJob(ExportJobInputs)`.
                          Same three-container shape as the bootstrap Job. The sei-sidecar
                          container has the `upload-file` HTTP task registered. The seid main
                          container runs `seid export > /sei/tmp/exported-state.json`, then
                          POSTs `upload-file` to the sidecar over localhost, polls until terminal,
                          and exits with a code derived from the task's `Retryable` flag.

await-export-job          Same task type/code shape as await-bootstrap-job, parameterized by
                          JobName. Reads pod.Status.ContainerStatuses[seid].State.Terminated.ExitCode
                          on failure to set Reason.

teardown-exporter         Deletes PVC + both Jobs explicitly. K8s cascade-GC from SND deletion is
                          the abandonment-path safety net.

assemble-genesis-fork     Unchanged. Reads <sourceChainId>/exported-state.json from S3.

collect-and-set-peers     Unchanged.

await-nodes-running       Unchanged.
```

Validator-side init plans untouched.

### A3. Owner-refs to the SND (not a SeiNode)

`ctrl.SetControllerReference(group, job, scheme)` for each of: PVC, bootstrap Job, export Job, headless Service.

### A4. `BootstrapPodInputs` (merged in PR A — recorded for context)

PR A merged at sei-k8s-controller#173 with the following struct shape:

```go
type BootstrapPodInputs struct {
    Name                 string
    Namespace            string
    ChainID              string
    Image                string  // single seid image (init + main)
    SidecarImage         string
    SidecarPort          int32
    SidecarResources     *corev1.ResourceRequirements
    Mode                 string  // "full" | "archive" | "validator"
    HaltHeight           int64
    ForbiddenSecretNames []string  // signing-key + node-key Secrets — fail closed if mounted
}
```

`GenerateBootstrapJob(BootstrapPodInputs, platform.Config) (*batchv1.Job, error)` enforces validator-identity invariants internally via `rejectForbiddenSecretMounts`. The per-SeiNode `nodeToBootstrapInputs(node, snap)` adapter populates `ForbiddenSecretNames` with the validator's signing-key and node-key Secret names.

The SND fork-genesis path constructs `BootstrapPodInputs` directly with `ForbiddenSecretNames: nil` — there is no validator in scope and the builder still fails closed if any future change inadvertently adds one.

### A5. `GenerateExportJob` builder

The export Job's pod shape is structurally identical to the bootstrap Job's: `seid-init` + `sei-sidecar` (native sidecar) + `seid` (main). What differs is the seid container's command and the sidecar tasks the seid container triggers. Don't fuse the builders — sibling helper:

```go
type ExportJobInputs struct {
    Name              string  // <group>-exporter-export
    Namespace         string
    ChainID           string  // = fork.SourceChainID
    SeidImage         string  // = fork.SourceImage
    SidecarImage      string  // resolved from platform.Config (same as bootstrap path)
    SidecarPort       int32   // resolved from platform.Config
    SidecarResources  *corev1.ResourceRequirements
    Mode              string  // for nodepool selection — "full" suffices for export
    PVCClaimName      string  // = the bootstrap-stage PVC, mounted RW
    ExportHeight      int64
    GenesisBucket     string  // from platform.Config
    GenesisRegion     string  // from platform.Config
    GenesisKey        string  // = "<sourceChainId>/exported-state.json"
}

func GenerateExportJob(ExportJobInputs, platform.Config) (*batchv1.Job, error)
```

Pod shape:

```yaml
spec:
  restartPolicy: Never
  hostname: <Name>-0
  subdomain: <Name>             # so sidecar URL is <Name>-0.<Name>.<ns>.svc...
  shareProcessNamespace: true
  initContainers:
    - name: seid-init
      image: <SeidImage>
      command: ["/bin/sh", "-c", "seid init <ChainID> --home /sei --overwrite || true; mkdir -p /sei/tmp"]
      volumeMounts:
        - { name: data, mountPath: /sei }
    - name: sei-sidecar           # native sidecar — restartPolicy: Always
      image: <SidecarImage>
      restartPolicy: Always
      command: ["seictl", "serve"]
      env:
        - { name: SEI_CHAIN_ID,       value: <ChainID> }
        - { name: SEI_SIDECAR_PORT,   value: "<SidecarPort>" }
        - { name: SEI_HOME,           value: /sei }
        - { name: SEI_GENESIS_BUCKET, value: <GenesisBucket> }
        - { name: SEI_GENESIS_REGION, value: <GenesisRegion> }
        # SEI_SNAPSHOT_BUCKET / SEI_SNAPSHOT_REGION inherited if needed
      volumeMounts:
        - { name: data, mountPath: /sei }
  containers:
    - name: seid
      image: <SeidImage>
      command: ["/bin/bash", "-c"]
      args: ["<seid-export-and-trigger-script — see below>"]
      env:
        - { name: TMPDIR,         value: /sei/tmp }
        - { name: EXPORT_HEIGHT,  value: "<ExportHeight>" }
        - { name: GENESIS_BUCKET, value: <GenesisBucket> }
        - { name: GENESIS_REGION, value: <GenesisRegion> }
        - { name: GENESIS_KEY,    value: <GenesisKey> }
        - { name: SIDECAR_PORT,   value: "<SidecarPort>" }
      volumeMounts:
        - { name: data, mountPath: /sei }
  volumes:
    - name: data
      persistentVolumeClaim: { claimName: <PVCClaimName> }
```

`backoffLimit: 0`, `restartPolicy: Never`. No `seictl-copy` init container, no `emptyDir` volume — the export Job's footprint matches the bootstrap Job's exactly.

The seid container's args is a bash script that runs the export and triggers the upload via the sidecar. Sketch (full implementation lives in `internal/task/export_resources.go`):

```bash
set -euo pipefail

# 1. Wait for the sidecar to come up (mirrors bootstrapWaitCommand healthz poll).
until (exec 3<>/dev/tcp/localhost/${SIDECAR_PORT} \
       && printf 'GET /v0/healthz HTTP/1.0\r\nHost: localhost\r\n\r\n' >&3 \
       && head -1 <&3 | grep -q ' 200 ') 2>/dev/null; do
    sleep 5
done

# 2. Run the export. Stderr stays on the container's stderr stream.
seid export --home /sei --height "${EXPORT_HEIGHT}" > /sei/tmp/exported-state.json

# 3. POST upload-file task to sidecar; capture task id from response.
BODY=$(printf '{"file":"/sei/tmp/exported-state.json","bucket":"%s","key":"%s","region":"%s"}' \
       "${GENESIS_BUCKET}" "${GENESIS_KEY}" "${GENESIS_REGION}")
TASK_ID=$(post_task localhost ${SIDECAR_PORT} /v0/tasks/upload-file "${BODY}")

# 4. Poll task status; map terminal Retryable=true → exit 11, else exit 10. Success → exit 0.
exit "$(poll_task_until_done localhost ${SIDECAR_PORT} "${TASK_ID}")"
```

`post_task` and `poll_task_until_done` are bash functions defined inline; they speak HTTP/1.0 over `/dev/tcp` and parse JSON with `grep`/`cut` (no `jq` in the seid distroless image). Same mechanic as `bootstrapWaitCommand`. Implementation captured as a module-level constant in `export_resources.go` so unit tests can pin the exact script.

The shared helpers between bootstrap and export Jobs (nodepool selection, tolerations, Karpenter `do-not-disrupt` annotation, common pod labels, PodSpec assembly) live as standalone functions both builders call.

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

Returns the standard sidecar task envelope: `{ "id": "<task-id>" }`. The task runs asynchronously inside the sidecar process; clients poll `GET /v0/tasks/<task-id>` for state. On terminal failure the response carries a populated `engine.TaskError` with `Retryable: bool`.

Implementation reuses `seis3.UploaderFactory` and `seis3.ClassifyS3Error` — same helpers the existing `snapshot-upload` task uses. Streams the file through `transfermanager.UploadObjectInput` so multi-GB exports don't sit in memory.

The seid container in the export Job consumes this task as described in §A5. Exit codes the seid container sets, derived from the polled task state:

```
exit 0  — task complete (Status: complete)
exit 10 — task failed with Retryable=false (operator must fix)
exit 11 — task failed with Retryable=true (re-run might succeed)
exit 1  — anything else (script-level error: file not found, sidecar unreachable, etc.)
```

`await-export-job` on the controller side reads `pod.Status.ContainerStatuses[<seid>].State.Terminated.ExitCode` and stamps `ForkExportComplete=False, Reason=UploadFailed (terminal|retryable)` on the SND.

The seictl HTTP `export-state` task that ran `exec seid` from inside the sidecar (the original bug) is deleted in PR 3.

### A7. SND status surface

New conditions on `SeiNodeDeployment.Status.Conditions`:

- `ForkBootstrapComplete` — set after `await-bootstrap-job` succeeds. Reasons: `HaltHeightReached`, `SnapshotRestoreFailed`, `BootstrapJobFailed`.
- `ForkExportComplete` — set after `await-export-job` succeeds. Reasons: `Uploaded`, `ExportFailed`, `UploadFailed`, `Unknown`.
- `ForkGenesisReady` — set after `assemble-genesis-fork` lands the final `<chainId>/genesis.json`.

New fields on `SeiNodeDeployment.Status.Fork`:

- `ExportArtifactURL` — `s3://<genesis-bucket>/<sourceChainId>/exported-state.json` once uploaded.
- `ExportedHeight` — the height that was exported, stamped after success.
- `ExportJobRef` — namespaced name of the export Job, so operators can `kubectl logs job/<ref>` directly from `kubectl describe seinodedeployment`.

### A8. Karpenter scheduling

Both Jobs mirror existing bootstrap Job's annotations:
- `karpenter.sh/do-not-disrupt: true` on the pod template
- Same nodeSelector/tolerations/topologySpreadConstraints copied from the SND template (both Jobs run in the same AZ to share the same RWO PVC)

Both Jobs use `restartPolicy: Never` and `backoffLimit: 0` (one-shot, fail-terminal).

### A9. Single-AZ PVC across two Jobs

The PVC is RWO and bound to a single AZ once the bootstrap Job's pod attaches it. The export Job's pod must land in the same AZ. Karpenter respects `topology.kubernetes.io/zone` on the bound PV; the second Job's pod will schedule into the same AZ or fail to schedule (which surfaces cleanly as a `Pending` pod with a `Multi-Attach`/`affinity` event). No silent corruption.

Documenting this as expected operational behavior. Chaining the two Jobs into one isn't viable: the bootstrap Job needs the sidecar running concurrently with seid (HTTP healthz gate before seid starts), and that's a sidecar-container pattern, not an init-container pattern. Init containers run sequentially.

## Plan-shape diff

| Before (current main) | After (Rev 4) |
|---|---|
| `create-exporter` (creates SeiNode) | `ensure-exporter-pvc` |
| (SeiNode init plan: PVC + bootstrap Job + StatefulSet) | `apply-bootstrap-job` |
| `await-exporter-running` (polls SeiNode phase) | `await-bootstrap-job` (polls Job) |
| `submit-export-state` (broken: sidecar exec seid) | `apply-export-job` |
| — | `await-export-job` (same shape as await-bootstrap-job) |
| `teardown-exporter` (deletes SeiNode) | `teardown-exporter` (deletes PVC + Jobs + Service) |
| `assemble-genesis-fork` | unchanged |

## Implementation outline

### PR A — `BootstrapPodInputs` refactor

Merged at sei-k8s-controller#173.

### PR 1 — Sidecar `upload-file` HTTP task (seictl, ~120 LoC + tests)

- New file `seictl/sidecar/tasks/upload_file.go`:
  - `UploadFileTask` implementing the sidecar `engine.Task` interface.
  - Params struct: `{ File, Bucket, Key, Region string }`.
  - Reuses `seis3.UploaderFactory` (default + injectable for tests) and `seis3.ClassifyS3Error`.
  - Returns the standard `engine.TaskError` shape; the sidecar's HTTP layer handles the wire envelope.
- Register the task type in `seictl/sidecar/serve.go` (or wherever the task registry lives).
- Tests modeled on `snapshot_upload_test.go`: mock `seis3.Uploader`, table-driven over success / `AccessDenied` (terminal) / `SlowDown` (retryable) / file-not-found (non-S3 error). 100% coverage of the task's branches.
- No CLI surface — the task is reachable only through `POST /v0/tasks/upload-file`.

### PR 2 — Fork-genesis SND-driven plan (controller, ~280 LoC net new + ~350 LoC deleted)

Files added:
- `internal/task/fork_exporter.go` — replaces `fork_export.go`. Task types and executions for `ensure-exporter-pvc`, `apply-bootstrap-job` (SND), `await-bootstrap-job` (SND, parameterized by JobName), `apply-export-job`, `await-export-job` (SND), `teardown-exporter` (SND). Helpers: `ExporterPVCName(group)`, `ExporterBootstrapJobName(group)`, `ExporterExportJobName(group)`, `ExporterServiceName(group)`.
- `internal/task/export_resources.go` — `GenerateExportJob(ExportJobInputs, platform.Config)`. The seid container's bash script (the trigger-and-poll body) lives as a `const` here so unit tests can pin it.

Files modified:
- `internal/planner/group.go` (or wherever the fork sub-plan is generated): emit the new task list above.
- `internal/task/registry.go`: register new task types; remove `TaskTypeCreateExporter`, `TaskTypeAwaitExporterRunning`, `TaskTypeSubmitExportState`.
- `internal/task/bootstrap_resources.go`: optionally factor `buildSeidInitContainer` / `buildSidecarContainer` out so `GenerateBootstrapJob` and `GenerateExportJob` can both call them — only if the duplication is more than ~30 LoC. Keep both builders distinct at the top level.
- `go.mod`: bump seictl to PR 1's release.
- `api/v1alpha1/seinodedeployment_types.go` (or wherever `Status.Fork` lives): add `ExportArtifactURL`, `ExportedHeight`, `ExportJobRef` fields.

Files deleted:
- `internal/task/fork_export.go` (entire file — the four old task types).

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
- `Retryable=true` path: `SlowDown` and simulated network error.
- `Retryable=false` path: `AccessDenied`, `NoSuchBucket`.
- File-not-found: returns non-classified error, sidecar surfaces it as terminal.
- Param validation: missing `File`/`Bucket`/`Key`/`Region` → 400.

Unit tests (PR 2):
- `GenerateExportJob(ExportJobInputs{...})` produces correct pod spec — table-driven; pin the seid bash script as a string assertion.
- `apply-bootstrap-job` and `apply-export-job` Execute is idempotent.
- `await-bootstrap-job` and `await-export-job` Status table cases: not-yet-succeeded → Running; succeeded → Complete; failed (with various exit codes from the seid container) → Failed with stamped Reason.
- Planner: `buildForkPlan` for an SND with `Genesis.Fork` set emits the new task list; without it, no fork-export tasks appear.

Integration smoke (post-PR-2 merge, on harbor):
- Reconstitute the `eng-bdchatham` cell from git history. Apply. Watch the new flow run from `ensure-exporter-pvc` through `assemble-genesis-fork`. Verify `kubectl describe seinodedeployment` shows `ForkExportComplete=True` and `Status.Fork.ExportArtifactURL` populated.
- Confirm both seid and sei-sidecar logs are reachable for the export Job.

## Execution plan

| PR | Repo | Scope | Reviewers | Status |
|---|---|---|---|---|
| **A** | sei-k8s-controller | `BootstrapPodInputs` refactor | k8s-spec | merged at #173 |
| **1** | seictl | Sidecar `upload-file` HTTP task | k8s-spec | pending |
| **2** | sei-k8s-controller | New SND-driven fork-genesis plan tasks. Uses `BootstrapPodInputs` directly. Adds `GenerateExportJob`. Bumps seictl module. Adds `Status.Fork.*`. Deletes `fork_export.go` + four old task types. | k8s-spec + platform-eng | pending |
| **3** | seictl | Delete `export-state` HTTP task | k8s-spec | pending |

Each PR gets coral cross-review before user-tagging. PR 1 and PR 2 can land in parallel, but PR 2 must bump to a seictl release that includes PR 1.

## Open questions

1. **Job `activeDeadlineSeconds`**: 6h (matches the previous `exportStateTimeout`)? Per-Job differentiation (bootstrap shorter, export longer)?
2. **Sidecar task retry surface**: PR 1 ships single-attempt. If observed transient failures warrant it, expose a `MaxAttempts int` field in the task params in a follow-up.
3. **`Status.Fork.ExportJobRef` cleanup timing**: stays referenced on the SND status after `teardown-exporter` deletes the Job — operators can still see "what Job ran the export," even though `kubectl logs` against it now 404s. Acceptable historical breadcrumb, or zero out on teardown?
4. **Inline bash vs external trigger script**: the seid container's POST/poll body is ~30 lines of bash. Keep it inline as a `const` in `export_resources.go`, or ship a tiny `seictl/scripts/trigger-task.sh` that the seictl image already carries and the seid container shells out to via the shared `data` PVC? Inline is simpler; external is more debuggable. Default to inline; revisit if the script grows.

## References

- Rev 1 (now superseded): merged at PR sei-k8s-controller#169.
- Rev 2 (force-pushed): the SND-driven sub-plan with sidecar-in-export-Job and a fused await-and-upload task.
- Rev 3 (merged at PR #170): single-container export Job with `seictl-copy` init + `seictl upload` CLI.
- Bootstrap-Job builder (post-PR-A): `internal/task/bootstrap_resources.go`. `BootstrapPodInputs`, `GenerateBootstrapJob`, `GenerateBootstrapService`, `rejectForbiddenSecretMounts`.
- Existing snapshot-upload sidecar task (PR 1's reference implementation): `seictl/sidecar/tasks/snapshot_upload.go` + `_test.go`.
- Existing fork-export tasks (to be deleted): `internal/task/fork_export.go`.
- `bootstrapWaitCommand` (the bash + `/dev/tcp` healthcheck pattern PR 2's seid container script extends): `internal/task/bootstrap_resources.go`.
