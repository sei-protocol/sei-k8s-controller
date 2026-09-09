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
- **Committed height**: the block height a validator has finalized. The node controller surfaces it on the SeiNode status, paired with the time it read it.
- **Producing signal**: the highest committed height any reachable validator reports. It advances when the current reading exceeds the observed height. A single lagging or restarting validator does not lower it.
- **Observed height**: the highest committed height the network controller has recorded, with the time it recorded it, on the SeiNetwork status. The stall detector compares the current reading against it, and the grace window runs from its time.
- **Producing condition**: the always-present condition that reports whether the network produces, carrying a reason from a named set.
- **Grace window**: the bounded time the network controller holds the phase at `Ready` after the observed height stops advancing.
- **Readiness probe**: the pod probe that gates whether Kubernetes routes traffic to the pod.
- **Plan**: the ordered task sequence the controller runs to bring the network or a node up.
- **Owning resource**: the SeiNode or SeiNetwork that runs a plan. Its task states and task events live on itself.
- **Task**: one step in a plan.
- **Task state**: the per-task value the plan already carries — `Pending`, `Running`, `Complete`, or `Failed`.
- **Event**: a Kubernetes Event the controller records against the owning resource.

## Boundary Context

- **Sits within**: the SeiNetwork phase and conditions, the observed height on the SeiNetwork status, the committed height on the SeiNode status, and the per-task state and events on any resource that runs a plan.
- **Owns**: the meaning of the `Ready` phase, the producing signal and its condition, the observed-height field on the SeiNetwork status, the committed-height field on the SeiNode status and the sidecar surface that feeds it, the assignment of the `Running` task state, and the per-task events on the owning resource.
- **Does not own**: the placement of a validator on a worker node, or the node-list report of it. The `node-ec2-locality` work item owns those status fields.
- **Does not own**: the deletion and ownership status. The `crd-ownership-and-deletion` work item owns that.
- **Does not own**: how a validator commits a block. seid owns consensus.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - Ready means the network produces blocks (Priority: P1)

An operator waits for a benchmark network to come up. The operator wants `Ready`
to mean the network commits new blocks, not that the pods reached the `Running`
phase. The controller holds the network off `Ready` until the observed height
advances, then reports `Ready`. A single validator restart does not drop `Ready`,
because the highest committed height keeps advancing. A network that produced and
then stalled reports `Degraded`, not `Initializing`.

**Why this priority**: an operator who starts load against a network that reports
`Ready` but commits no block measures a network that is not running, and reads the
result as a slow chain.

**Independent Test**: Bring up a network. Confirm that the phase stays off `Ready`
while the pods run but the observed height does not advance. Confirm that the phase
reaches `Ready` once the height advances. Restart one validator and confirm the
phase stays `Ready`.

**Acceptance Scenarios**:

1. **Given** a network whose validator pods are `Running` but whose observed height does not advance, **When** the network controller computes the phase, **Then** the phase is not `Ready`.
2. **Given** the same network once its observed height advances, **When** the network controller computes the phase, **Then** the phase is `Ready`.
3. **Given** a `Ready` network, **When** one validator restarts while the highest committed height keeps advancing, **Then** the phase stays `Ready`.
4. **Given** a network that reached `Ready` and then stopped committing past the grace window, **When** the network controller computes the phase, **Then** the phase is `Degraded`.

---

### User Story 2 - The producing signal is real and off the pod readiness probe (Priority: P1)

An operator brings up a fresh network. The producing signal must reflect the
committed height advancing, not that an endpoint responds, because seid answers
its status during initial sync and at a pinned freeze height without producing.
The signal must not sit on the pod readiness probe, because a pod that reports
not-ready receives no traffic, and the network never forms. The sidecar reports
the committed height, the node controller surfaces it with a read time on the
SeiNode status, the network controller records the observed height, and the pod
readiness probe still gates traffic on the pod's own liveness.

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
- What happens when the network controller cannot read the committed height at all? The observed height stops advancing, so the grace window governs — a within-window gap holds `Ready`, a longer gap becomes `Degraded` — see Requirement 1.
- What happens when one child of many has a stale read time? The network controller excludes that child, so a stale minority does not read as a stall and the phase stays `Ready` from the fresh children — see Requirement 2.
- What happens when the node controller stops reconciling and freezes every child's read time? The whole signal is unreadable, so the phase becomes `Degraded` past the grace window and the `Producing` reason is signal-unreadable, which the runbook reads as a controller outage, not a chain stall — see Requirement 1 and Requirement 2.
- What happens when a produced network stops committing past the grace window? The phase is `Degraded`, and the `Producing` condition reason distinguishes it from a `Degraded` caused by a failed child — see Requirement 1 and Requirement 2.
- What happens when a task fails part way through a plan? The plan keeps the task at `Failed`, distinguishable from the done tasks — see Requirement 3.

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: Ready means the network commits blocks

