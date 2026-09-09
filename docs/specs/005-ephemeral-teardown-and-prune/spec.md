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
policy is already `Delete` on the classes a benchmark's `full` and `validator`
nodes use; only the archive class retains. The disk nevertheless survives a
teardown, because nothing deletes the PVC. See Corrections below.

## Corrections (2026-09-09)

This section supersedes any statement below that contradicts it. The Draft was
written from the Benchmark Party transcript without checking the running code.
Three of its load-bearing factual claims are false, and two of its requirements
cannot both be satisfied. Each correction below cites the file and line that
settles it.

### C-1. The reclaim policy is already `Delete` on every benchmark class

For a PVC the controller generates, the storage class is a pure function of node
mode. `func NodeMode` (`internal/noderesource/noderesource.go:305-317`) collapses
the API's mode sub-specs, mapping `Replayer` -- and any node with no mode
sub-spec set -- to `full`. `func DefaultStorageForMode`
(`internal/noderesource/noderesource.go:350-362`) then selects the class:

| effective mode | class | reclaim policy |
|---|---|---|
| `archive` | `classArchive` = `gp3-archive` | `Retain` (`config/storage/storage-classes.yaml:20`) |
| `validator` | `classPerf` = `gp3-10k-750` | `Delete` (`config/storage/storage-classes.yaml:7`) |
| `full` (includes `replayer`, and any unset mode) | `classPerf` = `gp3-10k-750` | `Delete` (`config/storage/storage-classes.yaml:7`) |
| `seed` | `classDefault` | not defined in this repo -- see below |

Because `NodeMode` only ever returns `archive`, `validator`, `seed`, or `full`,
the `default:` branch of `DefaultStorageForMode` is unreachable through it, and
`classDefault` is reached only by `seed`.

Two limits on this table, stated rather than glossed:

- Class **names** are injected per cluster through `SEI_CONTROLLER_CONFIG`
  (`internal/platform/load.go`), so the name-to-policy binding is a deployment
  fact, not a repository fact. Harbor's deployed `sei-controller-config` sets
  `classPerf: gp3-10k-750`.
- `classDefault` (`gp3` on harbor) is **not** defined in this repository. Its
  reclaim policy cannot be established from these manifests and is not asserted
  here.

What this does settle: the transcript's premise -- "the persistent volume reclaim
policy is Retain, so the disk survives" -- is false for `full` and `validator`,
which are the shapes a benchmark network actually runs. Those land on a `Delete`
class today.

### C-2. There is no benchmark node mode, and no namespace input to class selection

The SeiNode API has five modes: `archive`, `validator`, `seed`, `full`,
`replayer`. "Benchmark" is not among them. This spec's own Glossary defines a
benchmark node by **namespace**, but `DefaultStorageForMode` takes only a mode and
a `PlatformConfig` -- it receives no namespace and cannot distinguish a benchmark
node from a non-benchmark one.

### C-3. Requirements 2 and 4 are mutually unsatisfiable as written

