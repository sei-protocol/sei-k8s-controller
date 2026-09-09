# Feature Specification: Ready means the network produces blocks, and the plan marks the running task

**Feature Branch**: `007-crd-status-conditions-events`

**Created**: 2026-09-09

**Status**: Draft

**Unblocks**: a benchmark an operator can trust to have started. This spec makes
the `Ready` phase mean the validators commit new blocks, and makes the plan mark
which task is running. The SeiNetwork already carries a conditions framework, a
phase, and plan events, and the plan already carries a per-task state. Two gaps
remain. The network reports `Ready` when its pods reach the `Running` phase, which
is not the same as the network committing blocks, so an operator starts load
against a network that does not yet run. And the plan never sets a task to
`Running`, so an operator cannot tell which task the plan is on without inferring
it from the first task that is not `Complete`.

**Input**: the Benchmark Party transcript, 2026-09-04, and review on pull request
525. The fence below holds the originator's words. The writing rules govern this
document, not its source.

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
records a plan-lifecycle event, and stores a per-task state on the plan. This spec
does not rebuild that framework. It corrects what `Ready` means, adds a producing
signal that reliably reflects block production, marks the running task, and records
a per-task event on the resource that owns the plan.

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach, because that gap is the honest part.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | the status, condition, and event surface | whether the controller reconciles correctly |
| controller-runtime | the reconcile that writes the status | idempotence of a specific reconcile |

## Glossary

- **Controller**: the sei-k8s-controller reconciler. The network controller reconciles a SeiNetwork; the node controller reconciles a SeiNode.
- **Operator**: a person who runs a benchmark and reads the status.
- **SeiNetwork**: the CRD that owns a pool of validators.
- **Validator**: a child SeiNode of the SeiNetwork.
- **Running phase**: the child SeiNode phase that means the pod and its sidecar are up.
- **Sidecar**: the container beside seid that reads seid's co-located CometBFT RPC.
- **Committed height**: the block height a validator has finalized. The node controller surfaces it on the SeiNode status.
- **Producing signal**: the highest committed height any reachable validator reports, advancing over time. A single lagging or restarting validator does not lower it.
- **Producing condition**: the always-present condition that reports whether the network produces, and carries the time production last advanced.
- **Grace window**: the bounded time the network controller holds the phase at `Ready` after the producing signal goes absent.
- **Readiness probe**: the pod probe that gates whether Kubernetes routes traffic to the pod.
- **Plan**: the ordered task sequence the controller runs to bring the network or a node up.
- **Owning resource**: the SeiNode or SeiNetwork that runs a plan. Its task states and task events live on itself.
- **Task**: one step in a plan.
- **Task state**: the per-task value the plan already carries — `Pending`, `Running`, `Complete`, or `Failed`.
- **Event**: a Kubernetes Event the controller records against the owning resource.

## Boundary Context

- **Sits within**: the SeiNetwork phase and conditions, the committed height on the SeiNode status, and the per-task state and events on any resource that runs a plan.
- **Owns**: the meaning of the `Ready` phase, the producing signal and its condition, the committed-height field on the SeiNode status and the sidecar surface that feeds it, the assignment of the `Running` task state, and the per-task events on the owning resource.
- **Does not own**: the placement of a validator on a worker node, or the node-list report of it. The `node-ec2-locality` work item owns those status fields.
- **Does not own**: the deletion and ownership status. The `crd-ownership-and-deletion` work item owns that.
- **Does not own**: how a validator commits a block. seid owns consensus.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - Ready means the network produces blocks (Priority: P1)

An operator waits for a benchmark network to come up. The operator wants `Ready`
to mean the network commits new blocks, not that the pods reached the `Running`
phase. The controller holds the network off `Ready` until the committed height
advances, then reports `Ready`. A single validator restart does not drop `Ready`,
because the highest committed height keeps advancing. A network that produced and
then stalled reports `Degraded`, not `Initializing`.

**Why this priority**: an operator who starts load against a network that reports
`Ready` but commits no block measures a network that is not running, and reads the
result as a slow chain.

**Independent Test**: Bring up a network. Confirm that the phase stays off `Ready`
while the pods run but the committed height does not advance. Confirm that the
phase reaches `Ready` once the height advances. Restart one validator and confirm
the phase stays `Ready`.

**Acceptance Scenarios**:

