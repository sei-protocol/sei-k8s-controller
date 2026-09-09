# Feature Specification: Ephemeral teardown and prune

**Feature Branch**: `005-ephemeral-teardown-and-prune`

**Created**: 2026-09-08

**Status**: Draft

**Blocks**: a low-cost benchmark cycle. An engineer who tears down a benchmark
namespace expects the volumes and their disks to be gone. Today a delete PR
leaves the resources in place, and a retained disk keeps costing money. This spec
reclaims the benchmark disks, prunes the resources whose source files a delete PR
removes, and keeps the retain protection for a volume whose data cannot be
recreated. Requirement 5 holds the one open question: the single source of the
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

The controller creates a node's data volume as a standalone PVC, owned by the
SeiNode, so a node delete garbage-collects the PVC. The storage class reclaim
policy is `Delete` on every class a benchmark node uses; only the archive class
retains. The disk nevertheless survives a teardown, because nothing deletes the
PVC. See Corrections below.

## Corrections (2026-09-09)

This section supersedes any statement below that contradicts it. The Draft was
written from the Benchmark Party transcript without checking the running code.
Three of its load-bearing factual claims are false, and two of its requirements
cannot both be satisfied. Each correction below cites the file and line that
settles it.

### C-1. The reclaim policy is already `Delete` on every benchmark class

`DefaultStorageForMode` selects a storage class by node **mode**
(`internal/noderesource/noderesource.go:350-362`):

| mode | class | reclaim policy |
|---|---|---|
| `archive` | `classArchive` = `gp3-archive` | `Retain` |
| `full`, `validator` | `classPerf` = `gp3-10k-750` | `Delete` |
| `seed` | `classDefault` = `gp3` | `Delete` |
| default (e.g. `replayer`) | `classDefault` = `gp3` | `Delete` |

Reclaim policies are set in `config/storage/storage-classes.yaml:7,20`; the class
names are injected per cluster through `SEI_CONTROLLER_CONFIG`, and harbor's
deployed `sei-controller-config` sets `classPerf: gp3-10k-750`, which has carried
`reclaimPolicy: Delete` since its first commit. There was never a `Retain` era for
the benchmark path.

The transcript's premise -- "the persistent volume reclaim policy is Retain, so
the disk survives" -- is therefore false for every shape a benchmark runs.

### C-2. There is no benchmark node mode, and no namespace input to class selection

The SeiNode API has five modes: `archive`, `validator`, `seed`, `full`,
`replayer`. "Benchmark" is not among them. This spec's own Glossary defines a
benchmark node by **namespace**, but `DefaultStorageForMode` takes only a mode and
a `PlatformConfig` -- it receives no namespace and cannot distinguish a benchmark
node from a non-benchmark one.

### C-3. Requirements 2 and 4 are mutually unsatisfiable as written

A benchmark full node and a non-benchmark full node are the same mode, so they
resolve to the same storage class. Requirement 2 demands the benchmark PVC land on
a `Delete` class; Requirement 4 demands the non-benchmark PVC land on a `Retain`
class and forbids it from landing on a `Delete` class. Under the current selection
function both cannot hold at once.

Requirement 4 is additionally **not** a description of current behavior, and must
not be implemented as a regression guard. Today every non-archive node --
including non-benchmark `full`, `validator`, and `seed` nodes -- sits on a `Delete`
class, which violates R4 criterion 2 as written. Implementing R4 literally would
move production non-benchmark volumes onto a `Retain` class and create a new,
larger disk leak than the one this spec set out to fix.

### C-4. The prune requirement is already satisfied

Requirement 3 asks the workspace reconciler to prune. `prune: true` is already set
at `platform/clusters/harbor/engineers/base/sync.yaml:14`, in the single shared
base that renders all nine per-engineer reconcilers. There is nothing to enable.

### C-5. The real cause: the cascade's default gates the teardown shut

The Assumptions section already names the dependency -- "a benchmark node deletion
arrives from the `crd-ownership-and-deletion` cascade ... its default policy for a
benchmark network gates the teardown" -- but does not notice that the default
gates it **shut**.

