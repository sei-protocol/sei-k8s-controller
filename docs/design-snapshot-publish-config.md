# Design: SeiNode — Decoupling Snapshot Generation from Publishing

**Status:** Draft / RFC
**Date:** 2026-04-20
**Tracks:** (issue TBD)
**Related:** `api/v1alpha1/common_types.go` (`SnapshotGenerationConfig`), `api/v1alpha1/full_node_types.go`, `api/v1alpha1/archive_types.go`

## Problem

Today `SnapshotGenerationConfig` is a flat type with a single field — `KeepRecent` — and its presence implicitly toggles *three* distinct behaviors:

1. Set archival pruning in app.toml (`pruning = "nothing"`).
2. Set `snapshot-interval` / `snapshot-keep-recent` in app.toml so seid produces Tendermint state-sync snapshots on disk.
3. Cause the sidecar to upload completed snapshots to `{SEI_SNAPSHOT_BUCKET}/{chainID}/` in S3.

There is no way to opt into (1)+(2) without also getting (3). A full node operator who wants N local Tendermint snapshots on disk — for fast internal restarts, for a local peer to state-sync from, or while a bucket isn't provisioned yet — has no first-class way to express that. Turning the feature off entirely is the only alternative, which also disables local generation.

The shape is also **not extensible**. `SnapshotGenerationConfig` is a flat record keyed to Tendermint state-sync snapshots specifically. Any future snapshot flavor (e.g., seidb snapshots, app-layer snapshots, a different upload target, or a second publish protocol) would require overloading field names or renaming existing fields. The current type is shaped for one mode and one destination.

Both gaps trace to the same underlying issue: the config conflates *what kind of snapshot* with *what to do with it*. Pulling those apart is the refactor.

## What the user can configure today

```yaml
spec:
  fullNode:
    snapshotGeneration:
      keepRecent: 5        # <-- only knob
```

Behavior: produce Tendermint snapshots on disk, retain the last 5, and upload every completed snapshot to S3. The upload is not separately addressable.

## Shapes evaluated

| Axis | **A** — add `publishToS3: bool` to flat struct | **B** — nest under `tendermint:` + boolean toggle | **C** — nest under `tendermint:` + present/absent `publish` struct | **D** — polymorphic `snapshotMethods: []` list |
|---|---|---|---|---|
| Extensibility to new snapshot modes | Poor — adding seidb/app-layer requires renaming or mode discriminators on the flat struct | Good — new modes = new sibling sub-structs (`seidb:`, `appLayer:`) | Good — same as B | Excellent — list is ordered and naturally polymorphic |
| Extensibility of `publish` config (bucket, prefix, future protocols) | Poor — boolean can't carry config; renaming later is churn | Poor — same as A | Good — struct can grow fields (`publish.bucket`, `publish.prefix`) without renaming | Good — each item carries its own config block |
| Consistency with existing SeiNode patterns | Low | Medium | **High** — matches `stateSync: {}` and `snapshot: { s3: {...} }` presence-as-toggle used throughout `common_types.go` | Low — no precedent for polymorphic lists in this CRD |
| Ergonomics for the "local-only snapshots" case | `publishToS3: false` (explicit negation reads awkwardly) | `publish: { enabled: false }` (double-negative adjacent to an `enabled` field) | Omit `publish:` entirely — the absence is the signal | Omit the S3-publish entry |
| Ergonomics for the default / common case (publish-enabled) | Most compact (`publishToS3: true`) | Compact (`publish: { enabled: true }`) | Compact (`publish: {}`) | Verbose (full method entry) |
| CRD validation surface | Minimal | Minimal | Minimal | High — list-of-discriminated-unions requires oneOf enforcement in OpenAPI |
| Reviewability of the diff | One-line spec change | New type + one field rename | New type + new leaf type + one field rename | New list type + method discriminator + union validation |

### Why not C with a stringly-typed mode?

An alternative to B/C is a single `mode: enum` field (`TendermintStateSync`, `Seidb`, …) on a flat struct. This keeps the shape flat but moves extensibility into enum values — future modes add enum cases rather than fields. Rejected: enum discriminators force every mode to share the same config fields (`keepRecent` may not even be meaningful for seidb), defeating the purpose. Nested sub-structs keep each mode's config co-located with the mode.

## Decision

**Adopt Shape C**: rename `SnapshotGenerationConfig` to wrap a `tendermint:` sub-struct that carries mode-specific fields (`keepRecent`), plus an optional `publish:` struct whose *presence* enables S3 upload. Absence of `publish` means "generate and retain locally; do not upload."

