# Design: Fork-Genesis Export Job — replace cross-container exec with a Job pod

**Status:** Draft
**Date:** 2026-05-05
**Tracks:** sei-k8s-controller fork-genesis architectural fix
**Related:** `internal/task/fork_export.go`, `internal/task/bootstrap_resources.go`, `seictl/sidecar/tasks/export_state.go`

## Problem

The fork-genesis ceremony's `submit-export-state` plan task fails terminally on every run with:

```
exec: "seid": executable file not found in $PATH
```

Root cause at `seictl/sidecar/tasks/export_state.go:85`:

```go
cmd := exec.CommandContext(ctx, "seid", args...)
err := cmd.Run()
```

This runs **inside the sei-sidecar container** (distroless seictl image). The `seid` binary lives in a separate container in the same pod (the seid image). Pod containers share network and named volumes but **not filesystems** — each container has its own image rootfs. So the sidecar's exec lookup fails: there is no `seid` binary on its PATH, and no straightforward way to make it appear.

Verified empirically: ran the full ceremony against a fresh 205208000 snapshot, plan failed at `submit-export-state` within seconds. SeiNode reached Running phase, sidecar API was reachable, S3 credentials worked — only the cross-container exec was broken.

## Goals

1. The export-state phase succeeds when invoked by the controller, producing the same `<sourceChainId>/exported-state.json` artifact in S3 that the broken flow targeted.
2. Architectural shape mirrors the existing bootstrap-Job pattern in `internal/task/bootstrap_resources.go` rather than introducing a new orchestration primitive.
3. Failure modes are debuggable from `kubectl get jobs` + `kubectl logs` without needing to grep controller logs first.
4. No PVC `ReadWriteOnce` multi-attach pending state under any plan ordering.

## Non-goals

1. Generalizing the export pattern to non-fork use cases. The export Job is fork-genesis-specific for now.
2. Changing the S3 artifact path or format. Consumers continue to read `<sourceChainId>/exported-state.json` from `SEI_GENESIS_BUCKET`.
3. Repackaging the seid image. The Job uses the operator-supplied source image as-is.
4. Validator-side plan changes. Identity/gentx generation and `configure-genesis` polling stay identical.

## Decisions

### D1. Use a Job pod with seid + sei-sidecar containers (mirror bootstrap pattern)

Replace `submit-export-state` (sidecar HTTP-driven seid exec) with `deploy-export-job`. The new Job pod has:

- **`seid` main container** (uses `Spec.FullNode.Snapshot.BootstrapImage`, the same source image the bootstrap Job uses): runs `seid export --home /sei --height N > /sei/tmp/exported-state.json` and exits.
- **`sei-sidecar` native sidecar container** (`restartPolicy: Always`, runs `seictl serve`): controller submits a new generic `upload-file` HTTP task that streams the JSON to S3.

The Job's main container exits on success; the sidecar exits when the Job's main container terminates. Mirrors the bootstrap Job's pod structure exactly.

**Alternatives considered**:
- *Two-Jobs-in-sequence* (seid Job, then seictl-upload Job): doubles the K8s lifecycle bookkeeping with no payoff.
- *Single-container shell pipe* (`seid export | aws s3 cp -`): forfeits `seis3.ClassifyS3Error` consistency we already have for snapshot-upload, and forces aws-cli into the seid image (manual install at runtime is a footgun).

### D2. Exporter SeiNode terminates at `BootstrapComplete`, no StatefulSet

The exporter SeiNode currently goes Pending → Bootstrap → **Running** (StatefulSet). Add a new phase **`PhaseBootstrapComplete`** as a terminal-for-exporter state that the SeiNode reconciler reaches after the bootstrap Job exits when an explicit one-shot signal is set on the spec.