1. **Given** a network whose validator pods are `Running` but whose committed height does not advance, **When** the network controller computes the phase, **Then** the phase is not `Ready`.
2. **Given** the same network once its committed height advances, **When** the network controller computes the phase, **Then** the phase is `Ready`.
3. **Given** a `Ready` network, **When** one validator restarts while the highest committed height keeps advancing, **Then** the phase stays `Ready`.
4. **Given** a network that reached `Ready` and then stopped committing past the grace window, **When** the network controller computes the phase, **Then** the phase is `Degraded`.

---

### User Story 2 - The producing signal is real and off the pod readiness probe (Priority: P1)

An operator brings up a fresh network. The producing signal must reflect the
committed height advancing, not that an endpoint responds, because seid answers
its status during initial sync and at a pinned freeze height without producing.
The signal must not sit on the pod readiness probe, because a pod that reports
not-ready receives no traffic, and the network never forms. The sidecar reports
the committed height, the node controller surfaces it on the SeiNode status, the
network controller reads the child statuses, and the pod readiness probe still
gates traffic on the pod's own liveness.

**Why this priority**: this ranks with Story 1. A gate on the pod readiness probe
deadlocks the bring-up: no traffic, so no consensus, so no production, so the
network never reaches `Ready`. A gate on "the endpoint responded" reports `Ready`
while the network is still catching up. This story is a constraint on Story 1
rather than a separable slice. It ships with Story 1.

**Independent Test**: Bring up a fresh network. Confirm that the validator pods
receive peer traffic before any validator commits a block. Confirm that a
catching-up validator whose status endpoint answers does not read `Ready`.

**Acceptance Scenarios**:

1. **Given** a fresh network forming consensus, **When** a validator pod has committed no block yet, **Then** Kubernetes still routes peer traffic to the pod.
2. **Given** a validator whose status endpoint answers during initial sync, **When** the network controller computes the phase, **Then** the phase is not `Ready`.

---

### User Story 3 - The plan marks the running task (Priority: P2)

An operator watches a resource come up. The plan already records each task as
`Pending`, `Complete`, or `Failed`, but never as `Running`, so the operator infers
the running task from the first task that is not `Complete`. The operator wants the
running task marked `Running`, so the sequence reads as a tick box directly.

**Why this priority**: this ranks below the readiness stories. An inferred running
task leaves the operator unable to tell a slow task from a stuck one when a retry
resets a later task to `Pending`.

**Independent Test**: Bring up a resource. Read its plan. Confirm that the task in
flight carries the `Running` state, and that the done and failed tasks stay
distinguishable.

**Acceptance Scenarios**:

1. **Given** a plan with a task in flight, **When** the operator reads the plan, **Then** the in-flight task carries the `Running` state.
2. **Given** the same plan, **When** the operator reads it, **Then** the `Complete` and `Failed` tasks stay distinguishable from the `Running` task.

---

### User Story 4 - Each task records an event on the resource that runs it (Priority: P2)

An operator reviews why a bring-up took the time it took. The controller already
has each task's start, finish, and outcome. The operator wants an event per task
on the resource that runs the plan — a SeiNode's task events on the SeiNode, the
ceremony's task events on the SeiNetwork — so `kubectl describe` on that resource
reads as a timeline.

**Why this priority**: this ranks with Story 3. A single plan-level event pair
hides where the time went.

**Independent Test**: Bring up a network. Read the events on each SeiNode and on
the SeiNetwork. Confirm that each task recorded a start event and an end event on
the resource that ran it.

**Acceptance Scenarios**:

1. **Given** a completed node plan, **When** the operator reads the SeiNode events, **Then** each of that plan's tasks carries a start event and an end event.

### Edge Cases

- What happens when one validator lags or restarts while the highest committed height keeps advancing? The phase stays `Ready` — see Requirement 1.
- What happens when the network controller cannot read the committed height at all? The signal counts as absent from that moment, and the grace window governs — a within-window gap holds `Ready`, a longer gap becomes `Degraded` — see Requirement 1.
- What happens when a produced network stops committing past the grace window? The phase is `Degraded`, and the `Producing` condition distinguishes it from a `Degraded` caused by a failed child — see Requirement 1 and Requirement 2.
- What happens when a task fails part way through a plan? The plan keeps the task at `Failed`, distinguishable from the done tasks — see Requirement 3.

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: Ready means the network commits blocks

