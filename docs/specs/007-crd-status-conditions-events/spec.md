# Feature Specification: Ready means the network produces blocks, and the plan shows task progress

**Feature Branch**: `007-crd-status-conditions-events`

**Created**: 2026-09-09

**Status**: Draft

**Unblocks**: a benchmark an operator can trust to have started. This spec makes
the `Ready` phase mean the validators produce blocks, and makes the plan's task
sequence visible as a tick box. The SeiNetwork already carries a conditions
framework, a phase, and plan events; two gaps remain. The network reports `Ready`
when its pods reach the `Running` phase. That is not the same as the validators
producing blocks, so an operator starts load against a network that does not yet
run. And the task sequence reports only a plan-level `Active` or `Complete`, so an
operator cannot see which task the plan is on.

**Input**: the Benchmark Party transcript, 2026-09-04. The fence below holds the
originator's words. The writing rules govern this document, not its source.

```text
It should be straightforward to turn the sequence of jobs and their execution and
their status into a simple textual tick box. Use the primitives that exist already
in Kubernetes to be super expressive about the status of the object. All the tasks
we execute inside the controller already have everything they need to report an
event; we just need to register those events. And readiness does not become ready
until the network is actually running, whereas this thing became ready while the
nodes were only just coming up. But be careful: if a pod is not ready, Kubernetes
will not route traffic to it, so if you pick the readiness condition too tight you
stop the network from communicating.
```

The controller already computes a phase, seeds a set of always-present conditions,
and emits a plan-lifecycle event. This spec does not rebuild that framework. It
corrects what `Ready` means and adds the per-task visibility the framework does
not yet surface.

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach, because that gap is the honest part.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | the status and event surface | whether the controller reconciles correctly |
| controller-runtime | the reconcile that writes the status | idempotence of a specific reconcile |

## Glossary

- **Controller**: the sei-k8s-controller reconciler that computes the SeiNetwork status.
- **Operator**: a person who runs a benchmark and reads the network status.
- **SeiNetwork**: the CRD that owns a pool of validators.
- **Validator**: a child SeiNode of the SeiNetwork.
- **Running phase**: the child SeiNode phase that means the pod and its sidecar are up.
- **Producing**: the state in which the validators advance the chain height, so the network runs.
- **Ready**: the SeiNetwork phase that this spec redefines to mean producing.
- **Producing signal**: the status signal the controller reads to decide that the network produces.
- **Producing gate**: the check the controller applies to the producing signal before it reports `Ready`.
- **Readiness probe**: the pod probe that gates whether Kubernetes routes traffic to the pod.
- **Plan**: the ordered task sequence the controller runs to bring the network or a node up.
- **Task**: one step in a plan.
- **Task progress**: the per-task state the controller reports, so the sequence reads as a tick box.
- **Event**: a Kubernetes Event the controller records against the SeiNetwork.

## Boundary Context

- **Sits within**: the SeiNetwork status computation — the phase, the conditions, the plan report, and the events.
- **Owns**: the meaning of the `Ready` phase, the producing signal that gates it, the per-task progress on the plan report, and the per-task events.
- **Does not own**: the placement of a validator on a worker node, or the node-list report of it. The `node-ec2-locality` work item owns those status fields.
- **Does not own**: the deletion and ownership status. The `crd-ownership-and-deletion` work item owns that.
- **Does not own**: how a validator produces a block. seid owns consensus. This spec reads a producing signal; it does not define production.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - Ready means the network produces blocks (Priority: P1)

An operator waits for a benchmark network to come up. The operator wants `Ready`
to mean the validators produce blocks, not that the pods reached the `Running`
phase. The controller holds the network off `Ready` until it reads a producing
signal, then reports `Ready`. The operator starts the load against a network that
runs.

**Why this priority**: an operator who starts load against a network that reports
`Ready` but produces no blocks measures a network that is not running. The operator
reads the result as a slow chain.

**Independent Test**: Bring up a network. Confirm that the phase stays off `Ready`
while the pods run but the chain height does not advance. Confirm that the phase
reaches `Ready` once the height advances.

**Acceptance Scenarios**:

