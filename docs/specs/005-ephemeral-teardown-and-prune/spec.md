# Feature Specification: Ephemeral teardown and prune

**Feature Branch**: `005-ephemeral-teardown-and-prune`

**Created**: 2026-09-08

**Status**: Draft

**Blocks**: a low-cost benchmark cycle. An engineer who tears down a benchmark
namespace expects the volumes and their disks to be gone. Today a delete PR
leaves the resources in place, and a retained disk keeps costing money. This spec
reclaims the volumes and their disks, prunes the resources whose source files a
delete PR removes, and keeps the retain protection for a volume whose data cannot
be recreated. Requirement 5 holds the one open question: the single source of the
ephemeral behavior.

**Input**: the Benchmark Party transcript, 2026-09-04. The fence below holds the
originator's words. The writing rules govern this document, not its source.

```text
Resources are not deleted after the delete PR merges — maybe we are missing a
prune option in the customization. And the persistent volume reclaim policy is
Retain, so the disk survives. The load test is a special case: treat load-testing
as ephemeral infrastructure. Massage the deletion policy for the PVCs so a
teardown leaves no cost behind.
```

The StatefulSet retention policy defaults to retain a node's volume, so a delete
does not lose its data. The platform team reviewed the transcript and kept that
protection for a volume whose data cannot be recreated. This spec gives the
ephemeral benchmark path a clean prune and reclaim.

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach, because that gap is the honest part.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | PVC retention, reclaim policy | whether the controller reconciles correctly |
| OpenGitOps | the prune of a resource whose source is gone | whether the overlay is correct |

## Glossary

- **Controller**: the sei-k8s-controller reconciler that creates a node's StatefulSet and volume claim.
- **Operator**: a person who runs and tears down a benchmark namespace.
- **Benchmark node**: a SeiNode in a benchmark namespace. Its volume is ephemeral.
- **Non-benchmark node**: a node outside a benchmark namespace. Its volume holds data worth keeping.
- **PVC**: the persistent volume claim a node mounts for its data.
- **Disk**: the cloud volume that backs a PVC. On this platform it is an EBS volume.
- **Reclaim policy**: the value on the storage class that decides whether a deleted PVC also deletes its disk. It holds `Retain` or `Delete`.
- **Retention policy**: the StatefulSet value that decides whether a deleted node also deletes its PVC. It holds `Retain` or `Delete`.
- **Workspace reconciler**: the Flux Kustomization that applies the workspace manifests. The platform team owns its settings.
- **Prune**: the GitOps step where the workspace reconciler deletes a resource once its source file is gone.
- **Delete PR**: a pull request that removes a benchmark manifest from the workspace repository.
- **Teardown**: the operator action that removes a benchmark namespace and everything in it.

## Boundary Context

- **Sits within**: the volume lifecycle of a benchmark node, and the GitOps teardown of a benchmark namespace.
- **Owns**: the retention policy on a benchmark node, the storage class of a benchmark PVC, and the prune requirement on the workspace reconciler.
- **Does not own**: the deletion cascade from a SeiNetwork down to its pods. The `crd-ownership-and-deletion` work item owns that.
- **Does not own**: the size or the throughput of a disk. The `configurable-node-resources` work item owns those.
- **Does not own**: the data protection for a non-benchmark node. This spec keeps the `Retain` default there.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - A deleted benchmark node takes its disk with it (Priority: P1)

An operator deletes a benchmark node at the end of a run. The node's PVC goes with
the node, and the disk goes with the PVC. No disk keeps costing money, and the
next run starts clean.

**Why this priority**: this story fixes the reported cost leak. A retained disk
keeps costing money after the run, and a stale volume collides with the next run.

**Independent Test**: Delete a benchmark node. Confirm that no PVC and no disk of
that node remains.

**Acceptance Scenarios**:

1. **Given** a benchmark node with a volume, **When** the operator deletes the node, **Then** the StatefulSet controller deletes the node's PVC.
2. **Given** the deleted PVC, **When** the storage provisioner runs, **Then** the provisioner deletes the disk that backed the PVC.

---

### User Story 2 - A delete PR removes the resource, not just the file (Priority: P2)

An operator opens a delete PR that removes a benchmark manifest from the
workspace. The operator wants the merge to delete the cluster resource, not to
leave it running with no source file.

**Why this priority**: this ranks below Story 1. Without a prune, a delete PR
removes the file and leaves the resource, so a namespace teardown does nothing.

**Independent Test**: Merge a delete PR that removes a benchmark manifest. Confirm
that the workspace reconciler deletes the cluster resource.

**Acceptance Scenarios**:

1. **Given** a benchmark resource with a manifest in the workspace, **When** the operator merges a PR that removes the manifest, **Then** the workspace reconciler prunes the resource from the cluster.

---

### User Story 3 - A non-benchmark node keeps its data (Priority: P2)

An operator tears down a benchmark namespace that sits beside a non-benchmark
node. The non-benchmark node holds data worth keeping, so its volume stays on
`Retain`. The teardown reclaims the benchmark disks and leaves the non-benchmark
disk in place.