**Objective:** As an operator, I want `Ready` to mean the network commits new
blocks, so that I start load against a network that runs.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE producing signal SHALL be the highest committed height any reachable validator reports, advancing over time.
2. WHILE no higher-precedence phase applies, AND the children are `Running`, AND the producing signal advances, THE network controller SHALL report the network phase as `Ready`.
3. WHILE the children are `Running` and the network has committed no block yet, THE network controller SHALL report the network phase as `Initializing`.
4. WHILE the children are `Running` and the network committed before but the producing signal has been absent longer than the grace window, THE network controller SHALL report the network phase as `Degraded`.
5. WHILE the producing signal has been absent for less than the grace window and the network was `Ready`, THE network controller SHALL keep the network phase at `Ready`.
6. IF the network controller cannot read the producing signal, THEN THE network controller SHALL treat the signal as absent from that time, so the grace window governs the outcome.
7. THE network controller SHALL derive the grace window from the time the producing signal last advanced, recorded on the `Producing` condition.

### Requirement 2: The producing signal is real, and off the pod readiness probe

**Objective:** As an operator, I want the producing signal to reflect real block
production and to stay off the pod readiness probe, so that the gate is trustworthy
and a forming network still receives traffic.

**Traces to:** User Story 2

#### Acceptance Criteria

1. THE producing signal SHALL reflect the committed height advancing, not that an endpoint responds.
2. THE sidecar SHALL report the committed height on its status surface.
3. THE node controller SHALL read the committed height from the sidecar and surface it on the SeiNode status.
4. THE network controller SHALL derive the producing signal from the committed heights on the child SeiNode statuses.
5. THE network controller SHALL report the producing signal as an always-present `Producing` condition, set to `False` with a stable reason when the network does not produce, and carrying the observed generation.
6. THE controller SHALL NOT use the readiness probe of a validator pod as the producing gate.
7. WHILE a validator forms consensus and commits no block, THE controller SHALL leave the pod eligible for peer traffic.

### Requirement 3: The plan marks the running task

**Objective:** As an operator, I want the running task marked `Running`, so that
the tick box reads directly and not by inference.

**Traces to:** User Story 3

#### Acceptance Criteria

1. WHEN a task begins executing, THE controller SHALL set that task's state to `Running`.
2. WHILE a task is `Running`, THE controller SHALL keep the done tasks at `Complete` and a failed task at `Failed`.
3. IF a task fails, THEN THE controller SHALL keep that task at `Failed`, distinguishable from the done tasks.

### Requirement 4: Each task records an event on the resource that owns the plan

**Objective:** As an operator, I want a per-task event on the resource that runs
the plan, so that `kubectl describe` on that resource reads as a timeline.

**Traces to:** User Story 4

#### Acceptance Criteria

1. WHEN a task starts, THE controller SHALL record a start event against the resource that owns the plan.
2. WHEN a task ends, THE controller SHALL record an end event against the resource that owns the plan.
3. THE end event SHALL name the task outcome.

### Key Entities

- **Producing signal**: see Glossary. The network controller reads it to gate `Ready`.
- **Committed height**: see Glossary. The node controller surfaces it on the SeiNode status.
- **Task state**: see Glossary. It belongs to one task in one plan, on the owning resource.

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: A network whose pods are `Running` but whose committed height does not advance is not `Ready`.
  *Verifier:* judgement — a platform engineer holds a network's height flat and confirms the phase is not `Ready`, exercising `computeGroupPhase` in `internal/controller/seinetwork/status_test.go`.
- **SC-002**: A network reaches `Ready` once its committed height advances.
  *Verifier:* judgement — a platform engineer confirms the phase reaches `Ready` after the height advances, exercising `computeGroupPhase` in `internal/controller/seinetwork/status_test.go`.
- **SC-003**: A single validator restart does not drop the network off `Ready` while the highest committed height keeps advancing.
  *Verifier:* judgement — a platform engineer restarts one validator on a `Ready` network and confirms the phase stays `Ready`.
- **SC-004**: A produced network that stops committing past the grace window reports `Degraded`, not `Initializing`.
  *Verifier:* judgement — a platform engineer stalls a produced network and confirms the phase becomes `Degraded`.
- **SC-005**: A committed-height read failure holds `Ready` within the grace window and becomes `Degraded` past it.
  *Verifier:* judgement — a platform engineer makes the height unreadable and confirms the phase holds at `Ready` within the window, then becomes `Degraded` past it.
- **SC-006**: A catching-up validator whose status endpoint answers does not read `Ready`.
  *Verifier:* judgement — a platform engineer confirms a syncing validator whose endpoint answers is not `Ready`.
- **SC-007**: A forming network's validator pods receive peer traffic before any validator commits a block, and the producing gate is not on the pod readiness probe.
  *Verifier:* judgement — a platform engineer confirms a pre-production validator pod is a traffic target and the readiness probe does not encode the producing gate.