1. **Given** a network whose validator pods reach the `Running` phase but whose chain height does not advance, **When** the controller computes the phase, **Then** the phase is not `Ready`.
2. **Given** the same network once its chain height advances, **When** the controller computes the phase, **Then** the phase is `Ready`.

---

### User Story 2 - The producing gate does not choke the network (Priority: P1)

An operator brings up a fresh network. The validators exchange peer traffic to
form consensus before any of them produces a block. The producing signal does not
sit on the pod readiness probe. A pod that reports not-ready receives no traffic,
and the network never forms. The controller reports producing on the network
status, and leaves the pod readiness probe to gate traffic on the pod's own
liveness.

**Why this priority**: this ranks with Story 1. A producing gate placed on the pod
readiness probe deadlocks the bring-up: no traffic, so no consensus, so no
production, so the network never reaches `Ready`. This story is a constraint on
Story 1 rather than a separable slice. It ships with Story 1.

**Independent Test**: Bring up a fresh network. Confirm that the validator pods
receive peer traffic before any validator produces a block. Confirm that the
producing gate is on the network status, not on the pod readiness probe.

**Acceptance Scenarios**:

1. **Given** a fresh network forming consensus, **When** a validator pod has produced no block yet, **Then** Kubernetes still routes peer traffic to the pod.
2. **Given** the network status, **When** the controller reports producing, **Then** the controller reports it as a status signal and does not hold the pod readiness probe not-ready.

---

### User Story 3 - The task sequence reads as a tick box (Priority: P2)

An operator watches a network come up. The plan runs an ordered sequence of tasks.
The operator wants to see which task the plan is on and which tasks are done, not
only that the plan is `Active`. The controller reports each task's state on the
plan, so the sequence reads as a tick box.

**Why this priority**: this ranks below the readiness stories. A plan that reports
only `Active` leaves the operator unable to tell a slow task from a stuck one.

**Independent Test**: Bring up a network. Read the plan on the status. Confirm that
each task carries a state, and that the running task and the done tasks are
distinguishable.

**Acceptance Scenarios**:

1. **Given** a plan part way through its tasks, **When** the operator reads the plan on the status, **Then** each task carries a state.
2. **Given** the same plan, **When** the operator reads it, **Then** the done tasks and the running task are distinguishable.

---

### User Story 4 - Each task records an event (Priority: P2)

An operator reviews why a bring-up took the time it took. The controller already
has each task's start, finish, and outcome. The operator wants an event per task,
not only the plan-level start and finish, so `kubectl describe` reads as a
timeline. The controller records an event as each task starts and ends.

**Why this priority**: this ranks with Story 3. A single plan-level event pair
hides where the time went.

**Independent Test**: Bring up a network. Read the events on the SeiNetwork.
Confirm that each task recorded a start event and an end event.

**Acceptance Scenarios**:

1. **Given** a completed plan, **When** the operator reads the SeiNetwork events, **Then** each task carries a start event and an end event.

### Edge Cases

- What happens when a validator pod is `Running` but its seid has stopped advancing the height? The network becomes not `Ready` — see Requirement 1.
- What happens when a task fails part way through the plan? The plan reports the task as failed, and the failed task is distinguishable from the done tasks — see Requirement 3.
- What happens when a producing signal is briefly unavailable during a routine restart? See Requirement 1, criterion 4.

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: Ready means producing

**Objective:** As an operator, I want `Ready` to mean the network produces blocks,
so that I start load against a network that runs.

**Traces to:** User Story 1

#### Acceptance Criteria

1. WHILE the validators produce no blocks for longer than any grace window, THE controller SHALL NOT report the network phase as `Ready`.
2. WHILE the child phases are `Running`, WHEN the controller reads a producing signal, THE controller SHALL report the network phase as `Ready`.
3. THE controller SHALL gate `Ready` on both the producing signal and the child `Running` phases.
4. WHEN a producing signal gap is brief, THE controller SHALL [NEEDS CLARIFICATION: drop the network off `Ready` at once, or hold `Ready` across a grace window? Owner: the platform team. Decide by: 2026-10-31.]