`SeiNetwork.spec.deletionPolicy` defaults to `Retain`
(`api/v1alpha1/seinetwork_types.go:129`, `+kubebuilder:default=Retain`), and the
reconciler strips the owner reference from child SeiNodes when the policy is
`Retain` (`internal/controller/seinetwork/controller.go:135`). The observed chain
is therefore:

1. A delete PR removes the SeiNetwork manifest.
2. Flux prunes correctly and the SeiNetwork object is deleted.
3. Because `deletionPolicy` is `Retain`, the controller deliberately orphans the
   child SeiNodes rather than deleting them.
4. The orphaned SeiNodes keep running. Nothing deletes them -- not GitOps, which
   never owned the controller-created genesis validators, and not garbage
   collection, whose owner reference was just removed.
5. The PVC is never deleted, so the SeiNode finalizer's PVC cleanup never runs.
6. The reclaim policy is never consulted, because reclaim only applies to a PVC
   that is actually deleted.

Every disk this spec set out to reclaim is lost at step 3, upstream of everything
Requirements 1, 2, and 4 govern. A correct reclaim policy cannot fix a PVC that is
never deleted.

### C-6. Consequence for this spec's scope

As written, Requirements 1-4 are either already satisfied (R1, R2, R3) or harmful
if implemented (R4), and none of them addresses the failure the spec exists to
fix. The load-bearing change is that a benchmark SeiNetwork must carry
`deletionPolicy: Delete`. This spec places that in Out of scope and assigns it to
`crd-ownership-and-deletion`, so **Spec 005 cannot deliver its own Blocks
statement**. Either the cascade's benchmark default moves into this spec's scope,
or this spec should be closed in favor of the cascade work item.

Changing the CRD-wide default is not the recommended shape: the `Retain` default
is deliberate, because a validator pool's PVCs hold ceremony-generated,
unrecoverable consensus identity. The narrower change is for the benchmark harness
to set `deletionPolicy: Delete` explicitly on the networks it creates.

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach, because that gap is the honest part.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | owner references, garbage collection, reclaim policy | whether the controller reconciles correctly |
| OpenGitOps | the prune of a resource whose source is gone | whether the overlay is correct |

## Glossary

- **Controller**: the sei-k8s-controller reconciler that creates a node's data PVC.
- **Operator**: a person who runs and tears down a benchmark namespace.
- **Benchmark node**: a SeiNode in a benchmark namespace. Its volume is ephemeral.
- **Non-benchmark node**: a node outside a benchmark namespace. Its volume holds data worth keeping.
- **Data PVC**: the standalone persistent volume claim the controller creates for a node's data. The controller owns it through the SeiNode.
- **Disk**: the cloud volume that backs a PVC. On this platform it is an EBS volume.
- **Owner reference**: the field on the data PVC that names its SeiNode, so Kubernetes garbage-collects the PVC when the node is deleted.
- **Garbage collection**: the Kubernetes behavior that deletes a child once its owner is gone.
- **Reclaim policy**: the value on the storage class that decides whether a deleted PVC also deletes its disk. It holds `Retain` or `Delete`.
- **Workspace reconciler**: the Flux Kustomization that applies the workspace manifests. The platform team owns its settings.
- **Prune**: the GitOps step where the workspace reconciler deletes a resource once its source file is gone.
- **Delete PR**: a pull request that removes a benchmark manifest from the workspace repository.
- **Teardown**: the operator action that removes a benchmark namespace and everything in it.

## Boundary Context

- **Sits within**: the volume lifecycle of a benchmark node, and the GitOps teardown of a benchmark namespace.
- **Owns**: the storage class reclaim of a benchmark PVC, and the prune requirement on the workspace reconciler. It keeps the owner reference that already garbage-collects the PVC.
- **Does not own**: the deletion cascade from a SeiNetwork down to its pods. The `crd-ownership-and-deletion` work item owns that.
- **Does not own**: the size or the throughput of a disk. The `configurable-node-resources` work item owns those.
- **Does not own**: the data protection for a non-benchmark node. This spec keeps the `Retain` reclaim there.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - A deleted benchmark node takes its disk with it (Priority: P1)

An operator deletes a benchmark node at the end of a run. The node owns its data
PVC, so garbage collection deletes the PVC with the node. The disk goes with the
PVC, because the benchmark storage class reclaims it. No disk keeps costing money,
and the next run starts clean.

