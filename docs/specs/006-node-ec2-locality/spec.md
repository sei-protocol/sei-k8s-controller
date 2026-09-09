# Feature Specification: Single-tenant scheduling for benchmark nodes

**Feature Branch**: `006-node-ec2-locality`

**Created**: 2026-09-09

**Status**: Draft

**Unblocks**: a trustworthy benchmark. This spec adds a typed scheduling field to
the SeiNode and the SeiNetwork, propagates it to every validator child, and
reports the placement. Two validators on one EC2 instance share that instance's
network bandwidth, so the result measures the instance, not the chain. The
controller can already render the anti-affinity that keeps one Sei pod alone on a
worker node. A validator cannot reach it: a SeiNetwork gives its children no
scheduling field, and the controller removes a hand-applied annotation on the next
reconcile.

**Input**: the Benchmark Party transcript, 2026-09-04, and review on pull request
522. The fence below holds the originator's words. The writing rules govern this
document, not its source.

```text
An excellent way to confuse yourself is to have multiple validators on an EC2
instance that shares bandwidth, and then the performance changes if they happen
to be scheduled across multiple instances. Network bandwidth is hardcoded to the
class of the instance, and it is the bottleneck we hit when we push hard. We need
to check the mapping of which pod ended up on which worker node, and give the node
groups the right taint and affinity to avoid running multiple validators on one
node.
```

Today the single-tenant mechanism is an experimental annotation on a standalone
SeiNode. Review chose to replace it with a typed field on both CRDs. A benchmark
validator can then request the isolation. The API server validates the request,
and the CRD schema makes it discoverable. The controller propagates the field the
same way it propagates the existing config and sidecar fields, so the field is the
smaller change as well as the more conventional one.

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach, because that gap is the honest part.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | CRD field shape, status, conditions | whether the controller reconciles correctly |
| Google AIP | an enum over a boolean | whether the resource model is the right one |

## Glossary

- **Controller**: the sei-k8s-controller reconciler that renders a node's pod and writes the SeiNetwork status.
- **Operator**: a person who runs a benchmark and reads its placement.
- **SeiNetwork**: the CRD that owns a pool of validators.
- **SeiNode**: the CRD for a single node. A validator child is a SeiNode; a standalone RPC node is a SeiNode.
- **Validator child**: a SeiNode the SeiNetwork creates and owns.
- **Worker node**: a Kubernetes node. On this platform it is one EC2 instance.
- **Scheduling field**: the typed field this spec adds to each CRD, which carries the node-isolation value.
- **Node isolation**: the scheduling value. It is one of `Shared` or `Dedicated`.
- **Effective node isolation**: the scheduling field value. For a standalone SeiNode with no field value, it is the legacy annotation instead. A validator child has no annotation fallback.
- **Requester term**: the anti-affinity term on a `Dedicated` pod that keeps it off a worker node holding another Sei pod.
- **Defensive term**: the anti-affinity term on every Sei pod that keeps it off a worker node holding a `Dedicated` pod.
- **Single-tenant nodepool**: a Karpenter nodepool with a taint only Sei pods tolerate, so no other workload lands on its worker nodes.
- **Legacy annotation**: the experimental `sei.io/dedicated-node` annotation on a standalone SeiNode, which the scheduling field supersedes.
- **Placement**: the map from each validator to the worker node that runs it.

## Boundary Context

- **Sits within**: the scheduling surface on the SeiNode and the SeiNetwork, the propagation to validator children, and the SeiNetwork status.
- **Owns**: the typed scheduling field on both CRDs, its propagation to every validator child, the anti-affinity the controller renders from it, and the placement report on the status.
- **Does not own**: the provisioning of a single-tenant nodepool and its taint. The platform team owns the nodepool.
- **Does not own**: the CPU, memory, or disk of a node. The `configurable-node-resources` work item owns those.
- **Does not own**: the network bandwidth of an instance type. This spec isolates the node; it does not size the instance.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - Each benchmark validator lands on its own EC2 (Priority: P1)