```go
// SnapshotGenerationConfig configures snapshot generation. One or more snapshot
// modes may be enabled by setting the corresponding sub-struct. A mode sub-struct
// being absent means that snapshot type is not produced.
type SnapshotGenerationConfig struct {
    // Tendermint configures Tendermint state-sync snapshot generation.
    // +optional
    Tendermint *TendermintSnapshotGenerationConfig `json:"tendermint,omitempty"`
}

// TendermintSnapshotGenerationConfig configures a node to produce Tendermint
// state-sync snapshots. The controller sets archival pruning and a
// system-default snapshot-interval in app.toml. Snapshots are written to the
// node's data volume.
type TendermintSnapshotGenerationConfig struct {
    // KeepRecent is the number of recent snapshots to retain on disk.
    // When Publish is set, must be at least 2 so the upload algorithm can
    // select the second-to-latest completed snapshot. Otherwise must be at
    // least 1.
    // +kubebuilder:validation:Minimum=1
    KeepRecent int32 `json:"keepRecent"`

    // Publish, when set, causes the sidecar to upload completed snapshots
    // to {SEI_SNAPSHOT_BUCKET}/{chainID}/. Absence means snapshots are kept
    // on disk only and are not uploaded.
    // +optional
    Publish *TendermintSnapshotPublishConfig `json:"publish,omitempty"`
}

// TendermintSnapshotPublishConfig configures how completed Tendermint
// snapshots are uploaded. Currently an empty struct — its presence on
// TendermintSnapshotGenerationConfig enables upload to the platform
// snapshot bucket. Fields may be added here in the future (e.g., bucket
// override, prefix) without a breaking change.
type TendermintSnapshotPublishConfig struct{}
```

YAML surface:

```yaml
# Generate Tendermint snapshots, keep 5 locally, upload to S3 (current default use case)
spec:
  fullNode:
    snapshotGeneration:
      tendermint:
        keepRecent: 5
        publish: {}

# Generate Tendermint snapshots, keep 3 locally, DO NOT upload (new capability)
spec:
  fullNode:
    snapshotGeneration:
      tendermint:
        keepRecent: 3

# No snapshot generation (unchanged)
spec:
  fullNode: {}
```

### Why present/absent struct instead of `enabled: bool`

Three reasons, in order of weight:

1. **Existing precedent in this CRD.** `StateSyncSource struct{}` (`common_types.go:107`) is already used this way — setting `spec.fullNode.snapshot.stateSync: {}` enables state sync. `spec.fullNode.snapshot.s3: {...}` is the sibling case where presence enables the S3 source path. Using the same pattern here keeps the CRD internally consistent; mixing `stateSync: {}` with `publish: { enabled: true }` would be needless schema drift.
2. **Extensibility without renames.** When `publish` eventually grows fields — bucket override per-node, snapshot-prefix override, a second publish target — those fields go into the existing struct. `enabled: bool` would need to either be dropped or coexist with the new fields, producing an awkward `enabled: true, bucket: "..."` shape.
3. **No double-negative omitted-field cases.** `publish: { enabled: false }` carries the same semantic as `publish:` unset, and operators will mix the two inconsistently. The absent struct is the single canonical way to say "off."

The Kubernetes API conventions don't mandate one form over the other — both appear in core APIs — but "presence-as-enabler for an optional nested feature" is idiomatic (e.g., `PodSpec.securityContext`, `ServiceSpec.sessionAffinityConfig`). `enabled: bool` is reserved for cases where presence of the parent struct is *also* meaningful and the boolean distinguishes a substate.

### Why the outer wrapper (`SnapshotGenerationConfig`) still exists

A thinner alternative is to drop the wrapper and put `tendermint:` directly on the full-node / archive spec:

```yaml
spec:
  fullNode:
    tendermintSnapshotGeneration:
      keepRecent: 5
```

Rejected: the outer `snapshotGeneration:` groups all snapshot-producing behavior under one namespace. A future `spec.fullNode.snapshotGeneration.seidb: {...}` has an obvious home; `spec.fullNode.seidbSnapshotGeneration` would proliferate top-level fields. The wrapper is one nesting level of YAML for consistent grouping — cheap.

## Controller behavior

The controller changes are confined to three call sites that read the config plus one helper in `internal/planner/planner.go`.

### 1. Planner override logic

`internal/planner/full.go:44` and `internal/planner/archive.go:35` currently read:

```go
sg := node.Spec.FullNode.SnapshotGeneration
if sg == nil { return nil }
return seiconfig.SnapshotGenerationOverrides(sg.KeepRecent)
```

