# Design: SeiNode — Import Existing Storage

**Status:** Draft / RFC
**Date:** 2026-04-20
**Tracks:** [#105](https://github.com/sei-protocol/sei-k8s-controller/issues/105)
**Related:** [#104](https://github.com/sei-protocol/sei-k8s-controller/issues/104)

## Problem

Today `internal/task/ensure_pvc.go` always calls `Create()` for a SeiNode's data PVC. There is no first-class way to express "use this pre-populated volume." The controller *accidentally* tolerates pre-existing PVCs — `Create()` returns `AlreadyExists`, the task marks Complete, and reconciliation proceeds. This is the bug in #104.

Three use cases want this behavior to be a real feature, not an accident:

1. **AMI/snapshot-based bootstrap.** Weekly AMIs of running pacific-1 archives exist; new k8s archives should bootstrap from the latest snapshot (catching up hours of blocks) instead of resync-from-genesis (months).
2. **One-off migrations.** Moving an existing archive into k8s without re-syncing — currently done by staging a PVC with the expected name and relying on the accidental path.
3. **Orphan adoption.** `deletionPolicy: Retain` + the SeiNode finalizer's `deleteNodeDataPVC` orphan PVs. A re-created SeiNode should be able to explicitly adopt an orphan.

The feature and the bug share a refactor: splitting `ensure-data-pvc` into a create-path and an import-path forces us to write the validation logic that #104's `AlreadyExists` swallow is currently hiding.

## Shapes evaluated

| Axis | **A** — import PVC by name | **B** — import EBS volume ID | **C** — import EBS snapshot ID |
|---|---|---|---|
| User ergonomics | Two-step (pre-create PV+PVC, then reference). Familiar to k8s operators. | One-step from user POV. Controller owns PV. | One-step; covers weekly-AMI use case end-to-end. |
| Controller complexity | Near-zero — conditional skip of `ensure-data-pvc`, plus a validating read. | Moderate — controller synthesizes a static PV with `csi.volumeHandle`, sets `claimRef`. No cloud SDK. | High — AWS SDK, IRSA creds, region plumbing, async `CreateVolume` polling, idempotency, AZ selection, backoff/retry. |
| Cloud-provider awareness | None | Thin — names a volume handle but never calls AWS | Crosses into cloud-provider controller territory |
| Failure modes | PVC missing / wrong size / wrong AZ / already bound elsewhere — all observable via k8s API | Volume in wrong AZ, attached elsewhere, wrong fsType — observable when CSI fails to attach | All of B, plus: snapshot doesn't exist, quota exceeded, AWS throttling, half-created volumes on controller crash, orphan-volume billing leaks |
| Safety | Strong — operator explicitly staged everything | Medium — controller creates the PV but the volume pre-exists and is user-supplied | Weak by default — controller creates real cloud resources that can leak money or conflict with out-of-band automation |
| Interaction with #104 | Forces the fix: if `import` is set, we validate bind state and spec match. If unset, the create path must assert `NotFound`. | Requires #104 fix plus PV-state validation | Same as B plus new failure surface during volume creation |
| Interaction with Retain-orphan | Solves adoption cleanly — reference the orphaned PVC, or recreate a PVC bound to the orphaned PV | Helps only if orphan retains the EBS volume (it does) | Irrelevant — snapshot path is orthogonal |

### Additional options considered

- **D** — `importFromPV` (reference a PV by name). Operator pre-creates the PV, controller creates the PVC with `volumeName:` pointing at it. Less friction than A for orphan adoption — the PVC spec (size, SC, labels) stays under controller ownership; we just bind.
- **E** — `VolumeSnapshot` CR as the snapshot source. If a team wants snapshot-restore, they materialize a `VolumeSnapshot` (or `VolumeSnapshotContent` pre-provisioned from an EBS snap ID), then reference it via the standard k8s `dataSource`. The external-snapshotter + CSI driver do the heavy lifting. **No AWS SDK in our controller.**
- **F** — Bootstrap Job pattern (separate `SeiNodeBootstrap` CR). Over-engineered for the current use cases; adds a resource type to reason about.

E is the k8s-native equivalent of C. Same user outcome, zero cloud SDK in our controller. The operator or an external platform pipeline produces the `VolumeSnapshotContent`; we consume it.

## Decision

**Adopt A and D together as a single design pass. Keep E on the roadmap. Do not build C.**

### Rationale

1. **A + D cover all three stated use cases today.** The manual migration was A minus the validation. Orphan adoption is D (or A, depending on how the operator stages the adoption). AMI bootstrap can be staged into A by a human or an external tool that produces the PV+PVC — later upgraded to E when snapshot-restore is worth the investment.

2. **A is the smallest defensible fix to #104.** The bug exists because we conflate "PVC present" with "PVC correct." Splitting the task into a create-path and an import-path forces the validation we're missing: the import path must assert `spec.resources.requests.storage >=` node needs, `storageClassName` matches or is intentionally empty (static PV), `DeletionTimestamp` is nil, and the PVC is `Bound` to a PV whose `capacity >=` required size. Same validation catches the orphan-adoption race where a Retain PV is still `Released`.

3. **A + D avoid the cloud-provider precedent.** Once the controller has AWS creds for `CreateVolume`, gravity pulls toward more cloud calls inside the controller. The team is explicitly shallow on k8s; the operator already runs alongside tooling (Karpenter, EBS CSI driver, Terraform/CDK) better-placed for cloud-resource lifecycle.

4. **D is a small delta from A** — worth including in the same design pass. Supporting both `import.pvcName` (fully external) and `import.pvName` (controller owns the PVC, binds to a named PV) covers "I staged everything" and "I just have an orphan PV" without much additional code.

5. **E becomes the snapshot story when it's needed.** Reuses the existing k8s snapshot ecosystem rather than building a parallel one. Until a second team asks for snapshot-restore, A + external tooling is fine.

## Spec sketch

```yaml
spec:
  dataVolume:
    import:
      pvcName: data-archive-0-0      # Shape A — reference a pre-existing PVC
      # OR
      pvName: pv-archive-restored    # Shape D — reference a pre-existing PV
      # Optional:
      reclaimPolicy: Preserve        # override the PV's default reclaim behavior on SeiNode deletion
```

Planner behavior: if `spec.dataVolume.import` is set, `ensure-data-pvc` is replaced with a new task — `validate-imported-pvc` (Shape A) or `adopt-pv-and-create-pvc` (Shape D). Successor tasks (`apply-statefulset`, `apply-service`, etc.) are unchanged. Snapshot-restore and bootstrap-Job tasks are skipped when importing — the data is by definition already present.

## Open questions (for the LLD)

1. **Deletion semantics for imported volumes.** When a SeiNode with `import.pvcName` is deleted, does `deleteNodeDataPVC` still delete the PVC?
   - **(a)** Always preserve imported PVCs (safe, but deletes don't fully clean up)
   - **(b)** Respect the PV's `persistentVolumeReclaimPolicy` (Retain vs Delete) — most k8s-idiomatic
   - **(c)** Add explicit `spec.dataVolume.import.reclaimPolicy` that overrides
   
   Leaning (b) with (c) as override.

2. **What does "validate" actually check, and do we ever mutate?** Size is obvious (PV capacity ≥ requested). But:
   - Do we require PVC labels to match `ResourceLabels`?
   - Do we require a specific `storageClassName` or allow any?
   - Do we refuse if the PVC is still `Pending` / `Released`, or wait with a retry budget?
   - Do we attempt to fix (patch labels, patch claimRef) or only read and fail?
   
   Lean: **validate, never mutate; fail loudly; no data-content validation** (the operator asserts it's a valid Sei data dir; sidecar's `configure-genesis` and seid startup catch gross errors anyway).

3. **How does import interact with the init plan's bootstrap progression?** If a node imports a PVC, the bootstrap Job (snapshot-restore into the PVC) is by definition wrong. Does `import` imply `skipBootstrap`, or do we allow both and detect conflict?
   
   Lean: **import implies no bootstrap.** Planner routes to a simplified init progression: `validate-imported-pvc` → `apply-statefulset` → `apply-service` → `configure-genesis` → `config-apply` → `config-validate` → `mark-ready`, skipping all snapshot-restore tasks. Needs a clear state-diagram in the LLD.

## What this design does NOT cover

- **Shape E / VolumeSnapshot integration.** Deferred; revisit when a team asks for snapshot-restore directly through the controller.
- **Automated AMI → PV+PVC pipeline.** External tooling concern (platform repo or a separate operator). This design just accepts a PVC/PV someone else produced.
- **Cross-cluster imports.** Out of scope.
- **Migration path for existing SeiNodes.** Import is opt-in via `spec.dataVolume.import`; SeiNodes created without it go through the normal `ensure-data-pvc` path unchanged.

## Related work

- [#104](https://github.com/sei-protocol/sei-k8s-controller/issues/104) — `ensure-data-pvc` marks Complete on `AlreadyExists`. Import-path design forces the underlying fix.
- Retain-reclaim-policy + finalizer orphan behavior (separate issue to file; the import-feature is safe to ship before it lands, but together they close the loop on SeiNode deletion + re-creation flows).
- `internal/task/ensure_pvc.go` — site of the create-path/import-path split
- `internal/noderesource/noderesource.go:176` (`GenerateDataPVC`) — unchanged; still generates the create-path PVC
- `internal/planner/bootstrap.go` — where the conditional routing to `validate-imported-pvc` / `adopt-pv-and-create-pvc` lives
- `api/v1alpha1/seinode_types.go` — where `spec.dataVolume.import` gets added