**Why this priority**: this story fixes the reported cost leak. A retained disk
keeps costing money after the run, and a stale volume collides with the next run.

**Independent Test**: Delete a benchmark node. Confirm that no PVC and no disk of
that node remains.

**Acceptance Scenarios**:

1. **Given** a benchmark node with a data PVC, **When** the operator deletes the node, **Then** garbage collection deletes the owner-referenced PVC.
2. **Given** the deleted PVC, **When** the storage provisioner runs, **Then** the provisioner deletes the disk, because the storage class reclaims it.

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
node. The non-benchmark node holds data worth keeping, so its PVC stays on a
`Retain` reclaim. The teardown reclaims the benchmark disks and leaves the
non-benchmark disk in place.

**Why this priority**: this ranks with Story 2. A reclaim that reached a
non-benchmark disk would lose data that cannot be recreated.

**Independent Test**: Tear down a benchmark namespace beside a non-benchmark node.
Confirm that the benchmark disks are gone and the non-benchmark disk remains.

**Acceptance Scenarios**:

1. **Given** a non-benchmark node, **When** the operator tears down a neighboring benchmark namespace, **Then** the non-benchmark disk remains.

### Edge Cases

- What happens when a delete PR removes a manifest but the workspace reconciler has no prune? The resource keeps running with no source file — see Requirement 3.
- What happens to a benchmark disk when its node is deleted? Garbage collection deletes the PVC, and the storage class reclaims the disk — see Requirement 1 and Requirement 2.
- What happens to a non-benchmark node in the same teardown? The controller keeps its PVC on a `Retain` reclaim — see Requirement 4.

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: A benchmark PVC is garbage-collected with its node

**Objective:** As an operator, I want a benchmark node's PVC to go when the node
goes, so that a teardown leaves no volume claim behind.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL set an owner reference from a benchmark node's data PVC to the SeiNode, so garbage collection deletes the PVC with the node.

### Requirement 2: A benchmark PVC reclaims its disk

**Objective:** As an operator, I want a benchmark disk to go when its PVC goes, so
that a teardown leaves no disk to pay for.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL place a benchmark PVC on a storage class whose reclaim policy is `Delete`, so the provisioner deletes the disk with the PVC.

**Correction (2026-09-09):** already satisfied. Every mode a benchmark runs
(`full`, `validator`, `seed`, `replayer`) already resolves to a `Delete` class --
see C-1. This requirement needs no implementation, and satisfying it does not
reclaim any disk, because the PVC is never deleted -- see C-5.

### Requirement 3: A delete PR prunes the resource

**Objective:** As an operator, I want a delete PR to remove the cluster resource,
so that a teardown does more than remove a file.

**Traces to:** User Story 2

#### Acceptance Criteria

1. WHEN a delete PR removes a resource's source file, THE workspace reconciler SHALL prune that resource from the cluster.

The prune is a setting on the workspace Kustomization. No controller code
implements this requirement.

**Correction (2026-09-09):** already satisfied. `prune: true` is set at
`platform/clusters/harbor/engineers/base/sync.yaml:14` -- see C-4. Note also that
prune can only ever reach resources Flux owns; the genesis validator SeiNodes are
created by the controller and were never in Flux's inventory, so prune
structurally cannot delete them.

### Requirement 4: A non-benchmark node keeps its data

**Objective:** As an operator, I want a non-benchmark node to keep its data, so
that a benchmark teardown does not lose a node's state.

**Traces to:** User Story 3

#### Acceptance Criteria

1. THE controller SHALL place a non-benchmark PVC on a storage class whose reclaim policy is `Retain`.
2. THE controller SHALL NOT place a non-benchmark PVC on a storage class whose reclaim policy is `Delete`.

**Correction (2026-09-09): do not implement these criteria as written.** They
describe behavior that does not exist and must not be created. Today only
`archive` resolves to a `Retain` class; every non-benchmark `full`, `validator`,
and `seed` node sits on a `Delete` class, violating criterion 2. Implementing
these literally would move production non-benchmark volumes onto `Retain` and
create a larger leak than the one this spec targets. Criterion 1 is also
unsatisfiable alongside Requirement 2, because class selection keys on mode alone
and cannot tell a benchmark node from a non-benchmark one -- see C-2 and C-3.