After the refactor:

```go
sg := node.Spec.FullNode.SnapshotGeneration
if sg == nil || sg.Tendermint == nil { return nil }
return seiconfig.SnapshotGenerationOverrides(sg.Tendermint.KeepRecent)
```

Archival pruning + `snapshot-interval` overrides apply whenever `tendermint` is set — identical to today's behavior when `SnapshotGeneration` was set. The override code does not care whether publishing is enabled; the overrides control what seid writes to disk.

### 2. Reuse the existing `snapshot-upload` sidecar task

The sidecar already defines `TaskTypeSnapshotUpload` and `SnapshotUploadTask{}` (seictl `sidecar/client/tasks.go`, present in pinned version v0.0.30). The task is a fire-and-forget RPC that tells the sidecar to start its continuous upload loop at its configured interval. Bucket/region/prefix are derived by the sidecar from its env — the existing `SEI_SNAPSHOT_BUCKET` / `SEI_SNAPSHOT_REGION` wiring at `internal/noderesource/noderesource.go:300-301` is unchanged.

The controller-side plumbing that used to submit this task (`planner.SnapshotUploadMonitorTask`, `planner.TaskSnapshotUpload`, the monitor-task reconcile loop) was deleted in #89 (`358a216 feat: remove monitor task system`) when the monitor-task subsystem was removed in favor of plan-driven reconciliation. The net change here is re-introducing that wiring, but as a **plan task** instead of a separate monitor-task surface:

- Register `sidecar.TaskTypeSnapshotUpload` in the controller's task registry (`internal/task/task.go`) with a `SnapshotUploadParams` wrapper, as fire-and-forget (matching `mark-ready`, `config-validate`).
- In `internal/planner/bootstrap.go`, append the `snapshot-upload` task to the init progression when `spec.*.snapshotGeneration.tendermint.publish` is present. Absence of `publish` means the task is not emitted; the sidecar never starts uploading.

