# Feature Specification: Predictable ownership and deletion

**Feature Branch**: `004-crd-ownership-and-deletion`

**Created**: 2026-09-08

**Status**: Draft

**Blocks**: a clean benchmark cycle. A SeiNetwork defaults to `Retain`, so a
delete orphans its validators, StatefulSets, and pods. A stale set keeps running,
and the next run collides with it. This spec keeps `Retain` as a deliberate
protection, makes the policy in effect visible, and gives the `Delete` path a
complete cascade. It does not remove `Retain`. Requirement 6 holds the one open
question: the default for a benchmark network.

**Input**: the Benchmark Party transcript, 2026-09-04. The fence below holds the
originator's words. The writing rules govern this document, not its source.

```text
Deletion of the same network does not delete the stateful sets that it created.
That's a bug. The RPCs got deleted but not the validators. The retain policy is
protecting a validator from being deleted, but it is highly surprising, so we
should remove that feature. And when you delete a stateful set directly it just
comes back, because the node recreates it — so the link is there, but the network
deletion is not cascading.
```

The platform team reviewed the transcript and kept the `Retain` protection as an
explicit choice, because a delete under `Retain` guards a validator's
ceremony-generated consensus identity. This spec makes the deletion predictable
and visible, and gives the ephemeral benchmark path a clean cascade.

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach, because that gap is the honest part.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | owner references, garbage collection | whether the controller reconciles correctly |
| controller-runtime | owner references, the reconcile loop | idempotence of a specific reconcile |

## Glossary

- **Controller**: the sei-k8s-controller reconciler that owns the SeiNetwork and SeiNode resources.
- **Operator**: a person who creates, runs, and deletes networks, including benchmark networks.
- **SeiNetwork**: the CRD that bootstraps a chain and owns a pool of validators.
- **Validator child**: a SeiNode the SeiNetwork creates and owns.
- **StatefulSet**: the workload a SeiNode creates and owns to run its node process.
- **Owner reference**: the field on a child that names its parent, so Kubernetes garbage-collects the child with the parent.
- **Garbage collection**: the Kubernetes behavior that deletes a child once its owner is gone.
- **Cascade**: the deletion that follows the owner references, from a parent down to every descendant.
- **Orphan** (noun): a child the controller keeps after a delete removes its parent.
- **Orphan** (verb): to remove the parent owner reference from a child, so the child survives the parent's deletion.
- **DeletionPolicy**: the SeiNetwork field that selects a cascade or an orphan. It holds `Delete` or `Retain`, and defaults to `Retain`.
- **Consensus identity**: the ceremony-generated validator key on a validator's volume. A loss of this key is permanent.
- **Teardown**: the operator action that removes a benchmark network and its workload.

## Boundary Context

- **Sits within**: the ownership and deletion path across the SeiNetwork, the SeiNode, and their StatefulSets and pods.
- **Owns**: the owner-reference linkage up the chain, the cascade on a `Delete` policy, and the record of the policy in effect.
- **Does not own**: the removal of the `Retain` policy. This spec keeps the policy and its protection. Whether `Retain` stays the default for a benchmark network is open — see Requirement 6.
- **Does not own**: the reclaim of a volume or its disk. The `ephemeral-teardown-and-prune` work item owns that.
- **Does not own**: the Flux prune of a workspace directory. The workspace reconciliation owns that.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - A deleted benchmark network leaves nothing behind (Priority: P1)

An operator finishes a run and deletes a benchmark SeiNetwork on the `Delete`
policy. The controller keeps the owner references, so Kubernetes garbage-collects
every validator child, StatefulSet, and pod. Nothing keeps running, and the next
run starts clean.

**Why this priority**: this story fixes the reported bug. A stale validator set
that keeps running collides with the next run and wastes cluster capacity.

**Independent Test**: Delete a SeiNetwork on the `Delete` policy. Confirm that no
child SeiNode, StatefulSet, or pod remains.

**Acceptance Scenarios**:

