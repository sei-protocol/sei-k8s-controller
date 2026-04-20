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

- **D** — `importFromPV` (reference a PV by name). Operator pre-creates the PV, controller creates the PVC with `volumeName:` pointing at it. Covers orphan adoption with the controller owning the PVC object. Evaluated but **not adopted** in this pass — the orphan-adoption use case is already reachable via Shape A (operator creates a PVC with `volumeName:` pointing at the orphan PV, then references that PVC from the SeiNode). Adding D is a 2nd CRD field and a 2nd task path for a use case that Shape A already covers with one extra operator step. Revisit if orphan adoption becomes frequent enough that the extra step is operational friction.
- **E** — `VolumeSnapshot` CR as the snapshot source. If a team wants snapshot-restore, they materialize a `VolumeSnapshot` (or `VolumeSnapshotContent` pre-provisioned from an EBS snap ID), then reference it via the standard k8s `dataSource`. The external-snapshotter + CSI driver do the heavy lifting. **No AWS SDK in our controller.**
- **F** — Bootstrap Job pattern (separate `SeiNodeBootstrap` CR). Over-engineered for the current use cases; adds a resource type to reason about.

E is the k8s-native equivalent of C. Same user outcome, zero cloud SDK in our controller. The operator or an external platform pipeline produces the `VolumeSnapshotContent`; we consume it.

## Decision

**Adopt Shape A. Keep E on the roadmap. Do not build C. Defer D.**

### Rationale

1. **A covers all three stated use cases today.** The manual migration was A minus the validation. Orphan adoption is A (operator creates a PVC bound to the orphaned PV, then references it). AMI bootstrap can be staged into A by a human or an external tool that produces the PV+PVC — later upgraded to E when snapshot-restore is worth the investment.

2. **A is the smallest defensible fix to #104.** The bug exists because we conflate "PVC present" with "PVC correct." Splitting the task into a create-path and an import-path forces the validation we're missing: the import path must assert the PVC is present, bound, and matches node requirements. Same validation catches the orphan-adoption race where a Retain PV is still `Released`.

3. **A avoids the cloud-provider precedent.** Once the controller has AWS creds for `CreateVolume`, gravity pulls toward more cloud calls inside the controller. The team is explicitly shallow on k8s; the operator already runs alongside tooling (Karpenter, EBS CSI driver, Terraform/CDK) better-placed for cloud-resource lifecycle.

4. **One shape, one field, one task path.** Keeping the surface area minimal means less to document, test, and maintain. D can be added later without invalidating A.

5. **E becomes the snapshot story when it's needed.** Reuses the existing k8s snapshot ecosystem rather than building a parallel one. Until a second team asks for snapshot-restore, A + external tooling is fine.

## Spec sketch

```yaml
spec:
  dataVolume:
    import:
      pvcName: data-archive-0-0      # name of a pre-existing PVC in the SeiNode's namespace
```

Planner behavior: **the init plan is unchanged.** The only difference is inside `ensure-data-pvc`: if `spec.dataVolume.import.pvcName` is set, the task verifies the named PVC instead of creating a fresh one. Every successor task (`apply-statefulset`, `apply-service`, `configure-genesis`, `config-apply`, `discover-peers`, `configure-state-sync`, `config-validate`, `mark-ready`) runs exactly as it does today.

This is a deliberate "no extra fluff" choice: import is a PVC-source substitution, not a bootstrap off-ramp. The operator is trusted to provide a PVC whose contents are compatible with the rest of the init progression. If the imported data is from an incompatible seid version, the wrong chain, or in an unexpected on-disk format, seid will fail to start on the pod and the operator gets a clear signal from the Failed plan — same failure channel as any other init problem.

The controller does **not**:
- Detect that the PVC is pre-populated and skip any steps
- Refuse to run `configure-genesis` or `config-apply` against imported data
- Add an `import` state to the init plan
- Take any responsibility for the data's contents

This keeps the controller small and the contract with the operator honest: "give me a PVC, I'll use it; making it the right PVC is your job."

## Requirements for an imported PVC

For the import branch of `ensure-data-pvc` to succeed, the PVC must satisfy **all** of the following. The controller never mutates the PVC — it reads and either accepts or fails the task.