An operator runs a benchmark. The operator sets the scheduling field on the
SeiNetwork to `Dedicated`. The controller copies that value to every validator
child and renders a hard anti-affinity on each one. No two validators share an
EC2 instance. The nodepool the validators land on admits no other workload, so no
non-Sei pod takes the instance bandwidth, and the throughput reflects the chain.

**Why this priority**: this story delivers the isolation. Without it, a validator
cannot request single-tenant scheduling at all, because the annotation the
standalone path uses does not reach a validator child.

**Independent Test**: Set the scheduling field on a SeiNetwork to `Dedicated`.
Confirm that every validator child carries the value. Confirm that no two
validator pods share a worker node.

**Acceptance Scenarios**:

1. **Given** a SeiNetwork with the scheduling field set to `Dedicated` and three validators, **When** the controller reconciles, **Then** each validator child carries the `Dedicated` value.
2. **Given** the same network, **When** the controller schedules the validators, **Then** each validator pod lands on a different worker node.
3. **Given** a `Dedicated` validator on a single-tenant nodepool, **When** a non-Sei workload requests that nodepool, **Then** the workload does not land on the validator's worker node.

---

### User Story 2 - The operator confirms placement before trusting a run (Priority: P2)

An operator wants to confirm the isolation before trusting a benchmark. The
operator reads the SeiNetwork status and sees the worker node that runs each
validator. The operator can then confirm that no two validators share a worker
node. When a pod moves, the status follows it.

**Why this priority**: this ranks below Story 1. An isolation the operator cannot
see is an isolation the operator cannot trust, and a silent co-location looks like
a slow chain.

**Independent Test**: Read the SeiNetwork status. Confirm that it names the worker
node of each validator. Reschedule a validator pod. Confirm that the status names
the new worker node.

**Acceptance Scenarios**:

1. **Given** a running validator network, **When** the operator reads the SeiNetwork status, **Then** the status names the worker node of each validator.
2. **Given** a rescheduled validator pod, **When** the controller reconciles, **Then** the status names the validator's new worker node.

---

### User Story 3 - A standalone node uses the same field, and existing users keep working (Priority: P2)

An operator sets the scheduling field on a standalone SeiNode to `Dedicated`. The
node schedules single-tenant, through the same field a SeiNetwork uses. An
operator who still sets the legacy annotation, and sets no field, keeps the old
behavior.

**Why this priority**: this ranks with Story 2. One field for both CRDs keeps the
surface consistent, and the backward-compatible annotation keeps a current user
from breaking on the upgrade.

**Independent Test**: Set the scheduling field on a standalone SeiNode. Confirm
that its pod schedules single-tenant. Set only the legacy annotation on another
SeiNode. Confirm that its pod still schedules single-tenant.

**Acceptance Scenarios**:

1. **Given** a standalone SeiNode with the scheduling field set to `Dedicated`, **When** the controller schedules it, **Then** its pod does not share a worker node with another Sei pod.
2. **Given** a SeiNode with the legacy annotation and no scheduling field, **When** the controller schedules it, **Then** its pod does not share a worker node with another Sei pod.

### Edge Cases

- What happens when a non-Sei workload tolerates the nodepool taint and lands on a `Dedicated` node? The anti-affinity applies only to Sei pods, so the guarantee holds only on a single-tenant nodepool — see Requirement 4.
- What happens when a validator pod moves to a new worker node? The controller re-reads the placement each reconcile and updates the status — see Requirement 5.
- What happens when a SeiNode sets both the scheduling field and the legacy annotation? The controller uses the field — see Requirement 1.
- What happens when the network holds more validators than the nodepool has worker nodes? A validator with no node stays pending, and the status reports it — see Requirement 5.

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: A typed scheduling field on both CRDs

**Objective:** As an operator, I want a validated scheduling field on the SeiNode
and the SeiNetwork, so that I request single-tenant scheduling and see it in the
CRD schema.