- **SC-008**: The in-flight task on a plan carries the `Running` state.
  *Verifier:* judgement — a platform engineer reads a mid-flight plan and confirms the in-flight task is `Running`, not inferred.
- **SC-009**: A failed task stays `Failed` and the done tasks stay `Complete`.
  *Verifier:* judgement — a platform engineer fails a task and confirms the plan keeps the states distinguishable.
- **SC-010**: Each task records a start event and an end event that names the outcome, on the resource that ran it.
  *Verifier:* judgement — a platform engineer reads the events on a SeiNode and on the SeiNetwork and confirms a start and a named end event per task.
- **SC-011**: The `Producing` condition is always present and reads `False` with a stable reason when the network does not produce.
  *Verifier:* judgement — a platform engineer reads the condition on a not-producing network and confirms it is present and `False` with a stable reason.
- **SC-012**: A `Degraded` from a stalled chain is distinguishable from a `Degraded` from a failed child.
  *Verifier:* judgement — a platform engineer reads the `Producing` condition on a `Degraded` network and confirms it names the cause.

## Assumptions

- The plan already carries a per-task state enum — `Pending`, `Running`, `Complete`, `Failed` — and the executor sets `Pending`, `Complete`, and `Failed`. Only `Running` is never assigned, so the running task is inferred from the first task that is not `Complete`. This spec assigns `Running`; it does not add a state field.
- The producing signal is the highest committed height across the reachable validators. Committed height is the height the validators agree on, so the maximum any reachable validator reports is the network height, and a lagging or restarting validator only reports a lower value that the maximum ignores. No quorum count is needed; the maximum is the cheapest correct read.
- The sidecar's status endpoint returns only a two-value readiness enum today, so the committed height does not cross the sidecar contract. Carrying it requires extending the sidecar API. This is in-scope cross-module work, and it follows the controller's ordered sidecar-image deploy and its rollback discipline; the contract module stays free of the chain graph.
- The node controller already holds a sidecar client, so it reads the committed height and surfaces it on the SeiNode status. The network controller reads the child SeiNode statuses it already consumes, so it gains no new sidecar client and no per-poll fan-out to child sidecars.
- The producing signal reflects the committed height advancing, not that an endpoint responds. seid answers its status endpoint during initial block sync and at a pinned freeze height, so a reachability check would report `Ready` on a node that is not producing.
- A committed-height read failure is not a production stall on its own. It counts as an absent signal from the time of the failure, so the grace window governs: a within-window gap holds `Ready`, and a gap past the window becomes `Degraded`. This bounds the hold, so a permanently lost read path cannot hold `Ready` forever.
- The grace window is bounded and configurable, with a default on the order of a small multiple of the block time. Requirement 1, criteria 4, 5, and 6 turn on it.
- The network controller derives the grace window from the `Producing` condition's last-transition time, which is where the "when did production last advance" state lives across reconciles. `computeGroupPhase` gains that time as an input.
- `Degraded` now has two causes: a child in the `PhaseFailed` state, and a stalled chain with the children `Running`. The `Producing` condition disambiguates them for the alerting consumer: `Producing` set to `False` with a stalled reason means the chain wedged, and a node did not fail. The phase gauge alert keys on the condition to tell the two apart.
- The `Ready` phase defers to the existing phase precedence. `Paused`, `PlanInProgress`, and a failed ceremony resolve before the producing gate, so Requirement 1, criterion 2 does not override them.
- `Ready` is no longer monotonic: a network can leave `Ready`. In-repo consumers read the current meaning — the `WaitReady` helper and the phase gauge — so leaving `Ready` is now an expected transition, not a fault.
- The per-task event is recorded on the resource that owns the plan. A SeiNode's task events land on the SeiNode; the genesis-ceremony task events land on the SeiNetwork. Requirement 3 and Requirement 4 share this subject.
- Kubernetes applies its default event retention, so a bring-up older than the retention window will not show its task events, and the per-task event volume scales with the validator count times the task count. Tuning event retention is out of scope.
- The `Producing` condition follows the controller's condition discipline: always present, `False` with a stable reason rather than absence, and the observed generation at every write.

## Out of scope

- The placement of a validator on a worker node, and the node-list report of it. The `node-ec2-locality` work item owns those status fields.
- The deletion and ownership status. The `crd-ownership-and-deletion` work item owns that.
- How a validator commits a block. seid owns consensus.
- A change to the existing conditions beyond the new `Producing` condition and the `Ready`-phase meaning.
- The tuning of Kubernetes event retention.
- The cadence and the timeout of the committed-height read. Those are plan concerns.
