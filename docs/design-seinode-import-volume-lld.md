# Design: SeiNode — Import Existing Storage (LLD)

**Status:** Draft / LLD
**Date:** 2026-04-21
**Tracks:** [#105](https://github.com/sei-protocol/sei-k8s-controller/issues/105)
**Related:** [#104](https://github.com/sei-protocol/sei-k8s-controller/issues/104), [`docs/design-seinode-import-volume.md`](design-seinode-import-volume.md)

This is the companion LLD to the direction doc. The direction doc fixes *what* and *why*; this doc fixes *how*. Scope is strictly bounded to Shape A (import PVC by name) plus the #104 create-path fix. No new shapes, no new fields, no new use cases.

## 1. CRD schema changes

A single new optional sub-struct is added to `SeiNodeSpec` in `api/v1alpha1/seinode_types.go`. Field naming follows the spec sketch in the direction doc (`spec.dataVolume.import.pvcName`) and k8s API conventions (lowerCamelCase JSON tags, PascalCase Go fields, no acronyms beyond `PVC`).

```go
// SeiNodeSpec additions (api/v1alpha1/seinode_types.go)

// DataVolume configures the data PersistentVolumeClaim for this node.
// When omitted, the controller creates a PVC using the node's mode-default
// storage class and size (see noderesource.DefaultStorageForMode).
// +optional
DataVolume *DataVolumeSpec `json:"dataVolume,omitempty"`

// DataVolumeSpec configures how the data PVC is sourced.
type DataVolumeSpec struct {
    // Import references a pre-existing PersistentVolumeClaim in the same
    // namespace as the SeiNode, instead of creating a new one. The
    // controller validates the referenced PVC but never mutates it.
    //
    // When Import is set, the controller never deletes the referenced PVC
    // on SeiNode deletion — storage lifecycle is the operator's responsibility.
    // +optional
    Import *DataVolumeImport `json:"import,omitempty"`
}

// DataVolumeImport names a pre-existing PVC to adopt as this node's data volume.
type DataVolumeImport struct {
    // PVCName is the name of a PersistentVolumeClaim in the SeiNode's
    // namespace. The PVC must be Bound, ReadWriteOnce, and sized at or above
    // the node mode's default storage size. Immutable after creation.
    //
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=253
    // +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="pvcName is immutable"
    PVCName string `json:"pvcName"`
}
```

### Why these markers

- Length + pattern mirror the k8s DNS1123Label constraint on PVC names — catches typos at admission time.
- `XValidation: self == oldSelf` makes the reference immutable. Swapping an imported PVC out from under a running SeiNode has no defined semantics; force delete-and-recreate if the operator wants to re-point.
- `DataVolume` as pointer keeps `omitempty` clean so existing SeiNodes serialize identically to today.
- No default `storageClassName`/`size` sub-fields — Shape A is "name a PVC, nothing else" per the direction doc.

### Regenerated artifacts

- `zz_generated.deepcopy.go` — new `DeepCopy`/`DeepCopyInto` methods for `DataVolumeSpec` and `DataVolumeImport`; updated `SeiNodeSpec.DeepCopyInto` to copy the pointer. Produced by `make generate`.
- `manifests/sei.io_seinodes.yaml` and `config/crd/sei.io_seinodes.yaml` — new `spec.dataVolume.import.pvcName` subtree with validation constraints. Produced by `make manifests`.

### Backward compatibility

Existing SeiNodes serialize `SeiNodeSpec` without the `dataVolume` key. The new CRD schema makes `dataVolume` optional, so old objects remain valid. The controller reads `node.Spec.DataVolume` as nil and takes the create path unchanged.

### Spec-unset vs. spec-empty

Per the direction doc's idiomatic-k8s guidance: `spec.dataVolume == nil`, `spec.dataVolume.import == nil`, and `spec.dataVolume.import.pvcName == ""` all mean "no import." The task's branch check is a single helper:

```go
func importPVCName(node *seiv1alpha1.SeiNode) string {
    if node.Spec.DataVolume == nil || node.Spec.DataVolume.Import == nil {
        return ""
    }
    return node.Spec.DataVolume.Import.PVCName
}
```

## 2. Task changes to `internal/task/ensure_pvc.go`

The task splits into two internal paths under one public API. The struct, params, and deserializer stay the same; only `Execute()` and `Status()` change.

### New structure

The existing `ensureDataPVCExecution` struct adds two ephemeral fields (`lastReason`, `lastMessage`) for condition propagation within one reconcile. The executor re-deserializes the task on every reconcile (executor.go:150), so per-instance state does not survive. No other state is added; the validation path is stateless across reconciles.

### Execute() — branching structure

```go
func (e *ensureDataPVCExecution) Execute(ctx context.Context) error {
    node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
    if err != nil {
        return Terminal(err)
    }

    if name := importPVCName(node); name != "" {
        return e.executeImport(ctx, node, name)
    }
    return e.executeCreate(ctx, node)
}
```

### Create path (fixes #104)

```go
func (e *ensureDataPVCExecution) executeCreate(ctx context.Context, node *seiv1alpha1.SeiNode) error {
    desired := noderesource.GenerateDataPVC(node, e.cfg.Platform)
    if err := ctrl.SetControllerReference(node, desired, e.cfg.Scheme); err != nil {
        return Terminal(fmt.Errorf("setting owner reference: %w", err))
    }

    existing := &corev1.PersistentVolumeClaim{}
    key := types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}
    switch err := e.cfg.KubeClient.Get(ctx, key, existing); {
    case apierrors.IsNotFound(err):
        // proceed to Create
    case err != nil:
        return fmt.Errorf("checking for existing data PVC: %w", err)
    default:
        // PVC exists. Accept if we own it (crash-recovery); else fail.
        if metav1.IsControlledBy(existing, node) {
            e.complete()
            return nil
        }
        return Terminal(fmt.Errorf(
            "data PVC %q already exists and is not owned by SeiNode %q; "+
                "set spec.dataVolume.import.pvcName to adopt, or delete the PVC",
            existing.Name, node.Name))
    }

    if err := e.cfg.KubeClient.Create(ctx, desired); err != nil {
        if apierrors.IsAlreadyExists(err) {
            // Lost the race with another actor between Get and Create;
            // requeue so the next reconcile's Get resolves ownership.
            return fmt.Errorf("data PVC created concurrently: %w", err)
        }
        return fmt.Errorf("creating data PVC: %w", err)
    }
    e.complete()
    return nil
}
```

Key changes from today's behavior:

| Before (bug #104) | After |
|---|---|
| `Create()` → swallow `AlreadyExists` as success | `Get()` first; `IsNotFound` is the happy prelude to `Create` |
| Any pre-existing PVC → task Complete | Pre-existing PVC owned by this SeiNode → Complete; otherwise `Terminal` failure |
| No way to distinguish "our crash-recovery" from "someone else's PVC" | `metav1.IsControlledBy` check separates the two |

### Import path — state machine

`Status()` remains:

```go
func (e *ensureDataPVCExecution) Status(_ context.Context) ExecutionStatus {
    return e.DefaultStatus()
}
```

`executeImport` leaves `e.status == ExecutionRunning` (the `taskBase` default) on transient validation failures and returns `nil`. The executor sees Running and requeues after `TaskPollInterval` (5 s, executor.go:178) — we use this interval as-is. Each reconcile does one Get against the controller-runtime cache, which is a ~free operation; no custom backoff, no state, no arithmetic.

```go
func (e *ensureDataPVCExecution) executeImport(ctx context.Context, node *seiv1alpha1.SeiNode, name string) error {
    reason, msg, state := e.validateImport(ctx, node, name)
    recordTransient(node, reason, msg) // writes "reason: msg" into PlannedTask.Error
    switch state {
    case importValid:
        e.complete()
        return nil
    case importTerminal:
        return Terminal(fmt.Errorf("%s: %s", reason, msg))
    default: // transient
        return nil
    }
}
```

`importValid`/`importTransient`/`importTerminal` are internal enums; `validateImport` returns one per requirement.

### Which reasons are transient vs. terminal

| # | Requirement | Validation failure | State |
|---|---|---|---|
| 1 | PVC exists | `IsNotFound` on Get | transient — operator may be about to apply the PVC |
| 2 | `deletionTimestamp == nil` | PVC being deleted | transient — may resolve if finalizers complete and PVC re-appears under external management (rare, but cheaper to retry than to fail) |
| 3 | `phase == Bound` | `Pending` | transient — binder may complete |
| 3 | `phase == Bound` | `Lost` | terminal — no recovery path for Lost |
| 3 | `phase == Bound` | `Released` | transient — operator may rebind to a new claim |
| 4 | Contains `ReadWriteOnce` | wrong access mode | terminal — PVC spec is immutable for accessModes |
| 5 | `status.capacity.storage >= default` | too small | terminal — a smaller PVC cannot grow to required size without operator action; operator must expand or re-provision |
| 6 | PV `spec.capacity == PVC status.capacity` | mismatch | terminal — indicates misconfigured static PV |
| 7 | PV exists and not `Failed` | PV missing | transient — lookup race during bind |
| 7 | PV exists and not `Failed` | PV `Failed` | terminal — CSI/provisioner declared the volume unusable |

**Rationale for the transient/terminal split:** the direction doc says "retry indefinitely with exponential backoff." A strict reading would make every failure mode transient. The LLD refines: failures that cannot recover without the operator changing the PVC spec (immutable accessModes, a too-small capacity) become terminal and surface as `plan.Phase = Failed` + `SeiNode Phase = Failed` via the existing `FailedPhase` from `buildBasePlan` (planner.go:447). This matches the direction doc's "seid will fail to start… the operator gets a clear signal from the Failed plan" — applied pre-flight where we know the failure is unrecoverable. A reviewer who prefers "always transient" can flip these at the cost of stuck-Initializing on operator typos.

### Reason strings

CamelCase, stable, and part of the public alerting contract (see §4). Exact strings:

```go
const (
    ReasonImportValidated         = "PVCValidated"
    ReasonImportPVCNotFound       = "PVCNotFound"
    ReasonImportPVCTerminating    = "PVCTerminating"
    ReasonImportPVCNotBound       = "PVCNotBound"       // Pending/Released
    ReasonImportPVCLost           = "PVCLost"           // terminal
    ReasonImportAccessModeInvalid = "AccessModeInvalid" // terminal
    ReasonImportCapacityTooSmall  = "CapacityTooSmall"  // terminal
    ReasonImportPVMissing         = "UnderlyingPVMissing"
    ReasonImportPVCapacityMismatch = "UnderlyingPVCapacityMismatch" // terminal
    ReasonImportPVFailed          = "UnderlyingPVFailed"            // terminal
)
```

Message format is human-readable and includes the PVC name plus the concrete defect:

```
PVC "data-archive-0-0" not found in namespace "default"
PVC "data-archive-0-0" phase is Pending, waiting for bind
PVC "data-archive-0-0" capacity 500Gi is less than required 2Ti
underlying PV "pv-abc" for PVC "data-archive-0-0" is in phase Failed
```

## 3. Validation path in detail

`validateImport(ctx, node, name) (reason, message, state)` runs the seven checks in order, returning on the first defect. Sketch:

```go
// 1. Get PVC in node.Namespace → IsNotFound → (PVCNotFound, transient)
// 2. deletionTimestamp != nil → (PVCTerminating, transient)
// 3. switch pvc.Status.Phase {
//      case Bound: continue
//      case Lost:  (PVCLost, terminal)
//      default:    (PVCNotBound, transient)   // Pending, Released, ""
//    }
// 4. !containsAccessMode(..., ReadWriteOnce) → (AccessModeInvalid, terminal)
// 5. required := resource.MustParse(DefaultStorageForMode(mode, platform).size)
//    actual := pvc.Status.Capacity[ResourceStorage]
//    !ok       → (CapacityTooSmall, transient)   // capacity not yet reported
//    actual<req → (CapacityTooSmall, terminal)
// 6/7. Get PV by pvc.Spec.VolumeName:
//      IsNotFound          → (UnderlyingPVMissing, transient)
//      pv.Status.Phase==Failed → (UnderlyingPVFailed, terminal)
//      pv.Spec.Capacity != actual → (UnderlyingPVCapacityMismatch, terminal)
// default: (PVCValidated, "", valid)
```

Helper `containsAccessMode` is a one-line slice scan. Messages are the templates in §4's table.

### Client operations summary

| Call | Verb | Resource | Purpose |
|---|---|---|---|
| Get PVC | `get` | `persistentvolumeclaims` | requirements 1–5 |
| Get PV | `get` | `persistentvolumes` | requirements 6–7 |

Both are reads. The task never mutates the imported PVC or PV — explicit per direction doc §77. RBAC already grants `get` on PVCs (controller.go:55). A new RBAC marker is required on the node controller to read PVs:

```go
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
```

This is the only RBAC addition. `make manifests` regenerates `manifests/role.yaml`.

### Requeue interval

We use the executor's existing `TaskPollInterval` (5 s, `executor.go:178`) unchanged for Running tasks. No custom backoff, no per-task state, no attempt counter. Each reconcile cycle on a still-transient import performs one cache-served Get against the named PVC (and possibly its PV) and returns Running.

**Why this is fine**: controller-runtime's default client serves Get from an informer cache; these are essentially free local-memory reads, not round trips to the API server. At one Get per 5 s per stuck import, even a fleet of stuck imports produces a trivial load footprint. There is no cost saving to be had from stretching the interval, so we don't.

**Stuck-import visibility**: the plan task's `error` field records the latest failure reason+message (§6), and the `ImportPVCReady` Condition (§4) carries the current state on the SeiNode. A human reading `kubectl describe seinode` sees the current reason directly; there is no hidden schedule state to reason about.

**Transient vs. terminal**: unrecoverable failures (wrong access modes, too-small capacity — see the table in §2) bypass this entirely and mark the plan Failed, giving the operator a clear signal. The 5 s-polling path is only for genuinely transient conditions (PVC still binding, PV provisioning race, operator about to apply the PVC).

## 4. Condition schema on SeiNode status

### Type constant

```go
// api/v1alpha1/seinode_types.go
const (
    ConditionNodeUpdateInProgress = "NodeUpdateInProgress"
    ConditionImportPVCReady       = "ImportPVCReady" // new
)
```

**Naming decision:** `ImportPVCReady` follows the k8s convention `<Subject>Ready` (cf. `PodReady`, `ContainersReady`, `NodeReady`). Alternatives considered and rejected:

| Name | Rejection reason |
|---|---|
| `DataVolumeImported` | Past tense implies one-shot event; condition should be a gate ("is the imported PVC ready right now?"). |
| `ImportedPVCBound` | Too narrow — Bound is only one of seven requirements. |
| `ImportPVCValid` | "Valid" is about spec correctness; "Ready" encompasses both validity and runtime state (Bound, PV present). Matches upstream usage. |

### Status / Reason / Message matrix

| Status | Reason | Message template | When set |
|---|---|---|---|
| `True` | `PVCValidated` | `PVC "<name>" passes all import requirements` | task transitions to Complete |
| `False` | `PVCNotFound` | `PVC "<name>" not found in namespace "<ns>"` | Get returns NotFound |
| `False` | `PVCTerminating` | `PVC "<name>" is being deleted (deletionTimestamp=<ts>)` | deletionTimestamp set |
| `False` | `PVCNotBound` | `PVC "<name>" phase is "<phase>", waiting for Bound` | phase Pending/Released/"" |
| `False` | `PVCLost` | `PVC "<name>" phase is Lost; underlying PV is gone` | phase Lost |
| `False` | `AccessModeInvalid` | `PVC "<name>" accessModes <modes> does not include ReadWriteOnce` | accessModes wrong |
| `False` | `CapacityTooSmall` | `PVC "<name>" capacity <actual> is less than required <required>` | status.capacity too small |
| `False` | `UnderlyingPVMissing` | `underlying PV "<pv>" for PVC "<name>" not found` | PV Get NotFound |
| `False` | `UnderlyingPVCapacityMismatch` | `underlying PV "<pv>" capacity <pv-cap> does not match PVC "<name>" capacity <pvc-cap>` | PV capacity mismatch |
| `False` | `UnderlyingPVFailed` | `underlying PV "<pv>" for PVC "<name>" is in phase Failed` | PV phase Failed |
| `Unknown` | — | — | never set; we always resolve to True/False after a reconcile |

The condition is **not** set on SeiNodes without `spec.dataVolume.import.pvcName`. Its presence in `.status.conditions` is itself a signal that the node is an import node.

### Public contract for alerting

The `Reason` strings are a **public contract**. Any Prometheus alert, audit tool, or operator script that keys on these strings (e.g., `kube_seinode_status_condition{type="ImportPVCReady",reason="PVCNotFound"}`) must keep working across controller versions. Adding a new Reason is a minor-version addition; renaming or removing an existing Reason is a breaking change and requires a deprecation window. This is called out in a Go doc comment on the `ReasonImport*` constant block.

### ObservedGeneration

Every `meta.SetStatusCondition` call passes `ObservedGeneration: node.Generation`, matching the existing `NodeUpdateInProgress` pattern at planner.go:195.

## 5. Finalizer interaction

The single-line change lives in `internal/controller/node/controller.go:218`'s `deleteNodeDataPVC`:

```go
func (r *SeiNodeReconciler) deleteNodeDataPVC(ctx context.Context, node *seiv1alpha1.SeiNode) error {
    // Imported PVCs are managed externally — never delete them.
    if node.Spec.DataVolume != nil && node.Spec.DataVolume.Import != nil &&
        node.Spec.DataVolume.Import.PVCName != "" {
        log.FromContext(ctx).Info("skipping data PVC delete for imported volume",
            "pvc", node.Spec.DataVolume.Import.PVCName)
        return nil
    }

    pvc := &corev1.PersistentVolumeClaim{}
    err := r.Get(ctx, types.NamespacedName{Name: noderesource.DataPVCName(node), Namespace: node.Namespace}, pvc)
    if apierrors.IsNotFound(err) {
        return nil
    }
    if err != nil {
        return err
    }
    return r.Delete(ctx, pvc)
}
```

Notes:

- The check uses the full nested-nil guard rather than the `importPVCName` helper because the finalizer path is in a different package (`internal/controller/node`) and the helper lives in `internal/task`. Duplicating the three-line guard is cheaper than introducing a cross-package helper for a single caller.
- `cleanupNodeMetrics` (controller.go:212) still runs — metrics cleanup is orthogonal to data lifecycle.

### PV reclaim policy interaction

Since the import path never touches the PVC on deletion, it never touches the PV either. The PV's `persistentVolumeReclaimPolicy` — whatever the operator configured — is unaffected. If the operator set `Retain`, the PV persists when the PVC is eventually deleted out-of-band (their responsibility). If they set `Delete`, the PV is cleaned up when they delete the PVC. The controller has no opinion. This is the symmetric behavior the direction doc calls for: "touch nothing the controller didn't create."

The separate orphan-PV + finalizer bug (direction doc §140) is unrelated to the import path and remains out of scope here.

## 6. Condition management

Per the controller's established pattern (planner.go:165-179 for `NodeUpdateInProgress`), **the planner owns all condition mutations**. The executor does not touch `SeiNode.Status.Conditions`.

### Where the condition is set

`planner.ReconcileImportPVCCondition(node)` is called from the reconciler *after* plan execution and *before* the status flush (a single new line in `Reconcile`, near controller.go:123). It is idempotent and reads the plan's `ensure-data-pvc` task:

- No import configured → `meta.RemoveStatusCondition(..., ConditionImportPVCReady)`.
- Task `Complete` → `True` / `PVCValidated` / success message.
- Task `Pending` or `Failed` → `False` / Reason parsed from `PlannedTask.Error` / message parsed from the same.
- No task in plan (plan already terminal) → leave existing condition as-is (once True on successful init, it stays True).

### Propagating task state to the condition

The task's `lastReason`/`lastMessage` fields are ephemeral (re-deserialized per reconcile). We persist them by **reusing `PlannedTask.Error`**: transient validations write `t.Error = reason + ": " + message` directly on the in-memory plan, and the planner parses on the prefix. Terminal failures already land in `plan.FailedTaskDetail.Error` via `failTask` (executor.go:209) with the same shape.

The task mutates `plan.Tasks[i].Error` from inside `Execute` — a deliberate narrow mutation, consistent with the single-patch model (the reconciler flushes the plan + conditions together) and with `Error` being a task-owned field.

### Clearing

- `ReasonImportValidated` is set on Complete (§6 table).
- The condition is removed on: deletion (handled by finalizer path and is moot because the object is going away), or `spec.dataVolume.import` being unset (removed via `meta.RemoveStatusCondition`).

The condition is **never cleared** while the task is Running and the PVC still fails validation; it just gets its Reason/Message updated. This matches the direction doc's "persistent Condition that is visible in `kubectl describe seinode`."

## 7. Tests

### Unit tests (`internal/task/ensure_pvc_test.go` — new file)

Table-driven, using the same fake-client pattern as `observe_image_test.go` (reference lines 33-52 for the scheme+client boilerplate).

**Create-path cases** (all with `spec.dataVolume.import` unset):

- `PVCMissing_CreatesAndCompletes` — no pre-existing PVC → Execute succeeds, Complete, PVC exists after.
- `PVCExistsOwnedByUs_Completes` — pre-existing PVC with controller ref to this SeiNode → Complete (crash-recovery).
- `PVCExistsNotOwned_TerminalError` — pre-existing PVC without owner ref (the #104 bug) → `*TerminalError`.
- `CreateRaceAlreadyExists_Requeues` — Get NotFound but Create returns AlreadyExists → non-terminal error, Running.

**Import-path happy path:** `AllRequirementsMet_Completes` — PVC Bound/RWO/sized, PV present and matching.

**Import-path per-requirement failures** (one case per row of §3's state-machine table), asserting task `Running`/`Failed` + `t.Error` prefix matching the expected Reason:

- req 1: `PVCNotFound_Transient`; `PVCNotFound_ThenAppears_Completes` (apply PVC between reconciles).
- req 2: `PVCTerminating_Transient`.
- req 3: `PVCPending_Transient`, `PVCReleased_Transient`, `PVCLost_Terminal`.
- req 4: `WrongAccessMode_Terminal`.
- req 5: `CapacityTooSmall_Terminal`, `CapacityUnset_Transient`.
- req 6: `PVCapacityMismatch_Terminal`.
- req 7: `PVMissing_Transient`, `PVFailed_Terminal`.

**Poll cadence** (regression guard): `TransientValidationRepeats` — stuck-in-transient state produces one Get per reconcile, matching the executor's `TaskPollInterval`. Asserted via counting client decorator over N reconciles.

### Integration / e2e-style (extends existing harnesses in `internal/controller/node/`)

- `Controller_Import_Happy_ReachesRunning` — import.pvcName references a Bound PVC → Phase=Running, `ImportPVCReady=True/PVCValidated`.
- `Controller_Import_LatePVC_Converges` — SeiNode applied first (no PVC) → Initializing+`PVCNotFound`. Apply PVC → Running+`True`. Validates the ordering-race promise in the direction doc.
- `Controller_Import_TerminalFailure_MarksFailed` — wrong accessMode → Plan Failed, SeiNode Phase=Failed, `False/AccessModeInvalid`.
- `Controller_Import_Deletion_PreservesPVC` — delete SeiNode with import → finalizer runs, PVC still exists.
- `Controller_NoImport_Deletion_DeletesPVC` — regression guard for existing behavior.

## 8. Observability

### Metrics

Add to `internal/controller/observability/`:

- `importPVCValidationTotal` (counter, attrs: controller, namespace, result∈{valid,transient,terminal}) — emitted every `validateImport` call.
- `importPVCTerminalTotal` (counter, attrs: controller, namespace, reason) — emitted on Terminal, so `CapacityTooSmall` vs. `UnderlyingPVFailed` alert independently.
- `importPVCTimeToValid` (histogram, attrs: controller, namespace) — observed at Complete from `SubmittedAt` → now. SLO: time-from-apply-to-imported-PVC-validated.

The existing `planDuration` histogram (planner.go:214) continues to record end-to-end init duration — no change needed beyond `classifyPlan` which already recognizes `ensure-data-pvc` as the "init" plan marker.

### Events

Emitted from the reconciler after status flush (tasks have no Recorder access via `ExecutionConfig`). `PVCImportValidated` (Normal) on transition to True; `PVCImportFailed` (Warning) when the False-condition Reason changes. De-duplication: compare against the previous condition Reason captured from the `DeepCopy` taken for the patch base (controller.go:91).

### Status updates

The latest validation error appears both on the `ImportPVCReady` condition (stable alerting contract) and on the `ensure-data-pvc` task's `.error` field (operator-debugging view). Both carry identical content; the task's field is `"<Reason>: <Message>"`.

## 9. Migration / rollout plan

**No migration.** Existing SeiNodes have `spec.dataVolume == nil`; the controller reads it as nil and takes the create path unchanged. The #104 create-path fix changes behavior in one edge case: an existing SeiNode whose data PVC was accidentally created out-of-band will now fail the plan on next reconcile with `data PVC "foo" already exists and is not owned by SeiNode "foo"`. This is intentional — the operator must either set `import.pvcName` to adopt it, or delete the conflicting PVC. Document this in the PR description and in the changelog.

**Controller upgrade is idempotent.** Any running SeiNode whose plan has already completed `ensure-data-pvc` is unaffected. A SeiNode whose plan is still Active and currently on `ensure-data-pvc` will re-enter the new task code on the next reconcile; if it's a plain create path and the PVC exists and is owned, the new path sees `IsControlledBy` and marks Complete.

**Operator rollout:**

```bash
make manifests generate
make test
make lint
make docker-build IMG=<registry>/sei-k8s-controller:<sha>
make docker-push IMG=<registry>/sei-k8s-controller:<sha>
# update Helm/flux values; controller restart is disruption-free
```

No CRD data migration is required since the new fields are additive and optional. Applying the new CRD on a cluster with old SeiNode objects is safe.

## 10. What this LLD does NOT cover

- **Shape D** (`importFromPV`) — deferred per direction doc.
- **Shape E** (VolumeSnapshot dataSource) — deferred per direction doc.
- **External AMI/snapshot → PV+PVC pipeline** — operator/platform concern.
- **Orphan-PV + finalizer interaction bug** — separate issue; import path does not touch PVs, so it is not made worse by this change.
- **Expanding an imported PVC after import** — not addressed; if capacity changes, operator edits the PVC directly. Controller does not reconcile size.
- **Re-pointing `import.pvcName` after creation** — explicitly blocked by the immutability marker (§1).

## Related work

- [#104](https://github.com/sei-protocol/sei-k8s-controller/issues/104) — the `AlreadyExists` swallow; fixed in §2's create path.
- [#105](https://github.com/sei-protocol/sei-k8s-controller/issues/105) — the import feature tracked here.
- `internal/task/ensure_pvc.go` — modified per §2.
- `internal/noderesource/noderesource.go:176` — unchanged.
- `api/v1alpha1/seinode_types.go` — new fields per §1.
- `internal/controller/node/controller.go:218` — finalizer skip per §5.
- `internal/planner/bootstrap.go` — unchanged.
- `internal/planner/executor.go:150-200` — retry model reused without modification.
- `internal/planner/planner.go:165-197` — condition pattern mirrored for `ImportPVCReady`.