Placement in the init progression: after `config-apply` / `config-validate` (so the snapshot-interval overrides are in place first) and before `mark-ready` (so the sidecar's upload loop is armed before seid starts producing snapshots).

This keeps the plan self-describing — a reader can tell from `.status.plan` whether a node is configured to publish — and it does so by using the sidecar task type that already exists, rather than inventing a new one.

### 3. `SnapshotGeneration` helper

`internal/planner/planner.go:312` currently extracts `*SnapshotGenerationConfig` from either `FullNode` or `Archive`. That helper stays; its return type doesn't change. Downstream callers reach through `.Tendermint` when they need the mode-specific config.

## Validation

Cross-field validation lives in each per-mode planner's `Validate(node)` hook — `fullNodePlanner.Validate` (`internal/planner/full.go:17`), `archiveNodePlanner.Validate` (`internal/planner/archive.go:18`). That hook already enforces semantic rules on sub-specs (e.g., `snap.BootstrapImage requires s3.TargetHeight > 0`) and is the established home for rules that the OpenAPI schema cannot express.

Not a job for the sidecar's `config-validate` task (`internal/task/config.go:33`): that task validates the **rendered** `app.toml` / `config.toml` against seid's requirements. `snapshot-keep-recent=1` is perfectly legal in seid — the "must be ≥ 2 when publishing" rule is ours, not seid's, so it does not belong at the rendered-config layer.

Rules:

- `snapshotGeneration.tendermint.keepRecent >= 1` at the schema level via kubebuilder marker (was `>= 2`).
- When `snapshotGeneration.tendermint.publish` is set, the planner `Validate(node)` enforces `keepRecent >= 2`. Rationale: the upload algorithm picks the second-to-latest completed snapshot. Without publish, retaining only the most recent is fine.
- `snapshotGeneration` with neither `tendermint` nor any other sub-struct set is treated as a validation error in `Validate(node)` — an empty `snapshotGeneration: {}` is almost certainly a user typo. Schema-level `oneOf` is an option but not required; planner-level validation is consistent with how other "pick one sub-mode" invariants are handled in this codebase.

No admission webhook is introduced.

## Migration

Per the skill brief: no launched product, no backward compat required. The existing field name (`snapshotGeneration`) is preserved; only its inner shape changes. All four in-tree samples that reference it (`manifests/samples/seinode/pacific-1-snapshotter.yaml`, `manifests/samples/seinode/pacific-1-state-syncer.yaml`, test fixtures) are updated in the same PR that lands the type change.

`make manifests generate` regenerates the CRD YAML and DeepCopy methods; no hand-editing. `make test` updates to reference the new shape.

## Rollout steps

1. Land type changes in `api/v1alpha1/common_types.go`: new `TendermintSnapshotGenerationConfig`, new empty `TendermintSnapshotPublishConfig`, rewritten `SnapshotGenerationConfig`.
2. `make manifests generate` — regenerates CRDs and DeepCopy.
3. Update planner override call sites (`internal/planner/full.go`, `internal/planner/archive.go`) to read `.Tendermint.KeepRecent`.
4. Add cross-field checks to `fullNodePlanner.Validate` and `archiveNodePlanner.Validate`: reject `snapshotGeneration: {}` with no sub-struct; require `keepRecent >= 2` when `publish` is set.
5. Re-wire the existing seictl `TaskTypeSnapshotUpload` on the controller side: add a `SnapshotUploadParams` wrapper in `internal/task/`, register it in the task registry at `internal/task/task.go` as fire-and-forget, and thread it into the init progression in `internal/planner/bootstrap.go` when `publish` is present. This restores controller-side plumbing that was deleted in #89 (`358a216`), now as a plan task rather than a monitor task.
6. Update sample manifests and tests to the new shape.
7. Update `internal/planner/planner.go:312` doc comment to note the indirection through `.Tendermint`.

## Open questions (for the LLD)

1. **Should `publish: {}` later grow a `bucket` override?** Out of scope for this design; the struct exists precisely to absorb that field later without a rename. Flag if the operator wants per-node bucket overrides imminently — affects whether `TendermintSnapshotPublishConfig` should be defined with future fields in mind now. Note: `SnapshotUploadTask` in seictl v0.0.30 currently takes no params; per-node overrides would likely require a minor bump on the sidecar as well.

2. **Archive node behavior when `publish` is absent.** Archives already disable pruning independent of `SnapshotGeneration` (per `archive_types.go` comment). Does omitting `publish` on an archive that was previously a "snapshotter" orphan the S3 history? Operational, not schema-level — note in release notes / migration instructions for any already-deployed dev archives.

## What this design does NOT cover

- **Other snapshot flavors** (seidb, app-layer, … ). The wrapper is shaped to accept them as sibling sub-structs on `SnapshotGenerationConfig`; actual design for those modes ships when the use case appears.
- **Per-node bucket / prefix overrides on `publish`**. Deferred; `TendermintSnapshotPublishConfig` is empty today precisely so these can land as field additions later.
- **A sidecar-managed on-disk GC policy beyond `keepRecent`.** The sidecar currently relies on seid's `snapshot-keep-recent`; no separate retention policy is introduced here.
- **Metrics / events for snapshot upload success & failure.** Covered separately in `.tide/observability.md` (`SnapshotScheduled`, `SnapshotComplete`, `SnapshotFailed`); this design does not alter those event names.

## Related work

- `api/v1alpha1/common_types.go:109` — `SnapshotGenerationConfig` (type being refactored)
- `api/v1alpha1/full_node_types.go:11`, `api/v1alpha1/archive_types.go:8` — consumers of the type
- `api/v1alpha1/common_types.go:107` — `StateSyncSource struct{}` precedent for the presence-as-toggle pattern
- `internal/planner/full.go:17`, `internal/planner/archive.go:18` — `Validate(node)` hooks; home for the new cross-field checks
- `internal/planner/full.go:44`, `internal/planner/archive.go:35` — override call sites
- `internal/planner/planner.go:312` — `SnapshotGeneration()` helper
- `internal/task/bootstrap.go:25` — `ConfigureStateSyncParams`, an example of the one-shot "enable a background sidecar behavior" task pattern
- `internal/task/config.go:33` — `ConfigValidateParams`, the rendered-config validator (distinct from spec-level `Validate(node)`)
- `internal/task/task.go:180` — sidecar task registry where `TaskTypeSnapshotUpload` is re-registered
- `internal/noderesource/noderesource.go:300-301` — sidecar `SEI_SNAPSHOT_BUCKET` / `SEI_SNAPSHOT_REGION` env (unchanged; sidecar reads upload target from these)
- seictl `sidecar/client/tasks.go` — `TaskTypeSnapshotUpload` / `SnapshotUploadTask{}` (v0.0.30, already present; the controller side is what's missing)
- Commit `358a216` (PR #89) — removed the monitor-task subsystem, including the old `SnapshotUploadMonitorTask` plumbing that this design reinstates as a plan task
- `manifests/samples/seinode/pacific-1-snapshotter.yaml`, `pacific-1-state-syncer.yaml` — sample manifests updated alongside the type change
- `.tide/observability.md` §Events — snapshot-related event names (unchanged by this design)