**Traces to:** User Story 1, User Story 3

#### Acceptance Criteria

1. THE SeiNode CRD SHALL carry a scheduling field that holds a node-isolation value.
2. THE SeiNetwork CRD SHALL carry a scheduling field with the same shape as the SeiNode field.
3. THE SeiNode CRD and THE SeiNetwork CRD SHALL each define the node-isolation value as the enumeration `Shared` and `Dedicated`.
4. IF a manifest omits the node-isolation value, THEN THE API server SHALL apply the default `Shared`.
5. WHILE a standalone SeiNode carries no scheduling field, THE controller SHALL read the effective node isolation from the legacy annotation.
6. WHILE a SeiNode carries a scheduling field value, THE controller SHALL read the effective node isolation from the field and ignore the legacy annotation.

### Requirement 2: The controller renders hard anti-affinity for a Dedicated node

**Objective:** As an operator, I want a `Dedicated` node kept off a shared worker
node, so that no two Sei pods share an EC2 instance and its bandwidth.

**Traces to:** User Story 1, User Story 3

#### Acceptance Criteria

1. WHILE a node's effective node isolation is `Dedicated`, THE controller SHALL render the requester term that keeps the pod off a worker node holding another Sei pod.
2. THE controller SHALL set the anti-affinity topology to the worker node, so the isolation is one pod per EC2 instance.
3. THE controller SHALL render the defensive term on every Sei pod, so the term spans every namespace and keeps the pod off a worker node holding a `Dedicated` pod.

### Requirement 3: The SeiNetwork propagates the field to every validator child

**Objective:** As an operator, I want one setting to configure the whole pool, so
that I do not set each validator.

**Traces to:** User Story 1

#### Acceptance Criteria

1. WHEN the controller creates a validator child, THE controller SHALL copy the network scheduling field into that child.
2. WHEN the network scheduling field changes, THE controller SHALL copy the new value into every validator child.
3. THE controller SHALL propagate the scheduling field through the same child-sync path it uses for the config and sidecar fields.

### Requirement 4: A Dedicated node schedules onto a single-tenant nodepool

**Objective:** As an operator, I want a `Dedicated` node to exclude every other
workload, so that a non-Sei co-tenant does not take the instance bandwidth.

**Traces to:** User Story 1

#### Acceptance Criteria

1. WHILE a node's effective node isolation is `Dedicated`, THE controller SHALL schedule the pod onto the single-tenant nodepool the platform team provisions.
2. THE controller SHALL render the toleration for the single-tenant nodepool taint.
3. THE controller SHALL render the node affinity that places the pod on the single-tenant nodepool.
4. IF the platform provisions no single-tenant nodepool, THEN THE controller SHALL still render the requester term. The guarantee then holds against another Sei pod only.

### Requirement 5: The controller reports the placement

**Objective:** As an operator, I want to see each validator's worker node, so that
I can confirm that no two share one.

**Traces to:** User Story 2

#### Acceptance Criteria

1. THE controller SHALL report the worker node of each validator on the node list in the SeiNetwork status.
2. THE controller SHALL read the worker node from the validator pod on each reconcile, so the report follows a rescheduled pod.
3. WHILE a validator has no worker node, THE controller SHALL report the validator as pending on the node list.

### Key Entities

- **Scheduling field**: belongs to one SeiNode or one SeiNetwork.
- **Placement**: one entry in the placement map. The controller reads it from the validator pod.

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: The SeiNode and the SeiNetwork each carry the scheduling field with the same shape.
  *Verifier:* judgement — a platform engineer reads the two CRD schemas and confirms the field and its enumeration match.
- **SC-002**: A `Dedicated` node renders the hard, worker-node-topology anti-affinity.
  *Verifier:* judgement — a platform engineer runs `TestBuildNodePodSpec_Dedicated_AddsRequesterTermAndLabel` in `internal/noderesource` and confirms the anti-affinity terms.