| # | Requirement | Rationale |
|---|---|---|
| 1 | PVC exists in the same namespace as the SeiNode | Namespace-scoped resource lookup |
| 2 | `metadata.deletionTimestamp` is nil | A PVC being deleted cannot be relied on to persist |
| 3 | `status.phase == Bound` | Pending/Lost PVCs have no data we can read; Released PVCs aren't attachable |
| 4 | `spec.accessModes` contains `ReadWriteOnce` | Matches what pods attached to a SeiNode require |
| 5 | `status.capacity.storage >=` whatever default the node's role demands (see `noderesource.DefaultStorageForMode`) | Prevent attaching too-small volumes |
| 6 | The underlying PV's `spec.capacity.storage` matches `status.capacity.storage` | Sanity — catch misconfigured PVs |
| 7 | The underlying PV exists and is not in a terminal state (`Failed`) | Same |

The controller does **not** check:
- The filesystem type (CSI mount will fail loudly if wrong)
- The data contents (seid startup / sidecar `configure-genesis` catches data-content problems)
- Labels on the PVC (those are operator concerns; we shouldn't gate on them)
- The `storageClassName` (operators may intentionally use empty SC for static PVs; we trust them)

If any check fails, the task enters a retry loop with the current error in the task's `error` field. A PVC that becomes valid later (e.g., finishes binding) resolves the task on the next reconcile.

## Deletion semantics (decided)

**When a SeiNode with `import.pvcName` is deleted, the controller does NOT delete the imported PVC.** The `deleteNodeDataPVC` finalizer is a no-op for imported volumes.

Rationale: the operator opted into `import.pvcName` precisely because they are managing the storage lifecycle externally (via Terraform, by hand, via another operator). Having the SeiNode controller silently delete the PVC on `kubectl delete seinode` would surprise that external lifecycle and risk data loss. The safer default for an import feature is "touch nothing the controller didn't create."

If the operator wants the PVC gone, they delete it explicitly after the SeiNode is gone. This is symmetric with `kubectl delete deployment` not deleting PVCs on the pods it owned.

## Validation retry semantics (decided)

When the import branch of `ensure-data-pvc` finds the PVC in a transient-bad state (missing, `Pending` during initial bind, `Released` mid-adoption, etc.), **the task retries indefinitely with exponential backoff and surfaces the latest validation error via a `Condition` on the SeiNode status.**

Rationale:
- Matches the existing pattern for other reconcilable-but-might-take-a-while tasks in the controller — tasks return `Running` until the external state is acceptable, and only mark `Failed` on truly terminal errors.
- Resilient to ordering races: an operator who applies the SeiNode and the PVC in close succession (or in either order) sees the controller converge once both resources exist.
- Typo detection remains feasible: a misspelled PVC name produces a persistent `Condition: ImportPVCReady=False, Reason: PVCNotFound` that is visible in `kubectl describe seinode`, `kubectl get seinode -o wide`, and any Prometheus alert built on `kube_seinode_status_condition`. Operator fixes the spec; reconciliation converges.
- A pod stuck in `Pending` because its PVC isn't bound will already fire the cluster's existing `KubePodNotReady` alerts, so we don't lose failure signal by retrying rather than giving up.

The LLD pins down the specifics: exact backoff curve, exact Condition name and Reason strings for each failure mode (`PVCNotFound`, `PVCTerminating`, `PVCNotBound`, `CapacityTooSmall`, `UnderlyingPVMissing`, etc.), and whether those Reasons become part of the public contract for alerting.

## Open questions (for the LLD)

None remaining at the direction level. All prior open questions are now decided:
- **Scope**: Shape A only (PVC by name)
- **Deletion semantics**: controller never deletes imported PVCs
- **Validation requirements**: seven-item table above
- **Init plan interaction**: unchanged — import is a PVC-source substitution inside `ensure-data-pvc`, nothing else
- **Retry semantics**: retry indefinitely with exponential backoff, surface via Condition

The LLD is now about implementation specifics (exact backoff curve, Condition schema, unit-test matrix, CRD schema details) rather than direction.

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
- `internal/planner/bootstrap.go` — unchanged (init plan is unchanged); `ensure-data-pvc` internally branches on `spec.dataVolume.import.pvcName`
- `api/v1alpha1/seinode_types.go` — where `spec.dataVolume.import` gets added
