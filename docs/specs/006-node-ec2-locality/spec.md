# Feature Specification: One validator per EC2 for a benchmark

**Feature Branch**: `006-node-ec2-locality`

**Created**: 2026-09-09

**Status**: Draft

**Blocks**: a trustworthy benchmark. Two validators on one EC2 instance share that
instance's network bandwidth, so the result measures the instance, not the chain.
The controller can already keep one Sei pod alone on a worker node. Today the
operator turns that on one node at a time, and cannot see where the pods landed.
This spec makes the isolation a network-level setting and reports the placement.
Requirement 5 holds one open question, and Requirement 3 holds the other: the
surface, and the default.

**Input**: the Benchmark Party transcript, 2026-09-04. The fence below holds the
originator's words. The writing rules govern this document, not its source.

```text
An excellent way to confuse yourself is to have multiple validators on an EC2
instance that shares bandwidth, and then the performance changes if they happen
to be scheduled across multiple instances. Network bandwidth is hardcoded to the
class of the instance, and it is the bottleneck we hit when we push hard. We need
to check the mapping of which pod ended up on which worker node, and give the node
groups the right taint and affinity to avoid running multiple validators on one
node.
```

The controller already renders a hard pod anti-affinity for a node that opts into
single-tenant scheduling. That pod then does not share a worker node with any
other Sei pod. Today the operator opts in one node at a time, through an
experimental annotation, and reads the placement by hand. This spec keeps that
mechanism and gives it a network-level setting and a reported placement.

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach, because that gap is the honest part.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | pod anti-affinity, status | whether the controller reconciles correctly |

## Glossary

- **Controller**: the sei-k8s-controller reconciler that renders a validator's pod and writes the SeiNetwork status.
- **Operator**: a person who runs a benchmark and reads its placement.
- **SeiNetwork**: the CRD that owns a pool of validators.
- **Benchmark validator**: a validator child of a benchmark SeiNetwork.
- **Worker node**: a Kubernetes node. On this platform it is one EC2 instance.
- **Single-tenant scheduling**: a placement where a validator's pod does not share its worker node with another Sei pod.
- **Anti-affinity**: the pod scheduling rule that keeps a pod off a worker node that already holds a matching pod.
- **Placement**: the map from each validator to the worker node that runs it.

## Boundary Context

- **Sits within**: the pod scheduling of a benchmark validator, and the SeiNetwork status that reports its placement.
- **Owns**: the network-level single-tenant setting, and the report of each validator's worker node on the status.
- **Does not own**: the provisioning of the worker nodes or their instance type. The platform owns the node pool.
- **Does not own**: the CPU, memory, or disk of a validator. The `configurable-node-resources` work item owns those.
- **Does not own**: the placement of an RPC node. This spec covers a benchmark validator.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - Each benchmark validator lands on its own EC2 (Priority: P1)

An operator runs a benchmark on a single-tenant validator network. The controller
renders each validator so its pod does not share a worker node with another Sei
pod. No two validators share an EC2 instance, so no two share its bandwidth, and
the throughput reflects the chain.

**Why this priority**: this story fixes the invalid result. Two validators on one
EC2 share its bandwidth, so the benchmark measures the instance, not the chain.

**Independent Test**: Run a single-tenant validator network. Confirm that no two
validator pods share a worker node.

**Acceptance Scenarios**:

1. **Given** a single-tenant validator network with three validators, **When** the controller schedules them, **Then** each validator pod lands on a different worker node.
2. **Given** a single-tenant validator on a worker node, **When** a later Sei pod schedules, **Then** the later pod does not land on that worker node.

---

### User Story 2 - An operator confirms the placement (Priority: P2)

An operator wants to confirm the isolation before trusting a run. The operator
reads the SeiNetwork status and sees the worker node that runs each validator, so
the operator can confirm that no two validators share a worker node.

**Why this priority**: this ranks below Story 1. An isolation the operator cannot
see is an isolation the operator cannot trust, and a silent co-location looks like
a slow chain.

**Independent Test**: Read the SeiNetwork status. Confirm that it names the worker
node of each validator.

**Acceptance Scenarios**:

1. **Given** a running validator network, **When** the operator reads the SeiNetwork status, **Then** the status names the worker node of each validator.

---

### User Story 3 - One setting for the whole validator pool (Priority: P2)

An operator wants single-tenant scheduling for a benchmark network without
annotating each validator. The operator sets it once on the SeiNetwork, and the
controller applies it to every validator child.

**Why this priority**: this ranks with Story 2. A per-node opt-in is easy to miss
on one validator, and one co-located validator makes the whole run invalid.

**Independent Test**: Mark a SeiNetwork single-tenant. Confirm that the controller
applies single-tenant scheduling to every validator child.

**Acceptance Scenarios**:

1. **Given** a SeiNetwork marked single-tenant, **When** the controller creates its validators, **Then** the controller applies single-tenant scheduling to every validator child.

### Edge Cases