- **SC-003**: A `Dedicated` value on a SeiNetwork reaches every validator child.
  *Verifier:* judgement — a platform engineer sets the network field and confirms every validator child carries the value.
- **SC-004**: A node with no scheduling field and no legacy annotation behaves as `Shared`.
  *Verifier:* judgement — a platform engineer creates a node with neither the field nor the annotation and confirms the controller renders no requester term.
- **SC-005**: A SeiNode with only the legacy annotation still schedules single-tenant.
  *Verifier:* judgement — a platform engineer sets only the annotation and confirms the pod avoids a worker node holding another Sei pod.
- **SC-006**: A `Dedicated` node schedules onto a single-tenant nodepool when one exists, and renders the requester term when none does.
  *Verifier:* judgement — a platform engineer runs a `Dedicated` node with and without a single-tenant nodepool and confirms the placement in each case.
- **SC-007**: The SeiNetwork status names each validator's worker node and follows a reschedule.
  *Verifier:* judgement — a platform engineer reads the status, reschedules a pod, and confirms the status names the new worker node.
- **SC-008**: A pending validator appears on the SeiNetwork status.
  *Verifier:* judgement — a platform engineer runs more validators than worker nodes and confirms the status reports the pending validator.
- **SC-009**: A SeiNode that sets both the field and the legacy annotation uses the field.
  *Verifier:* judgement — a platform engineer sets a `Dedicated` field and a conflicting annotation and confirms the controller follows the field.
- **SC-010**: A `Shared` Sei pod still carries the defensive term.
  *Verifier:* judgement — a platform engineer reads a `Shared` pod and confirms it carries the defensive anti-affinity term.

## Assumptions

- The node-isolation value is an enumeration of `Shared` and `Dedicated`, not a boolean. Google AIP prefers a descriptive value over a boolean, because a two-state boolean often grows a third state. A later tier, such as a shared-but-spread value, then fits the same field.
- The scheduling field defaults to `Shared`, so an existing node keeps its current behavior. The benchmark harness sets `Dedicated` for a benchmark run.
- The controller propagates the scheduling field through the existing child-sync path, the same path that carries the config, sidecar, and pod-label fields. This replaces the annotation path, which cannot reach a validator child. The SeiNetwork gives its children no annotation, and the controller reconciles a hand-applied child annotation back to none on the next loop.
- The controller already renders the hard cross-namespace anti-affinity, with the worker node as the topology, for a `Dedicated` pod. Requirement 2 is a regression guard on that rendering. Requirement 1, Requirement 3, and Requirement 5 are new work: the typed field, the propagation, and the placement report.
- The controller already renders a per-mode nodepool toleration and node affinity. Requirement 4 points that placement at a single-tenant nodepool; provisioning that nodepool is new platform work.
- Full instance exclusivity depends on a single-tenant nodepool with a taint only Sei pods tolerate. The platform team provisions it. Without it, `Dedicated` keeps one Sei pod per worker node, but a non-Sei co-tenant that tolerates the nodepool taint can still share the instance.
- One worker node is one EC2 instance, so an anti-affinity on the worker node isolates at the EC2 level.
- The single-tenant nodepool is the binding capacity, not the whole cluster. Where the pool holds fewer worker nodes than the network holds `Dedicated` validators, a validator stays pending and Requirement 5 reports it. The platform team sizes the pool.
- The controller may report the pending state as a field on the node-list entry, or as a status condition. If it uses a condition, the always-present, stable-reason, and observed-generation discipline applies.

## Out of scope

- The provisioning of the single-tenant nodepool and its taint. The platform team owns the nodepool.
- The CPU, memory, or disk of a node. That work lives in the `configurable-node-resources` work item.
- The network bandwidth of an instance type. This spec isolates the node; it does not size the instance.
- A per-validator scheduling value that differs across the pool. This spec sets one value for the whole pool.