**Objective:** As an operator, I want `Ready` to mean the network commits new
blocks, so that I start load against a network that runs.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE producing signal SHALL be the highest committed height any reachable validator reports.
2. WHEN the producing signal exceeds the observed height, THE network controller SHALL record the new height and the observation time as the observed height on the SeiNetwork status.
3. WHILE no higher-precedence phase applies, AND the children are `Running`, AND the observed height advanced within the grace window, THE network controller SHALL report the network phase as `Ready`.
4. WHILE the children are `Running` and the observed height has not advanced since the network first recorded one, THE network controller SHALL report the network phase as `Initializing`.
5. WHILE the children are `Running` and the observed height advanced at least once before but not within the grace window, THE network controller SHALL report the network phase as `Degraded`.
6. IF the network controller cannot read the committed height, THEN THE network controller SHALL leave the observed height unchanged, so the grace window measured from its observation time governs the outcome.
7. THE network controller SHALL measure the grace window from the observed height's observation time.

### Requirement 2: The producing signal is real, and off the pod readiness probe

**Objective:** As an operator, I want the producing signal to reflect real block
production and to stay off the pod readiness probe, so that the gate is trustworthy
and a forming network still receives traffic.

**Traces to:** User Story 2

#### Acceptance Criteria

1. THE producing signal SHALL reflect the committed height advancing, not that an endpoint responds.
2. THE sidecar SHALL report the committed height on its status surface, as an optional field whose absence means the height is unreadable, not zero.
3. THE node controller SHALL read the committed height from the sidecar and surface it, with the time it read it, on the SeiNode status.
4. THE network controller SHALL take the producing signal from the committed heights on the child SeiNode statuses, and SHALL exclude a child whose read time is stale, so a stale minority does not read as a stall.
5. WHILE every child read time is stale, THE network controller SHALL set the `Producing` reason to signal unreadable, and let the grace window drive the phase.
6. THE network controller SHALL report the `Producing` condition, always present, carrying a reason from the set: advancing, awaiting the first block, stalled, and signal unreadable, with the observed generation.
7. THE controller SHALL NOT use the readiness probe of a validator pod as the producing gate.
8. WHILE a validator forms consensus and commits no block, THE controller SHALL leave the pod eligible for peer traffic.

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

- **Observed height**: see Glossary. The network controller records it on the SeiNetwork status and runs the grace window from its time.
- **Committed height**: see Glossary. The node controller surfaces it on the SeiNode status with its read time.
- **Task state**: see Glossary. It belongs to one task in one plan, on the owning resource.

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: A network whose pods are `Running` but whose observed height does not advance is not `Ready`.
  *Verifier:* judgement — a platform engineer holds a network's height flat and confirms the phase is not `Ready`, exercising `computeGroupPhase` with an injected clock in `internal/controller/seinetwork/status_test.go`.
- **SC-002**: A network reaches `Ready` once its observed height advances.
  *Verifier:* judgement — a platform engineer confirms the phase reaches `Ready` after the observed height advances, exercising `computeGroupPhase` with an injected clock in `internal/controller/seinetwork/status_test.go`.
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
- **SC-011**: The `Producing` condition is always present and reads `False` with a reason from the named set when the network does not produce.
  *Verifier:* judgement — a platform engineer reads the condition on a not-producing network and confirms it is present, `False`, and carries one of the named reasons.
- **SC-012**: A `Degraded` from a stalled chain is distinguishable from a `Degraded` from a failed child.
  *Verifier:* judgement — a platform engineer reads the `Producing` condition reason on a `Degraded` network and confirms it names the stall.