1. **Given** a SeiNetwork on the `Delete` policy with three validators, **When** the operator deletes the SeiNetwork, **Then** the controller deletes all three validator children.
2. **Given** the same deletion, **When** garbage collection runs, **Then** the StatefulSets and pods of that SeiNetwork are gone.

---

### User Story 2 - An operator navigates from a network to its pods (Priority: P2)

An operator opens a SeiNetwork and follows it down to its validator pods. The
operator wants to follow the owner references from the SeiNetwork to each SeiNode,
to each StatefulSet, and to each pod.

**Why this priority**: this ranks below Story 1. Without an intact chain, an
operator cannot see which pods a network owns, or tell a running network from a
stale one.

**Independent Test**: Read a validator pod and walk its owner references up to the
SeiNetwork. Confirm that each step names its parent.

**Acceptance Scenarios**:

1. **Given** a running SeiNetwork, **When** the operator reads a validator pod, **Then** the pod names its StatefulSet.
2. **Given** the same pod, **When** the operator reads its StatefulSet, **Then** the StatefulSet names its SeiNode.
3. **Given** the same StatefulSet, **When** the operator reads its SeiNode, **Then** the SeiNode names the SeiNetwork.

---

### User Story 3 - A kept or recreated child is explained, not a surprise (Priority: P2)

An operator meets a child that outlives a delete, or returns after one. A `Retain`
delete keeps the validators, because their consensus identity cannot be recovered.
A StatefulSet deleted under a live SeiNode returns, because the SeiNode owns it.
The controller records why in each case, so the operator is not surprised.

**Why this priority**: this ranks with Story 2. The `Retain` default surprised the
team, and a recreated StatefulSet surprised them too. A recorded reason turns each
surprise into an expected, explained outcome.

**Independent Test**: Delete a SeiNetwork on the `Retain` policy, and separately
delete a StatefulSet under a live SeiNode. Confirm that the controller records the
reason in each case.

**Acceptance Scenarios**:

1. **Given** a SeiNetwork on the `Retain` policy, **When** the operator deletes it, **Then** the children keep running and carry the recorded reason.
2. **Given** a live SeiNode, **When** the operator deletes its StatefulSet, **Then** the controller recreates it and records that the SeiNode still exists.

### Edge Cases

- What happens when an operator deletes a StatefulSet directly under a live SeiNode? The controller recreates it — see Requirement 5.
- What happens to a validator's consensus identity on a `Delete` cascade? The controller deletes the workload; the `ephemeral-teardown-and-prune` work item decides the fate of the volume and the key.
- What happens to the volumes on a cascade? The controller deletes the workload; the `ephemeral-teardown-and-prune` work item owns the volume reclaim.

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: The ownership chain is complete

**Objective:** As an operator, I want each child to name its parent, so that a
reader can walk the chain from the SeiNetwork to a pod.

**Traces to:** User Story 2

#### Acceptance Criteria

1. WHILE the SeiNetwork exists, THE controller SHALL set an owner reference from each validator child to the SeiNetwork.
2. THE controller SHALL set an owner reference from each StatefulSet to its SeiNode.
3. THE controller SHALL keep the pod owner reference that the StatefulSet sets.

Requirement 4, criterion 2 removes the SeiNetwork reference on a `Retain` delete.
That is the only exception.

### Requirement 2: A Delete policy lets garbage collection remove the tree

**Objective:** As an operator, I want a delete to remove the whole tree, so that no
stale workload survives.

**Traces to:** User Story 1

#### Acceptance Criteria

1. WHEN the operator deletes a SeiNetwork on the `Delete` policy, THE controller SHALL keep the owner reference from each validator child.
2. THE controller SHALL rely on Kubernetes garbage collection to remove each validator child, StatefulSet, and pod.

### Requirement 3: A Delete leaves nothing behind

**Objective:** As an operator, I want the deleted tree to stay deleted, so that the
next run starts clean.

**Traces to:** User Story 1

#### Acceptance Criteria

1. WHEN the operator deletes a SeiNetwork on the `Delete` policy, THE controller SHALL leave no validator child, StatefulSet, or pod of that SeiNetwork.
2. WHILE a `Delete` cascade runs, THE controller SHALL NOT recreate a child it is deleting.