### Requirement 5: The ephemeral reclaim has one source

**Objective:** As an operator, I want one source for the ephemeral reclaim, so
that every teardown reclaims the same way.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL take the reclaim of a benchmark disk from [NEEDS CLARIFICATION: a benchmark storage class, or a per-network setting? Owner: the platform team. Decide by: 2026-10-31.]

The two candidates:

- A benchmark storage class whose reclaim policy is `Delete`. It needs no new field.
- A per-network setting the controller applies to the PVC storage class. It keeps the choice with the network.

**Correction (2026-09-09): the question as posed does not decide anything.** Both
candidates select a *reclaim policy*, and reclaim policy is not what leaks the
disk -- reclaim is never reached, because the PVC is never deleted (C-5). Choosing
either candidate leaves the leak fully intact.

The decision that actually matters is the one this spec placed out of scope: what
sets `deletionPolicy` on a benchmark SeiNetwork, and to what value. The
recommended shape is for the benchmark harness to set `deletionPolicy: Delete`
explicitly per network, rather than flipping the CRD-wide default, which is
deliberately `Retain` to protect unrecoverable validator consensus identity.

If a benchmark-versus-non-benchmark distinction is still wanted in class selection
after that, it needs an input `DefaultStorageForMode` does not currently receive
(C-2) -- most naturally a field on the existing `spec.dataVolume.storage` API home,
which today carries size only.

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
- **SC-006**: Every benchmark PVC takes its reclaim from the source that Requirement 5 selects.
  *Verifier:* not built — Requirement 5, criterion 1 is open, so the chosen source does not exist yet.
  **Correction (2026-09-09):** this criterion cannot be satisfied or verified while Requirement 5 is open, and per the Requirement 5 correction it would not reclaim a disk even once closed. Any work item that lists SC-006 in its acceptance criteria is citing a criterion that is by its own verifier not built.

**Correction (2026-09-09) on SC-001, SC-002, SC-005:** these three are the criteria
that encode the actual goal, and none of them passes today, for the reason given in
C-5. Note the asymmetry in the Independent Tests: deleting a *node* directly (SC-005)
already behaves correctly, because owner-referenced garbage collection and a `Delete`
reclaim both work. The failure appears only from the operator's real entry point --
deleting a *network* via a teardown PR -- where `deletionPolicy: Retain` orphans the
nodes before any of that machinery runs. A test written at the node entry point will
pass while the reported bug remains unfixed.

## Assumptions

- The controller creates a node's data volume as a standalone PVC through the ensure-data-pvc task, and owns it through the SeiNode. This spec sets the storage class reclaim; it does not add the volume model.
- The owner reference already garbage-collects the data PVC when the node is deleted, so Requirement 1 restates existing behavior as a regression guard. The open lever is the storage class reclaim in Requirement 2.
- ~~The storage class reclaim policy defaults to `Retain` and protects the disk. This spec changes the reclaim for a benchmark volume only.~~ **False -- see C-1.** The default class `gp3` and the perf class `gp3-10k-750` both reclaim with `Delete`. Only `gp3-archive` retains, and it is reachable only by `archive` mode, which no benchmark node uses.
- A benchmark node deletion arrives from the `crd-ownership-and-deletion` cascade. That work item owns the cascade, and its default policy for a benchmark network gates the teardown. **This assumption is unmet -- see C-5.** That default is `Retain`, so it gates the teardown *shut*: the cascade orphans the child SeiNodes instead of deleting them, and no PVC is ever deleted. Spec 005 was carved out of a precondition that was never satisfied, and placed the fix for it out of scope.
- A benchmark run mints a fresh consensus identity at its genesis ceremony. A reclaim of a benchmark disk therefore loses nothing that a later run cannot recreate.
- A non-benchmark node keeps its `Retain` reclaim, because its data cannot be recreated. This spec changes the benchmark path only.

## Out of scope

- The deletion cascade from a SeiNetwork down to its pods. That work lives in the `crd-ownership-and-deletion` work item.
- The size or the throughput of a disk. That work lives in the `configurable-node-resources` work item.
- The `Retain` reclaim for a non-benchmark node. This spec keeps it.
- A snapshot of a benchmark disk before a teardown.