- **SC-013**: A stale minority of children does not drop the network off `Ready`, and a wholly unreadable signal surfaces the signal-unreadable reason while the phase still goes `Degraded` past the window.
  *Verifier:* judgement — a platform engineer makes one child of many stale and confirms the phase stays `Ready` from the fresh children, then stops the node controller and confirms the `Producing` reason is signal-unreadable and the phase becomes `Degraded` past the window.

## Assumptions

- The plan already carries a per-task state enum — `Pending`, `Running`, `Complete`, `Failed` — and the executor sets `Pending`, `Complete`, and `Failed`. Only `Running` is never assigned, so the running task is inferred from the first task that is not `Complete`. This spec assigns `Running`; it does not add a state field.
- The producing signal is the highest committed height across the reachable validators. Committed height is the height the validators agree on, so the maximum any reachable validator reports is the network height, and a lagging or restarting validator only reports a lower value that the maximum ignores. No quorum count is needed; the maximum is the cheapest correct read.
- The observed height — a height plus its observation time — lives on the SeiNetwork status, not on the `Producing` condition. `SetStatusCondition` moves a condition's transition time only when the status changes, so it cannot record "when the height last advanced". The stall detector needs the prior height and its time to notice that the height stopped, and that state must survive a controller restart or a leader change, so it belongs on the status.
- The `Producing` condition status flips to `False` when the observed height has not advanced within the grace window. Its reason distinguishes four cases, and the reason strings are the public API for runbooks and alerts: `HeightAdvancing`, `AwaitingFirstBlock`, `HeightStalled`, and `SignalUnreadable`. `HeightStalled` on a `Degraded` network means the chain wedged, which a `Degraded` caused by a failed child does not carry.
- The grace window is a multiple of the status poll interval, which is about 30 seconds, not a multiple of the block time. An observed height is up to about 60 seconds stale end to end across the node and network reconciles, so a block-time window would flap `Ready` and `Degraded` on sampling jitter with nothing wrong on the chain. The window is configurable.
- The committed height on the SeiNode status carries the time the node controller read it. The network controller excludes a child whose read time is older than a staleness bound, so a stale minority does not read as a stall. When every child is stale — a node controller that stopped reconciling freezes them all at once — the whole signal is unreadable: the observed height stops advancing, the grace window drives the phase to `Degraded`, and the `Producing` reason is `SignalUnreadable`, so the runbook reads a controller outage rather than a chain stall. Both an unreadable signal and a real stall reach `Degraded`, which keeps the bound; the reason tells them apart.
- The network distinguishes a network that has not yet advanced (`Initializing`) from one that advanced and then stalled (`Degraded`) from the observed-height history — whether the observed height has risen above the value the network first recorded — not from the genesis height, which is on no status. This reads correctly for a network that starts at its genesis height and for one restored from a snapshot or a state sync, whose first reading already exceeds genesis. The `Producing` reason carries this history across a controller restart.
- The observed height need not capture every advance. The grace window only needs it fresh within the window, so the network controller coalesces updates: a producing network records the observed height at least once well within each grace window rather than on every reconcile. This avoids a guaranteed per-poll status write that would bump the resource version every cycle and raise the optimistic-lock conflict rate.
- The sidecar height field is optional, and its absence means unreadable, not zero. A controller reading an older sidecar image sees the field absent and treats the height as unreadable, so an image that predates the field does not read as a fleet-wide stall of zero. The ordered deploy puts the sidecar image before the controller, which keeps the normal direction safe.
- The sidecar's status endpoint returns only a two-value readiness enum today, so the committed height does not cross the sidecar contract. Carrying it extends the sidecar API. This is in-scope cross-module work, and it follows the controller's ordered sidecar-image deploy and its rollback discipline; the contract module stays free of the chain graph.
- The node controller already holds a sidecar client, so it reads the committed height and surfaces it on the SeiNode status. The network controller reads the child SeiNode statuses it already consumes, so it gains no new sidecar client and no per-poll fan-out to child sidecars.
- The producing signal reflects the committed height advancing, not that an endpoint responds. seid answers its status endpoint during initial block sync and at a pinned freeze height, so a reachability check would report `Ready` on a node that is not producing.
- `computeGroupPhase` gains the current time and the observed height and its time as inputs, so the phase test injects a clock to verify the readiness criteria.
- The `Ready` phase defers to the existing phase precedence. `Paused`, `PlanInProgress`, and a failed ceremony resolve before the producing gate, so Requirement 1, criterion 3 does not override them.
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