Requirement 5, criterion 1 is the opposite case: a child deleted under a live
parent returns.

### Requirement 4: The controller records why it kept the children

**Objective:** As an operator, I want a retained delete explained, so that a
running set after a delete is an expected outcome, not a surprise.

**Traces to:** User Story 3

#### Acceptance Criteria

1. WHEN the operator deletes a SeiNetwork on the `Retain` policy, THE controller SHALL keep the validator children.
2. WHEN the controller orphans a child, THE controller SHALL record the `Retain` decision and its reason on the child.

### Requirement 5: The controller records why it recreated a child

**Objective:** As an operator, I want a recreated child explained, so that a
StatefulSet that returns after a manual delete is not a surprise.

**Traces to:** User Story 3

#### Acceptance Criteria

1. WHILE a SeiNode is live, IF an operator deletes its StatefulSet, THEN THE controller SHALL recreate it.
2. WHEN the controller recreates a child, THE controller SHALL record that it recreated the child because the SeiNode still exists.

Requirement 3, criterion 2 is the exception: a controller in a `Delete` cascade
does not recreate a child it is deleting.

### Requirement 6: The default policy for a benchmark network

**Objective:** As an operator, I want the default policy to suit an ephemeral
benchmark network, so that a teardown leaves no cost behind.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL default the `DeletionPolicy` of a benchmark network to [NEEDS CLARIFICATION: `Delete` or `Retain`? Owner: the platform team. Decide by: before Barcelona.]

The two candidates:

- `Delete` for an ephemeral eng-namespace network. It matches the ephemeral benchmark expectation.
- The current `Retain` default, with the harness selecting `Delete` on teardown. It protects an unrecoverable consensus identity.

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: A `Delete` of a SeiNetwork removes every child SeiNode, StatefulSet, and pod.
  *Verifier:* judgement — a platform engineer deletes a `Delete`-policy SeiNetwork and confirms no child resource remains.
- **SC-002**: A validator pod's owner references walk up to its SeiNetwork.
  *Verifier:* judgement — a platform engineer reads a pod and confirms the chain from pod to StatefulSet to SeiNode to SeiNetwork.
- **SC-003**: A `Delete` cascade recreates nothing it is deleting.
  *Verifier:* judgement — a platform engineer deletes a `Delete`-policy SeiNetwork and confirms no child returns.
- **SC-004**: A `Retain` delete keeps the children and records the reason.
  *Verifier:* judgement — a platform engineer deletes a `Retain`-policy SeiNetwork and confirms the children run and carry the recorded reason.
- **SC-005**: A StatefulSet deleted under a live SeiNode returns with a recorded reason.
  *Verifier:* judgement — a platform engineer deletes a StatefulSet and confirms the controller recreates it and records that the SeiNode still exists.
- **SC-006**: A benchmark teardown removes the network.
  *Verifier:* judgement — the benchmark owner tears down a benchmark network and confirms no network, child, or pod remains.
- **SC-007**: A new benchmark network carries the default policy that Requirement 6 selects.
  *Verifier:* not built — Requirement 6, criterion 1 is open, so the expected default does not exist yet.

## Assumptions

- The controller already sets owner references up the chain and already supports a `Delete` and a `Retain` policy. Requirement 1 restates the linkage as a regression guard; it does not add the ownership model.
- A `Retain` policy protects a validator's consensus identity, which cannot be recovered. This spec keeps that protection.
- The benchmark harness selects the `Delete` policy on a teardown, so an ephemeral chain leaves no cost behind.
- The volume reclaim is a separate concern. A cascade deletes the workload; the `ephemeral-teardown-and-prune` work item decides the fate of the volume.

## Out of scope

- The removal of the `Retain` policy. The transcript asks for it; review kept the protection as an explicit choice. Requirement 6 holds the one open question about the default for a benchmark network.
- The reclaim of a volume or its disk. That work lives in the `ephemeral-teardown-and-prune` work item.
- The Flux prune of a workspace directory after a delete PR merges. The workspace reconciliation owns that.
- A rename of the `DeletionPolicy` field or its values.
