# Design: Fork-Genesis Export — push the lifecycle into plan generation

**Status:** Revision 3 (force-pushed to Rev 2 PR after a simplification round)
**Date:** 2026-05-05
**Tracks:** sei-k8s-controller fork-genesis architectural fix
**Related:** `internal/task/fork_export.go`, `internal/task/bootstrap_resources.go`, `seictl/sidecar/tasks/export_state.go`, `internal/planner/group.go`

## Problem

`submit-export-state` fails on every fork-genesis ceremony with:

```
exec: "seid": executable file not found in $PATH
```

Root cause: `seictl/sidecar/tasks/export_state.go:85` does `exec.CommandContext(ctx, "seid", args...)` from inside the sei-sidecar container, which is distroless and ships only `seictl`. The `seid` binary lives in a separate container in the same pod. Pod containers share network and named volumes — not filesystems. The exec lookup is structurally impossible.

## How this design evolved (Rev 1 → Rev 3)

- **Rev 1 (merged at #169)** added `Spec.OneShot bool` + `PhaseBootstrapComplete` to opt the exporter SeiNode out of the StatefulSet phase.
- **Rev 2** dropped `Spec.OneShot` after user feedback ("adding fields into the spec is a really bad smell"). Exporter no longer wrapped as a SeiNode — SND-level plan owns Jobs and PVC directly. Branch on `Spec.Genesis.Fork != nil` (already in spec). Proposed a fused `await-export-finished-and-upload` task with a sidecar in the export Job.
- **Rev 3 (this revision)** drops the sidecar from the export Job entirely. Export Job is single-container; uses an init container to copy the `seictl` static binary, and chains `seid export > file && seictl upload` in the main container. `await-export-job` becomes a standard `Job.status.succeeded` poll, identical in shape to `await-bootstrap-job`. `BootstrapPodInputs` refactor lands as its own prep PR, and a sibling `ExportJobInputs` struct stays distinct rather than fusing the two builders.

## Goals

1. Export-state phase succeeds end-to-end with no cross-container exec.
2. No new SeiNode CRD spec fields, no new lifecycle phases — branch on existing semantic information.
3. Reuse the bootstrap-Job pod-spec primitives (the seid + sei-sidecar container shape) without forcing a single fused builder for both bootstrap and export.
4. Failure modes debuggable via `kubectl get jobs` + `kubectl logs` on the SND. Single-container export Job means one log stream.
5. Status surface on the SND tells operators whether export succeeded, where the artifact lives, and which Job to read logs from.

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
                          — the helper refactored in PR A. seid + sei-sidecar containers
                          (existing shape). seid runs `seid start --halt-height N`.

await-bootstrap-job       Polls Job.status.succeeded == 1.

apply-export-job          Job <group>-exporter-export. Uses new `GenerateExportJob(ExportJobInputs)`
                          — single seid container, init container copies seictl binary into a
                          shared emptyDir. Main container chains:
                            seid export --home /sei --height N > /sei/tmp/exported-state.json
                              && seictl upload --file /sei/tmp/exported-state.json \
                                              --bucket <genesis> --key <chain>/exported-state.json
                                              --region <region>

await-export-job          Same task type/code shape as await-bootstrap-job, parameterized by
                          JobName. Reads pod.Status.ContainerStatuses[0].State.Terminated.ExitCode
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

### A4. Refactor `GenerateBootstrapJob` to take `BootstrapPodInputs` as its own PR (PR A)

Today: `GenerateBootstrapJob(node *SeiNode, snap *SnapshotSource, platformCfg) (*batchv1.Job, error)` — pulls fields off the SeiNode.

PR A introduces:

```go
type BootstrapPodInputs struct {
    Name             string  // Job/Service name
    Namespace        string
    ChainID          string
    SeidImage        string  // for the seid-init container
    BootstrapImage   string  // for the main "seid" container (snap.BootstrapImage with fallback to SeidImage)
    SidecarImage     string  // resolved (with default)
    SidecarPort      int32   // resolved (with default)
    SidecarResources *corev1.ResourceRequirements
    Mode             string  // "full" | "archive" | "validator"
    HaltHeight       int64
    PVCClaimName     string  // explicit, not derived from Name — the export Job needs to
                             // mount the bootstrap-provisioned PVC by name
}
```

`GenerateBootstrapJob(in BootstrapPodInputs) (*batchv1.Job, error)`. Existing per-SeiNode caller (`bootstrap_job.go::Execute`) adds a small `nodeToInputs(node, snap)` adapter and calls the new signature.

`assertNoSigningKeyOnBootstrapPod` stays at the per-SeiNode adapter boundary — it reads `node.Spec.Validator.SigningKey.Secret.SecretName`, which doesn't exist in `BootstrapPodInputs`. The SND fork-genesis path doesn't have a SeiNode and physically cannot leak signing material, so the assertion is irrelevant on that path.

### A5. Separate `GenerateExportJob` builder, distinct from `GenerateBootstrapJob`

The export Job is structurally different from bootstrap (no sidecar, different command shape). Don't fuse the builders. Sibling helper:

```go
type ExportJobInputs struct {
    Name             string  // <group>-exporter-export
    Namespace        string
    SeidImage        string  // = fork.SourceImage
    SeictlImage      string  // pinned by digest, used by the init container
    PVCClaimName     string  // = the bootstrap PVC, mounted read-write
    ExportHeight     int64
    ChainID          string  // = fork.SourceChainID, used in the seid export command
    GenesisBucket    string  // from platformCfg
    GenesisRegion    string  // from platformCfg
    GenesisKey       string  // = "<sourceChainId>/exported-state.json"
    Mode             string  // for nodepool selection (likely "full")
    OwnerRef         metav1.OwnerReference  // SND
    EmptyDirSizeLimit *resource.Quantity     // for the seictl-binary copy volume — small (~50Mi)
}

func GenerateExportJob(in ExportJobInputs) (*batchv1.Job, error)
```

Pod shape:

```yaml
spec:
  restartPolicy: Never
  initContainers:
    - name: seictl-copy
      image: <seictl-image-by-digest>
      imagePullPolicy: IfNotPresent
      command: ["sh", "-c", "cp /usr/local/bin/seictl /seictl/seictl && chmod +x /seictl/seictl"]
      volumeMounts:
        - { name: seictl-bin, mountPath: /seictl }
  containers:
    - name: seid
      image: <seid-image>
      command: ["/bin/bash", "-c"]
      args: ["seid export --home /sei --height $EXPORT_HEIGHT > /sei/tmp/exported-state.json && /seictl/seictl upload --file /sei/tmp/exported-state.json --bucket $GENESIS_BUCKET --key $GENESIS_KEY --region $GENESIS_REGION"]
      env:
        - { name: EXPORT_HEIGHT, value: "<height>" }
        - { name: GENESIS_BUCKET, value: "..." }
        - { name: GENESIS_KEY, value: "..." }
        - { name: GENESIS_REGION, value: "..." }
        - (Pod Identity creds inherited via webhook-injected env)
      volumeMounts:
        - { name: data, mountPath: /sei }
        - { name: seictl-bin, mountPath: /seictl }
  volumes:
    - name: data
      persistentVolumeClaim: { claimName: <bootstrap PVC name> }
    - name: seictl-bin
      emptyDir:
        medium: ""        # disk, not tmpfs
        sizeLimit: 50Mi
```

`backoffLimit: 0`, no `terminationGracePeriodSeconds` tuning needed. Stderr stays on the container's stderr stream by default — only stdout is redirected to file. A seid panic remains visible in `kubectl logs`.

The shared helpers between bootstrap and export Jobs (nodepool selection, tolerations, Karpenter `do-not-disrupt` annotation, common pod labels) live as standalone functions both builders call.

### A6. `seictl upload` CLI subcommand (PR 1)

New top-level CLI subcommand in seictl. Reuses `seis3.UploaderFactory` and `seis3.ClassifyS3Error` from the existing snapshot-upload path. Distinct exit codes for downstream classification:

```
exit 0  — success
exit 10 — terminal S3 error (4xx, NoSuchBucket, AccessDenied, etc.) — operator must fix
exit 11 — retryable S3 error (5xx, throttling, transient network) — re-run might succeed
exit 1  — other (file not found, args invalid, etc.)
```

`await-export-job` on the controller side reads `pod.Status.ContainerStatuses[0].State.Terminated.ExitCode` and stamps `ForkExportComplete=False, Reason=UploadFailed (terminal|retryable)` on the SND.

The seictl HTTP `upload-file` task that earlier revisions proposed is **not built**. Just the CLI.

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

### A10. Init container `seictl` binary distribution (PR 2)

The export Job's init container uses the seictl image (pinned by digest) with `imagePullPolicy: IfNotPresent` to copy the static binary into a shared `emptyDir`. emptyDir sized at `50Mi` (binary is ~25 MB), `medium: ""` (disk, not tmpfs — multi-GB exported-state shouldn't share tmpfs). The seictl image's pin digest comes from `platform.Config` (or hardcoded near the existing `DefaultSidecarImage` constant — same image, different consumer).