The claim is scoped to PVCs the **controller generates** under a single
`PlatformConfig`, which is what Requirements 2 and 4 place obligations on ("THE
controller SHALL place ..."). For those, class is a pure function of mode, and
configuration is loaded once at startup (`internal/platform/load.go`). A benchmark
`full` node and a non-benchmark `full` node are the same mode, so they resolve to
the same class. Requirement 2 demands the benchmark PVC land on a `Delete` class;
Requirement 4 demands the non-benchmark PVC land on a `Retain` class and forbids a
`Delete` class. The controller cannot satisfy both, because it receives no
namespace and has no other input that separates the two.

**One supported path escapes this**, and the spec should acknowledge it rather
than claim a blanket impossibility: `spec.dataVolume.import.pvcName`
(`api/v1alpha1/seinode_types.go`, `DataVolumeSpec`) lets an operator attach a
pre-existing PVC in the same namespace. `ensure_pvc.go:55` routes imports away
from creation entirely, and the storage class of an imported PVC is
**intentionally not validated** -- "the importer's responsibility." So a
non-benchmark node *can* end up on a `Retain` volume beside a benchmark node on a
`Delete` one. That is a manual operator act on a volume the controller never
places and never deletes, not automatic namespace-based placement -- but it means
the honest claim is the narrower one above.

Requirement 4 is additionally **not** a description of current behavior, and must
not be implemented as a regression guard. Today every non-archive node --
including non-benchmark `full`, `validator`, and (via `NodeMode`) `replayer` nodes
-- sits on a `Delete` class, which violates R4 criterion 2 as written.

Implementing R4 literally would therefore be a **behavior change, not a guard**:
it would move newly generated non-benchmark volumes onto a `Retain` class. That is
defensible against R4's own data-preservation objective, and it would not migrate
existing volumes. But it inverts the cost objective this spec opens with, and it
shifts those volumes to manual reclamation -- the same cleanup burden the spec
exists to remove. The trade is real and should be decided deliberately, not
inherited from a requirement written before the code was checked.

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
(`api/v1alpha1/seinetwork_types.go:99`, `+kubebuilder:default=Retain`; the
reconciler independently defaults an empty policy to `Retain` at
`internal/controller/seinetwork/controller.go:132`). When the policy is `Retain`,
`controller.go:135` takes the branch that removes the network owner reference from
the child SeiNodes (`internal/controller/seinetwork/nodes.go:285`). The chain is
therefore:

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

For a controller-generated PVC, this is lost at step 3 -- upstream of everything
Requirements 1, 2, and 4 govern. A correct reclaim policy cannot fix a PVC that is
never deleted.

Two scope limits on that statement. An **imported** PVC
(`spec.dataVolume.import`) is never deleted by the controller on any path, by
design (`internal/controller/node/controller.go:390`), so it survives teardown
independently of deletion policy. And the `Retain` default is an API default, not
an observation: it establishes what an unset network gets, not that every affected
deployed network left it unset.

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

### C-7. The `Delete` path works, and the field is mutable before deletion

Two properties were checked before recommending C-6, because a recommendation that
assumes an untested mechanism is how this spec went wrong in the first place.

**The `Delete` cascade completes.** SeiNetwork deletion under `Delete` leaves the
child owner references in place, so the children are garbage-collected; each
SeiNode's finalizer deletes its data PVC
(`internal/controller/node/controller.go:398`), and the class's `Delete` reclaim
releases the disk. The one mechanism that could have silently broken this is the
`GenerationChangedPredicate` on both primary watches
(`internal/controller/seinetwork/controller.go`, `internal/controller/node/controller.go`):
if a deletion did not change `metadata.generation`, the update could be filtered
and the finalizer would never run. It does change it -- the API server increments
`generation` when it marks a finalized object as deleting, on the
non-graceful-deletion path that custom resources take
(`k8s.io/apiserver`, `pkg/registry/generic/registry/store.go`, `markAsDeleting`).
The deletion update is not filtered.

**`deletionPolicy` is mutable.** It carries no CEL `XValidation` immutability rule
and no validating webhook; the generated CRD
(`config/crd/sei.io_seinetworks.yaml`) shows only a default, description, enum,
and type. Only `spec.genesis`, `spec.replicas`, `spec.dataVolume`, and
`spec.resources` are immutable.

The consequence for remediation is an ordering constraint, and it is sharp. A live
network can be patched from `Retain` to `Delete` **before** teardown, and the
cascade then works. Once a `Retain` teardown has already stripped the owner
references and removed the parent, the cascade is unrecoverable and the leftover
SeiNodes and PVCs require manual cleanup. Patching after deletion is too late.

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

**Correction (2026-09-09): these criteria describe behavior that does not exist.**
Today only `archive` resolves to a `Retain` class; non-benchmark `full`,
`validator`, and `replayer` nodes all sit on a `Delete` class, violating criterion
2 as written. They are therefore **not** a regression guard, and must not be
implemented as one.

Implementing them would be a deliberate behavior change: newly generated
non-benchmark volumes would move to a `Retain` class and thereafter require manual
reclamation. That serves R4's data-preservation objective but works against this
spec's cost objective. Decide it on its merits.

Criterion 1 also cannot hold alongside Requirement 2 for controller-generated
PVCs, because class selection keys on mode alone and receives no namespace -- see
C-2 and C-3, including the `spec.dataVolume.import` path that is the one exception.

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
(C-2). Note that `DataVolumeSpec` (`api/v1alpha1/seinode_types.go`) currently
carries **only** `Import` -- there is no existing size or class field to extend --
and it already bears a CEL rule preventing `import` from being unset once
configured, so any new field there needs its own immutability decision.

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

**Correction (2026-09-09) on SC-001, SC-002 versus SC-005:** these criteria do not
share a fate, and the difference is the most useful thing in this section.

- **SC-005** ("a deleted benchmark node leaves no PVC") **passes today** for a
  controller-generated PVC. Deleting a SeiNode directly runs its finalizer, which
  deletes the data PVC (`internal/controller/node/controller.go:398`), and the
  `Delete` reclaim then releases the disk. Imported PVCs are explicitly exempt
  (`controller.go:390`).
- **SC-001 and SC-002** ("a benchmark *teardown* leaves no PVC / no disk") fail,
  for the reason in C-5.

The gap between them is the whole bug. The machinery works from the node entry
point and is never reached from the operator's real entry point -- deleting a
*network* via a teardown PR -- because `deletionPolicy: Retain` orphans the nodes
first. A test written at the node entry point passes while the reported bug
remains unfixed.

## Assumptions

- The controller creates a node's data volume as a standalone PVC through the ensure-data-pvc task, and owns it through the SeiNode. This spec sets the storage class reclaim; it does not add the volume model.
- The owner reference already garbage-collects the data PVC when the node is deleted, so Requirement 1 restates existing behavior as a regression guard. The open lever is the storage class reclaim in Requirement 2.
- ~~The storage class reclaim policy defaults to `Retain` and protects the disk. This spec changes the reclaim for a benchmark volume only.~~ **False for the benchmark path -- see C-1.** The perf class `gp3-10k-750`, which every `full` and `validator` node resolves to, reclaims with `Delete`. Of the classes this repository defines, only `gp3-archive` retains, and it is reachable only by `archive` mode, which no benchmark node uses. The reclaim policy of `classDefault` is not established by this repository.
- A benchmark node deletion arrives from the `crd-ownership-and-deletion` cascade. That work item owns the cascade, and its default policy for a benchmark network gates the teardown. **This assumption is unmet -- see C-5.** That default is `Retain`, so it gates the teardown *shut*: the cascade orphans the child SeiNodes instead of deleting them, and no PVC is ever deleted. Spec 005 was carved out of a precondition that was never satisfied, and placed the fix for it out of scope.
- A benchmark run mints a fresh consensus identity at its genesis ceremony. A reclaim of a benchmark disk therefore loses nothing that a later run cannot recreate.
- A non-benchmark node keeps its `Retain` reclaim, because its data cannot be recreated. This spec changes the benchmark path only.

## Out of scope

- The deletion cascade from a SeiNetwork down to its pods. That work lives in the `crd-ownership-and-deletion` work item.
- The size or the throughput of a disk. That work lives in the `configurable-node-resources` work item.
- The `Retain` reclaim for a non-benchmark node. This spec keeps it.
- A snapshot of a benchmark disk before a teardown.