- What happens when the node pool has fewer worker nodes than validators? A validator with no free node stays pending, and the operator sees the pending validator on the status — see Requirement 4.
- What happens when a Sei pod in another namespace targets the same worker node? The anti-affinity matches across namespaces, so that pod does not land there — see Requirement 2.
- What happens to an RPC node under the same setting? This spec covers a validator; the RPC placement is out of scope.

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: A single-tenant validator does not share a worker node

**Objective:** As an operator, I want each benchmark validator on its own worker
node, so that no two share an EC2 instance and its bandwidth.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL render a hard anti-affinity on a single-tenant validator, so its pod does not share a worker node with another Sei pod.
2. THE controller SHALL set the anti-affinity topology to the worker node, so the isolation is one pod per EC2 instance.

The controller renders this anti-affinity today. This requirement is a regression
guard, not new work.

### Requirement 2: The isolation holds against a later pod

**Objective:** As an operator, I want a validator's isolation to survive a later
pod, so that nothing lands beside it after it schedules.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL render an anti-affinity on every Sei pod, so the pod avoids a worker node that already holds a single-tenant validator.
2. THE controller SHALL match the anti-affinity across every namespace, so a co-tenant in another namespace does not share the node.

The controller renders this defensive term on every Sei pod today, across every
namespace. This requirement is a regression guard, not new work.

### Requirement 3: The network sets single-tenant for every validator

**Objective:** As an operator, I want one setting for the whole pool, so that I do
not annotate each validator.

**Traces to:** User Story 3

#### Acceptance Criteria

1. WHEN an operator marks a SeiNetwork single-tenant, THE controller SHALL render every validator child as single-tenant.
2. THE controller SHALL apply single-tenant scheduling to a benchmark validator network [NEEDS CLARIFICATION: by default, or only when an operator marks the network? Owner: the platform team. Decide by: 2026-10-31.]

### Requirement 4: The controller reports the placement

**Objective:** As an operator, I want to see each validator's worker node, so that
I can confirm that no two share one.

**Traces to:** User Story 2

#### Acceptance Criteria

1. THE controller SHALL report the worker node of each validator on the SeiNetwork status.
2. WHILE a validator has no worker node, THE controller SHALL report the validator as pending on the SeiNetwork status.

### Requirement 5: The single-tenant setting has one surface

**Objective:** As an operator, I want one surface for the single-tenant setting, so
that every benchmark network isolates the same way.

**Traces to:** User Story 3

#### Acceptance Criteria

1. THE controller SHALL take the single-tenant setting of a benchmark network from [NEEDS CLARIFICATION: a per-network CRD field, or the existing `sei.io/dedicated-node` annotation, where the controller propagates it from the network to every validator child? Owner: the platform team. Decide by: 2026-10-31.]

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: No two validators of a single-tenant network share a worker node.
  *Verifier:* judgement — a platform engineer runs a single-tenant validator network and confirms each validator pod is on a different worker node.
- **SC-002**: A later Sei pod does not land on a single-tenant validator's worker node, from any namespace.
  *Verifier:* judgement — a platform engineer schedules a later Sei pod in a different namespace and confirms it avoids the validator's worker node.
- **SC-003**: A single-tenant SeiNetwork renders every validator single-tenant.
  *Verifier:* judgement — a platform engineer marks a SeiNetwork single-tenant and confirms the controller applies single-tenant scheduling to every validator child.
- **SC-004**: The SeiNetwork status names the worker node of each validator.
  *Verifier:* judgement — a platform engineer reads the SeiNetwork status and confirms the worker node of each validator.
- **SC-005**: A pending validator appears on the SeiNetwork status.
  *Verifier:* judgement — a platform engineer runs more validators than worker nodes and confirms the status reports the pending validator.
- **SC-006**: The single-tenant setting comes from one surface.
  *Verifier:* not built — Requirement 5, criterion 1 is open, so the chosen surface does not exist yet.
- **SC-007**: A benchmark validator network has a stated default.
  *Verifier:* not built — Requirement 3, criterion 2 is open, so the default does not exist yet.

## Assumptions

- The controller already renders a hard pod anti-affinity for a node that opts into single-tenant scheduling, on the worker-node topology, across every namespace. Requirement 1 and Requirement 2 restate this as a regression guard. Requirement 4 is new: the controller does not report the placement today.
- One worker node is one EC2 instance, so an anti-affinity on the worker node isolates at the EC2 level.
- The single-tenant mechanism is annotation-driven and experimental today. Requirement 3 and Requirement 5 promote it to a network-level setting.
- The platform owns the node pool. This spec assumes the pool holds at least as many worker nodes as the network holds validators. A validator with no free node stays pending, and Requirement 4, criterion 2 reports it.

## Out of scope

- The provisioning of the worker nodes or the choice of instance type. The platform owns the node pool.
- The CPU, memory, or disk of a validator. That work lives in the `configurable-node-resources` work item.
- The placement of an RPC node. This spec covers a benchmark validator.
- The network bandwidth of an instance type. This spec isolates the validator; it does not size the instance.