## Plan-shape diff

| Before (current main) | After (Rev 3) |
|---|---|
| `create-exporter` (creates SeiNode) | `ensure-exporter-pvc` |
| (SeiNode init plan: PVC + bootstrap Job + StatefulSet) | `apply-bootstrap-job` |
| `await-exporter-running` (polls SeiNode phase) | `await-bootstrap-job` (polls Job) |
| `submit-export-state` (broken: sidecar exec seid) | `apply-export-job` |
| — | `await-export-job` (same shape as await-bootstrap-job) |
| `teardown-exporter` (deletes SeiNode) | `teardown-exporter` (deletes PVC + Jobs + Service) |
| `assemble-genesis-fork` | unchanged |

## Implementation outline

### PR A — `BootstrapPodInputs` refactor (standalone, ~150 LoC)

- `internal/task/bootstrap_resources.go`:
  - Define `BootstrapPodInputs` struct (~30 LoC).
  - Modify all 11 helper functions to take `BootstrapPodInputs` instead of `*SeiNode` (~50 LoC of mechanical s/`node.X`/`in.X`/).
- `internal/task/bootstrap_job.go`:
  - Add `nodeToBootstrapInputs(node, snap, platformCfg)` adapter (~25 LoC).
  - `Execute` calls the adapter, then `GenerateBootstrapJob(inputs)`.
  - `assertNoSigningKeyOnBootstrapPod(node, podSpec)` runs after the builder returns — unchanged signature.