**Explicit signal** (avoiding implicit role inference per platform-engineer's feedback): add `Spec.OneShot bool` to `SeiNodeSpec`. The `create-exporter` task sets it to `true` on the SeiNode it creates. The SeiNode planner reads this flag and:

- Builds the existing init plan up through `await-bootstrap-complete` and stops there.
- Skips `apply-statefulset`, `apply-service`, sidecar bootstrap tasks (`config-apply` / `discover-peers` / `mark-ready`), and the Pending → Running transition.
- Phase transitions to `BootstrapComplete` instead of `Running`.

The group plan replaces `await-exporter-running` with `await-bootstrap-complete` and slots `deploy-export-job` + `await-export-job` between it and `teardown-exporter`.

**Alternatives considered**:
- *Labels-driven role inference* (existing `sei.io/role=exporter`): implicit, fragile under copy-paste.
- *Pause/resume bracket on a real StatefulSet*: starts a long-running pod that does nothing useful before being paused; PVC RWO conflict tolerable only via Karpenter same-node scheduling, brittle.
- *Drop the SeiNode wrapper for the exporter entirely*: forces re-implementing PVC provisioning + ownership + teardown at the SeiNodeDeployment level, breaks the "everything is a SeiNode" abstraction the controller relies on.

### D3. Slim seictl's `export_state.go` to a generic `upload-file` task

Drop the `seid` exec entirely. Replace the export-state task with a generic `upload-file` task with the request shape:

```go
type UploadFileRequest struct {
    Path        string `json:"path"`        // path on the sidecar's mounted PVC
    S3Bucket    string `json:"s3Bucket"`
    S3Key       string `json:"s3Key"`
    S3Region    string `json:"s3Region"`
    ContentType string `json:"contentType,omitempty"`
}
```

Reuses the existing `seis3.UploaderFactory` and `seis3.ClassifyS3Error`. Independently useful for any future on-PVC artifact upload (not just exported-state JSON).

The controller-side `deploy-export-job` task submits this `upload-file` task to the export Job's sidecar after the seid main container completes (detected via Job status, not sidecar polling).

### D4. Job owned by the exporter SeiNode

`ctrl.SetControllerReference(node, job, scheme)` so the export Job has `ownerReferences[0]` pointing at the exporter SeiNode. Cascade-delete on `teardown-exporter` removes the Job (and its pods, and the headless Service mirroring `GenerateBootstrapService`).

PVC ownership stays as today (owned by the SeiNode via `ensure-data-pvc`).

## Implementation outline

### Plan reshape

Group plan tasks (`internal/planner/group.go::buildForkPlan` or wherever the fork sub-plan is built):

| Before | After |
|---|---|
| `create-exporter` | `create-exporter` (sets `Spec.OneShot=true` on the new SeiNode) |
| `await-exporter-running` | **`await-bootstrap-complete`** (new — polls exporter SeiNode for `BootstrapComplete` phase) |
| `submit-export-state` | **`deploy-export-job`** (new — applies the export Job, owner-ref to exporter SeiNode) |
| — | **`await-export-job`** (new — polls Job `.status.succeeded == 1`, then submits `upload-file` to its sidecar, polls for completion) |
| `teardown-exporter` | `teardown-exporter` (unchanged contract — deletes SeiNode, cascades to Job + Service + PVC) |
| `assemble-genesis-fork` | `assemble-genesis-fork` (unchanged) |

Validator-side plans unchanged.

### Files added

- `internal/task/fork_export_resources.go` — new
  - `GenerateExportJob(node, params, platformCfg) (*batchv1.Job, error)` — builds the seid+sidecar Job pod modeled on `GenerateBootstrapJob`.
  - `GenerateExportService(node) *corev1.Service` — headless service mirroring `GenerateBootstrapService` so the controller has stable DNS to reach the export Job's sidecar.
  - `ExportJobName(node) string` — returns `<node>-export`.
- `seictl/sidecar/tasks/upload_file.go` — new (replaces `export_state.go`)
  - `UploadFileRequest`, `FileUploader`, `Handler()`. Uses `seis3.UploaderFactory`, `seis3.ClassifyS3Error`.

### Files modified

- `api/v1alpha1/seinode_types.go`:
  - Add `OneShot bool` to `SeiNodeSpec`.
  - Add `PhaseBootstrapComplete` constant alongside existing phases.
- `internal/controller/node/` (the SeiNode planner):
  - When `node.Spec.OneShot` is `true`, the init plan stops after `await-bootstrap-complete`. Phase transitions to `BootstrapComplete`. No StatefulSet, no service, no sidecar bootstrap.
- `internal/task/fork_export.go`:
  - `create-exporter`: set `Spec.OneShot=true` on the SeiNode it creates.
  - Replace `submit-export-state` task type + execution with `deploy-export-job` and `await-export-job` types + executions.
- `internal/task/registry.go` (or equivalent dispatcher table): swap registrations.
- `seictl/sidecar/server.go` (or equivalent task registry): swap `TaskTypeExportState` for `TaskTypeUploadFile`.
- `seictl/sidecar/client/tasks.go`: drop `TaskTypeExportState`, add `TaskTypeUploadFile`.

### Dead code to delete outright

`internal/task/fork_export.go`:
- `TaskTypeSubmitExportState` constant.
- `SubmitExportStateParams` struct.
- `submitExportStateExecution` struct + all methods (`Execute`, `Status`, `sidecarTaskID`, `deserializeSubmitExportState`).
- `exportStateTimeout` const — replaced by Job `activeDeadlineSeconds` configured at the same value.

`seictl/sidecar/tasks/export_state.go`: entire file. Replaced by `upload_file.go`.

`seictl/sidecar/tasks/`: registration of `TaskTypeExportState`. Replaced by `TaskTypeUploadFile`.

`seictl/sidecar/client/tasks.go`: `TaskTypeExportState` constant + typed wrapper if any.

## Test plan

Unit tests:
- `GenerateExportJob` produces correct pod spec (seid+sidecar containers, owner ref to SeiNode, expected env, no signing-key volume).
- SeiNode planner with `Spec.OneShot=true` builds an init plan that stops at `await-bootstrap-complete`.
- `deploy-export-job` task is idempotent on `IsAlreadyExists`.
- `await-export-job` correctly transitions through Running → Complete on Job `.status.succeeded == 1` and submitted upload-file task completion.
- Sidecar `upload-file` happy path + ClassifyS3Error coverage on PutObject failure.

Integration test (mirrors the existing bootstrap-Job test pattern):
- Create a SeiNodeDeployment with `Genesis.Fork`. Watch the exporter SeiNode reach `BootstrapComplete`, the export Job complete, the upload-file task complete, the Job + SeiNode get torn down by `teardown-exporter`, the validator plans flip Complete on `configure-genesis`.

End-to-end smoke (post-merge, on harbor):
- Reconstitute `eng-bdchatham` cell from git history (the snapshot-of-the-fork-test SND we tore down earlier).
- Apply, watch the new flow run from create-exporter through assemble-genesis-fork.
- Expected runtime: ~5 min snapshot-restore → ~5 min `seid export` (pacific-1 v6.4.3 at height 205208000 produces ~5-15 GB JSON depending on chain size) → ~1-2 min upload → ~1-2 min assemble. Validators converge shortly after.

## Open questions

1. **Export Job timeout surface**: today the `exportStateTimeout = 6 * time.Hour` lives in the controller's polling loop. When we move to a real Job, this becomes either `Job.spec.activeDeadlineSeconds` (the kubelet enforces it) or a controller-side polling timeout (the controller fails the plan task without killing the Job). Recommend: both — `activeDeadlineSeconds` for the kubelet to clean up runaway Jobs, plus a sidecar-side timeout on the upload-file task to bail if S3 upload stalls. The bootstrap pattern doesn't have this concern because `seid start --halt-height` is bounded by reaching the height.

2. **`Spec.OneShot` discoverability**: should this field be visible to operators applying SeiNodeDeployments, or hidden from the user-facing CRD surface (i.e., always set by the controller, never set by humans)? Recommend: visible but with a docstring discouraging manual use. Keeps the wire format clean and lets future use cases (e.g., one-shot data-replay nodes) reuse it.

3. **TTL on completed export Jobs**: `BootstrapJob` uses `TTLSecondsAfterFinished: 3600` (1h). For export Jobs, especially on failure, that's tight for human investigation. Recommend: 24h on success, 7d on failure (or no TTL on failure — leave to operator cleanup).

## References

- Bootstrap-Job builder: `internal/task/bootstrap_resources.go:45-80` (`GenerateBootstrapJob`)
- Bootstrap-Service builder: `internal/task/bootstrap_resources.go:91-109` (`GenerateBootstrapService`)
- Existing fork-export tasks: `internal/task/fork_export.go`
- Broken sidecar exec: `seictl/sidecar/tasks/export_state.go:85`
- Coral review (k8s-specialist + platform-eng): see session transcript dated 2026-05-04 (recommended Option A in this design).