**Why this priority**: this ranks with Story 2. A reclaim that reached a
non-benchmark disk would lose data that cannot be recreated.

**Independent Test**: Tear down a benchmark namespace beside a non-benchmark node.
Confirm that the benchmark disks are gone and the non-benchmark disk remains.

**Acceptance Scenarios**:

1. **Given** a non-benchmark node, **When** the operator tears down a neighboring benchmark namespace, **Then** the non-benchmark disk remains.

### Edge Cases

- What happens when a delete PR removes a manifest but the workspace reconciler has no prune? The resource keeps running with no source file — see Requirement 3.
- What happens to a benchmark disk when its node is deleted? The PVC goes with the node, and the disk goes with the PVC — see Requirement 1 and Requirement 2.
- What happens to a non-benchmark node in the same teardown? The controller keeps its volume on `Retain` — see Requirement 4.

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: A benchmark node deletes its PVC on delete

**Objective:** As an operator, I want a benchmark node's PVC to go when the node
goes, so that a teardown leaves no volume claim behind.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL set the retention policy of a benchmark node to `Delete`, so the StatefulSet controller deletes the PVC with the node.

### Requirement 2: A benchmark PVC deletes its disk

**Objective:** As an operator, I want a benchmark disk to go when its PVC goes, so
that a teardown leaves no disk to pay for.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL place a benchmark PVC on a storage class whose reclaim policy is `Delete`, so the provisioner deletes the disk with the PVC.

### Requirement 3: A delete PR prunes the resource

**Objective:** As an operator, I want a delete PR to remove the cluster resource,
so that a teardown does more than remove a file.

**Traces to:** User Story 2

#### Acceptance Criteria

1. WHEN a delete PR removes a resource's source file, THE workspace reconciler SHALL prune that resource from the cluster.

The prune is a setting on the workspace Kustomization. No controller code
implements this requirement.

### Requirement 4: A non-benchmark node keeps its data

**Objective:** As an operator, I want a non-benchmark node to keep its data, so
that a benchmark teardown does not lose a node's state.

**Traces to:** User Story 3

#### Acceptance Criteria

1. THE controller SHALL keep the retention policy of a non-benchmark node at `Retain`.
2. THE controller SHALL NOT place a non-benchmark PVC on a storage class whose reclaim policy is `Delete`.

### Requirement 5: The ephemeral behavior has one source

**Objective:** As an operator, I want one source for the ephemeral behavior, so
that every teardown reclaims the same way.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL take the ephemeral behavior of a benchmark volume from [NEEDS CLARIFICATION: the storage class, or a per-network setting? Owner: the platform team. Decide by: 2026-10-31.]

The two candidates:

- A benchmark storage class whose reclaim policy is `Delete`. It needs no new field.
- A per-network setting the controller applies to the retention policy and the storage class. It keeps the choice with the network.

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: A benchmark teardown leaves no PVC of that namespace.
  *Verifier:* judgement — a platform engineer tears down a benchmark namespace and confirms no PVC remains.
- **SC-002**: A benchmark teardown leaves no disk of that namespace.
  *Verifier:* judgement — a platform engineer tears down a benchmark namespace and confirms no disk remains.
- **SC-003**: A delete PR merge prunes the cluster resource.
  *Verifier:* judgement — a platform engineer merges a delete PR and confirms the resource leaves the cluster.
- **SC-004**: A non-benchmark disk survives a neighboring benchmark teardown.
  *Verifier:* judgement — a platform engineer tears down a benchmark namespace beside a non-benchmark node and confirms the non-benchmark disk remains.
- **SC-005**: A deleted benchmark node leaves no PVC.
  *Verifier:* judgement — a platform engineer deletes a benchmark node and confirms no PVC remains.
- **SC-006**: Every benchmark PVC takes its ephemeral behavior from the source that Requirement 5 selects.
  *Verifier:* not built — Requirement 5, criterion 1 is open, so the chosen source does not exist yet.

## Assumptions

- The controller already creates a node's PVC from a storage class and a volume claim template. This spec sets the retention and the storage class; it does not add the volume model.
- The StatefulSet retention policy defaults to `Retain`, and the storage class reclaim policy protects the disk. This spec changes both for a benchmark volume only.
- A benchmark node deletion arrives from the `crd-ownership-and-deletion` cascade. That work item owns the cascade, and its default policy for a benchmark network gates the teardown.
- A benchmark run mints a fresh consensus identity at its genesis ceremony. A reclaim of a benchmark disk therefore loses nothing that a later run cannot recreate.
- A non-benchmark node keeps its `Retain` default, because its data cannot be recreated. This spec changes the benchmark path only.

## Out of scope

- The deletion cascade from a SeiNetwork down to its pods. That work lives in the `crd-ownership-and-deletion` work item.
- The size or the throughput of a disk. That work lives in the `configurable-node-resources` work item.
- The `Retain` default for a non-benchmark node. This spec keeps it.
- A snapshot of a benchmark disk before a teardown.