- `internal/task/bootstrap_task_test.go`: tests keep `*SeiNode` fixtures; the call site routes through the adapter. Should require ~10 LoC of plumbing changes.

No fork-genesis changes. No behavior change. Standalone.

### PR 1 — `seictl upload` CLI subcommand (seictl, ~80 LoC)

- New file `seictl/upload.go` (or wherever CLI subcommands live):
  - `upload --file PATH --bucket BUCKET --key KEY --region REGION [--content-type TYPE]`
  - Uses `seis3.UploaderFactory` + `seis3.ClassifyS3Error`.
  - Distinct exit codes per A6.
- New CLI test exercising each exit-code path.
- The HTTP `upload-file` sidecar task is **not** added.

### PR 2 — Fork-genesis SND-driven plan (controller, ~250 LoC net new + ~350 LoC deleted)

Files added:
- `internal/task/fork_exporter.go` — replaces `fork_export.go`. Task types and executions for `ensure-exporter-pvc`, `apply-bootstrap-job` (SND), `await-bootstrap-job` (SND, parameterized by JobName), `apply-export-job`, `await-export-job` (SND), `teardown-exporter` (SND). Helpers: `ExporterPVCName(group)`, `ExporterBootstrapJobName(group)`, `ExporterExportJobName(group)`, `ExporterServiceName(group)`.
- `GenerateExportJob(in ExportJobInputs)` lives in this file (sibling to `GenerateBootstrapJob`).

Files modified:
- `internal/planner/group.go` (or wherever the fork sub-plan is generated): emit the new task list above.
- `internal/task/registry.go`: register new task types; remove `TaskTypeCreateExporter`, `TaskTypeAwaitExporterRunning`, `TaskTypeSubmitExportState`.
- `go.mod`: bump seictl to PR 1's release.
- `api/v1alpha1/seinodedeployment_types.go` (or wherever `Status.Fork` lives): add `ExportArtifactURL`, `ExportedHeight`, `ExportJobRef` fields.