### Requirement 2: The producing gate stays off the pod readiness probe

**Objective:** As an operator, I want the producing gate on the status, so that the
network forms and my bring-up does not deadlock.

**Traces to:** User Story 2

#### Acceptance Criteria

1. THE controller SHALL report the producing signal on the SeiNetwork status.
2. THE controller SHALL NOT use the readiness probe of a validator pod as the producing gate.
3. WHILE a validator forms consensus and produces no block, THE controller SHALL leave the pod eligible for peer traffic.

### Requirement 3: The plan reports per-task progress

**Objective:** As an operator, I want each task's state on the plan, so that the
sequence reads as a tick box.

**Traces to:** User Story 3

#### Acceptance Criteria

1. THE controller SHALL report a state for each task on the plan.
2. THE controller SHALL make the running task distinguishable from the done tasks.
3. IF a task fails, THEN THE controller SHALL report that task as failed.
4. WHILE a plan holds a failed task, THE controller SHALL keep the done tasks distinguishable from it.

### Requirement 4: The controller records a per-task event

**Objective:** As an operator, I want an event per task, so that `kubectl describe`
reads as a timeline.

**Traces to:** User Story 4

#### Acceptance Criteria

1. WHEN a task starts, THE controller SHALL record a start event against the SeiNetwork.
2. WHEN a task ends, THE controller SHALL record an end event against the SeiNetwork.
3. THE end event SHALL name the task outcome.

### Key Entities

- **Producing signal**: see Glossary. The controller reads it to gate `Ready`.
- **Task progress**: see Glossary. It belongs to one task in the plan.

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: A network whose pods are `Running` but whose height does not advance stays not `Ready`.
  *Verifier:* judgement — a platform engineer holds a network's height flat and confirms the phase stays not `Ready`.
- **SC-002**: A network reaches `Ready` once its height advances.
  *Verifier:* judgement — a platform engineer confirms the phase reaches `Ready` after the height advances.
- **SC-003**: A forming network's validator pods receive peer traffic before any validator produces a block.
  *Verifier:* judgement — a platform engineer confirms a pre-production validator pod is a traffic target.
- **SC-004**: The producing gate is on the status, not on the pod readiness probe.
  *Verifier:* judgement — a platform engineer reads the pod readiness probe and confirms it does not encode the producing gate.
- **SC-005**: Each task on the plan carries a state, and the running task is distinguishable from the done tasks.
  *Verifier:* judgement — a platform engineer reads a mid-flight plan and confirms the per-task states.
- **SC-006**: A failed task is distinguishable from the done tasks on the plan.
  *Verifier:* judgement — a platform engineer fails a task and confirms the plan marks it failed.
- **SC-007**: Each task records a start event and an end event that names the task outcome.
  *Verifier:* judgement — a platform engineer reads the SeiNetwork events and confirms a start and a named end event per task.
- **SC-008**: A network with a producing signal but a child not in the `Running` phase stays not `Ready`.
  *Verifier:* judgement — a platform engineer holds one child out of `Running` and confirms the phase stays not `Ready`.

## Assumptions

- The SeiNetwork already computes a phase, seeds a set of always-present conditions, and records a plan-lifecycle event. This spec redefines the `Ready` phase and adds per-task progress and per-task events; it does not add the conditions framework.
- The `Running` phase of a child SeiNode means the pod and its sidecar are up, not that seid produces blocks. This is the gap the `Ready` redefinition closes.
- The controller can read a producing signal, such as an advancing chain height, from the validators. How it reads that signal is a plan concern.
- The plan already carries the ordered task list and the failed-task index. This spec adds a per-task state and a per-task event, which the plan does not yet surface.
- The producing signal is a status signal, separate from the pod readiness probe, because a not-ready pod receives no peer traffic and a forming network would deadlock.

## Out of scope

- The placement of a validator on a worker node, and the node-list report of it. The `node-ec2-locality` work item owns those status fields.
- The deletion and ownership status. The `crd-ownership-and-deletion` work item owns that.
- How a validator produces a block. seid owns consensus.
- A change to the existing condition set beyond the `Ready` meaning and the per-task additions.