Files deleted:
- `internal/task/fork_export.go` (entire file — the four old task types).
- `seictl/sidecar/tasks/export_state.go` references that become dangling — actual deletion lands in PR 3.

### PR 3 — seictl: delete `export-state` HTTP task

- Delete `seictl/sidecar/tasks/export_state.go` + its test.
- Remove `engine.TaskExportState` and `client.TaskTypeExportState`.
- Remove the registration in `serve.go`.

Wait until PR 2 is in a deployed controller image before merging — once PR 3 ships in a seictl release, any controller still running old `submit-export-state` against new sidecar images breaks.

## Test plan

Unit tests (PR A):
- `nodeToBootstrapInputs` produces the expected struct from a representative SeiNode (table-driven over Mode).
- Existing `bootstrap_task_test.go` cases still pass after the adapter routing.

Unit tests (PR 1):
- `seictl upload` happy path.
- Exit-code 10 on `AccessDenied`.
- Exit-code 11 on simulated 5xx.
- Exit-code 1 on file-not-found.

Unit tests (PR 2):
- `GenerateExportJob(ExportJobInputs{...})` produces correct pod spec — table-driven.
- `apply-bootstrap-job` and `apply-export-job` Execute is idempotent.
- `await-bootstrap-job` and `await-export-job` Status table cases: not-yet-succeeded → Running; succeeded → Complete; failed (with various exit codes) → Failed with stamped Reason.
- Planner: `buildForkPlan` for an SND with `Genesis.Fork` set emits the new task list; without it, no fork-export tasks appear.

Integration smoke (post-PR-2 merge, on harbor):
- Reconstitute the `eng-bdchatham` cell from git history. Apply. Watch the new flow run from `ensure-exporter-pvc` through `assemble-genesis-fork`. Verify `kubectl describe seinodedeployment` shows `ForkExportComplete=True` and `Status.Fork.ExportArtifactURL` populated.

## Execution plan

| PR | Repo | Scope | Reviewers |
|---|---|---|---|
| **A** | sei-k8s-controller | `BootstrapPodInputs` refactor in isolation, no fork-genesis changes | k8s-spec |
| **1** | seictl | `seictl upload` CLI subcommand additively | k8s-spec |
| **2** | sei-k8s-controller | New SND-driven fork-genesis plan tasks. Uses `BootstrapPodInputs` directly. Adds `GenerateExportJob`. Bumps seictl module. Adds `Status.Fork.*`. Deletes `fork_export.go` + four old task types. | k8s-spec + platform-eng |
| **3** | seictl | Delete `export-state` HTTP task | k8s-spec |

Each PR gets coral cross-review before user-tagging. PR A and PR 1 can land in parallel; PR 2 depends on both.

## Open questions

1. **Job `activeDeadlineSeconds`**: 6h (matches the previous `exportStateTimeout`)? Per-Job differentiation (bootstrap shorter, export longer)?
2. **`seictl upload` retry surface**: PR 1 ships single-attempt. If observed transient failures warrant it, expose an in-CLI `--retry-count` flag in a follow-up.
3. **`Status.Fork.ExportJobRef` cleanup timing**: stays referenced on the SND status after `teardown-exporter` deletes the Job — operators can still see "what Job ran the export," even though `kubectl logs` against it now 404s. Acceptable historical breadcrumb, or zero out on teardown?

## References

- Rev 1 (now superseded): merged at PR sei-k8s-controller#169.
- Rev 2 (force-pushed to sei-k8s-controller#170 by this revision): the SND-driven sub-plan with sidecar-in-export-Job and a fused await-and-upload task.
- Bootstrap-Job builder: `internal/task/bootstrap_resources.go:45-80` (`GenerateBootstrapJob`).
- Bootstrap-Service builder: `internal/task/bootstrap_resources.go:91-109` (`GenerateBootstrapService`).
- Existing fork-export tasks (to be deleted): `internal/task/fork_export.go`.
- Coral round on Rev 3: kubernetes-specialist + platform-engineer aligned. Refinements folded into A4–A10 above.
