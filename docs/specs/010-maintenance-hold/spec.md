# Feature Specification: Maintenance hold for ConfigMap-configured nodes

**Feature Branch**: `010-maintenance-hold`

**Created**: 2026-10-06

**Status**: Draft

**Tracking**: PLT-1390 (parent PLT-1389, atlantic-2 migration)

**Unblocks**: `seid rollback`, chain-halt diagnostics, and the coordinated chain
recovery in the FlatKV migration runbook, on a SeiNode with `spec.nodeConfig`.
Those operations need seid stopped, the pod alive, and the data volume mounted.
Today no durable way to reach that state exists. If an operator kills seid by
hand, the container restarts it, because the start gate is open. A pod restart
reopens the gate, because the controller re-marks the sidecar ready when it sees
`SidecarReady=False`. `spec.paused` scales the StatefulSet to zero replicas, so the
pod and the volume mount go away.

**Input**: PLT-1389 and PLT-1390, the readiness review, and the FlatKV migration
runbook (`sei-protocol/runbooks`, `flatkv-migration-runbook.md`). The runbook
asks for two things the cluster cannot do today: "keep a Kubernetes pod stopped
after `seid` exits at `halt-height`", and stop a validator, roll it back, and
start it again on a new binary.

This spec adds one spec field, a hold. While the hold is set, the controller
keeps the start gate closed on the node's pod and on every replacement pod. The
operator uses `kubectl exec -c seid` into the parked container to run tools
against the data. The design needs no SSH and no new command API.

## Semantic Anchors

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| Kubernetes API conventions (`spec.paused`, `spec.suspend`) | the hold as durable desired state in the spec | what an operator does while the node is held |
| CometBFT `priv_validator_state.json` | why a rollback under a hold is safe for a validator | an operator who deletes the file by hand |

## Glossary

- **Controller**: the sei-k8s-controller SeiNode reconciler.
- **Operator**: a person who runs a node and merges the GitOps commit that changes it.
- **ConfigMap-configured node**: a SeiNode with `spec.nodeConfig` set.
- **Hold**: the new `spec.maintenance.hold` field. Its value is `Immediate` or `AfterExit`. An absent value means no hold.
- **Immediate hold**: closes the start gate and stops seid now.
- **AfterExit hold**: closes the start gate and leaves seid running. When seid exits on its own, for example at `halt-height`, the container restarts and parks at the gate.
- **Hold in effect**: the new `status.maintenanceHold` field. `Immediate` means seid is parked by the hold. `AfterExit` means the gate is closed and seid may still run. Empty means no hold acts on the node. The controller compares it with the spec, the same way spec 009 compares its two counters.
- **Start once**: the step that starts seid on a parked node and closes the gate again. The controller marks the sidecar ready, waits until the `seid start` process runs, and then runs `mark-not-ready`.
- **Release**: removing the hold. The controller then marks the sidecar ready, and seid starts on whatever data the volume holds.
- **Start gate**: the sidecar readiness flag that holds seid from starting until the sidecar receives `mark-ready`.
- **Start guard**: the controller rule from spec 009 that decides whether `mark-ready` may reach the sidecar. This spec adds "no hold is set" to it.
- **Parked**: the seid container runs its wait loop and seid does not run.

## Boundary Context

- **Sits within**: the SeiNode spec and status, the Running-phase plans of a ConfigMap-configured node, and the start guard.
- **Owns**: the hold field, the `MaintenanceInProgress` condition, the hold and release plans, and the effect of the hold on every other plan.
- **Does not own**: the start guard's reset rule and the reset plan. Spec 009 owns them. This spec states how they behave while held.
- **Does not own**: who may run `kubectl exec` in production. The platform repository owns that RBAC.
- **Does not own**: the tools an operator runs while the node is held. The seid image and the runbooks own them.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Roll a node back with seid stopped (Priority: P1)

An operator sets `spec.maintenance.hold: Immediate` in git. The controller stops
seid and keeps the pod alive. The operator runs `seid rollback` through
`kubectl exec -c seid`, then removes the hold. seid starts on the rolled-back data.

**Why this priority**: A2 and A7 in the FlatKV runbook need it, and no other path
exists.

**Independent Test**: hold a running harbor node, run `seid rollback -n 1` through
exec, release, and confirm the node starts at the lower height and catches up.

**Acceptance Scenarios**:

1. **Given** a running node, **When** the operator sets an `Immediate` hold, **Then** seid stops, the pod stays up, and the condition reads `True/Held`.
2. **Given** a held node, **When** the operator removes the hold, **Then** the controller marks the sidecar ready and seid starts.

---

### User Story 2 - Keep a node stopped after `halt-height` (Priority: P1)

An operator sets `spec.maintenance.hold: AfterExit` before the halt height. seid
keeps running until it exits at the halt height. The container restarts and parks,
so the node commits no further block.

**Why this priority**: the coordinated chain recovery depends on it. Without it, the
container restarts seid, which commits one more block and can move the chain past
the verification height.

**Independent Test**: on a harbor chain, set `halt-height` to H in the node's
ConfigMap, set an `AfterExit` hold, and confirm seid exits at H and does not start
again.

**Acceptance Scenarios**:

1. **Given** a running node, **When** the operator sets an `AfterExit` hold, **Then** seid keeps running and the condition reads `True/Armed`.
2. **Given** an `AfterExit` hold, **When** seid exits, **Then** the container parks at the start gate and seid does not start.
3. **Given** a node parked by an `Immediate` hold, **When** the operator changes the hold to `AfterExit`, **Then** seid starts once and parks at its next exit.

---

### User Story 3 - The hold survives restarts (Priority: P1)

A held node stays held when its pod is deleted, when it rolls onto a new template,
and when the controller restarts.

**Why this priority**: a hold that a restart can release is not a hold.

**Independent Test**: hold a node, delete its pod, and restart the controller.
Confirm seid stays parked.

**Acceptance Scenarios**:

1. **Given** a held node, **When** the pod is deleted, **Then** the new pod stays parked.
2. **Given** a held node, **When** the controller restarts, **Then** the node stays held.
3. **Given** a held node, **When** the operator merges a new image, **Then** the pod rolls onto the new image and stays parked.

---

### User Story 4 - Reset a held node (Priority: P2)

An operator merges a reset commit while the node is held with `Immediate`. The
controller resets the data and leaves the node held. The operator can inspect the
node before release. Under `AfterExit`, the reset also leaves seid parked, and the
controller then starts it once, because `AfterExit` means seid runs until it
exits.

**Why this priority**: A3 in the runbook saves the hash log before the reset, so
the operator holds the node, copies the files, then resets.

**Independent Test**: hold a node, merge a reset commit, and confirm the handled
counter advances and seid stays parked.

**Acceptance Scenarios**:

1. **Given** a node held with `Immediate`, **When** a reset becomes pending, **Then** the controller runs the reset and the node stays held.

---

### User Story 5 - Create a node parked (Priority: P2)

An operator creates a SeiNode with an `Immediate` hold. The node initializes and
stops at the start gate before seid first runs. The operator inspects the data,
then releases. A node created with `AfterExit` also initializes parked, and the
controller then starts it once.

**Why this priority**: the EC2 cutover checks a validator's imported data and sign
state before the validator signs for the first time.

**Independent Test**: create a harbor node with an `Immediate` hold, and confirm it
reaches `Running` with seid parked.

**Acceptance Scenarios**:

1. **Given** a new SeiNode with an `Immediate` hold, **When** its init plan completes, **Then** seid has not started and the condition reads `True/Held`.

### Edge Cases

- A hold set after the node's init plan was built does not stop the init plan from starting seid: the hold acts on the start guard only once the node is `Running`, because an init plan failure is terminal. The hold plan then stops seid.
- Under an `AfterExit` hold in effect, a pod roll parks seid as an exit does: a template change, a pod delete, an eviction, or the startup-probe restart after about five days. The condition stays `Armed`, and nothing starts seid again. To run a new template up to a halt height, hold with `Immediate` first, then change to `AfterExit` in the commit that changes the template; start-once then runs on the new pod.
- The operator changes the hold back to `Immediate` while a start-once plan runs: the start-once step refuses, and seid does not start.
- A start-once plan fails after it opened the gate: the sidecar then reports the gate open under the hold, and the controller closes it again.
- The pod rolls after `start-seid-once` and before seid starts: the new pod's gate is closed, so seid does not start. `await-seid-start` fails after two minutes, and the controller builds the start-once plan again against the new pod.
- The operator changes `AfterExit` to `Immediate`: the controller stops seid now.
- The operator changes `Immediate` to `AfterExit` while seid is parked: seid starts once. The gate closes again a few seconds after `seid start` runs. On atlantic-2 and pacific-1, seid needs minutes to load its state before it can commit a block, so it parks at its next exit. A small harbor chain loads faster, so a rehearsal there can see seid commit before the gate closes.
- A coordinated recovery changes the image, sets `halt-height` in a new ConfigMap, and changes the hold to `AfterExit`, in one commit per validator. The pod rolls onto the new template, starts once, and parks when seid exits at the halt height.
- A plan built before the hold reaches `mark-ready`: the start guard fails the task, and the planner builds the next plan from the current spec.
- The pod is not Ready while held, because pod readiness follows the sidecar's start gate. A Service that routes only to Ready pods drops the node.
- The parked container restarts after about five days, when its startup probe gives up. The hold stays, and the new container parks again. An open exec session ends.
- An operator releases the hold while an exec'd `seid rollback` still runs. seid then starts beside it. The runbook MUST tell the operator to finish exec work before release.
- An operator deletes or zeroes `priv_validator_state.json` through exec. The controller does not detect it. The runbook MUST forbid it.
- An emergency hold that cannot wait for a merge: the operator suspends the Flux Kustomization and patches the field. The operator MUST resume Flux only after git carries the same value, or Flux releases the hold.

## Requirements *(mandatory)*

### Requirement 1: The SeiNode carries a hold field

**Objective:** As an operator, I want the hold in git, so that it is reviewed, durable, and visible.

**Traces to:** User Story 1, User Story 2

#### Acceptance Criteria

1. The SeiNode spec SHALL carry an optional `spec.maintenance.hold` field with the values `Immediate` and `AfterExit`.
2. WHEN a SeiNode without `spec.nodeConfig` sets the hold, THE API server SHALL reject it.
3. The SeiNode status SHALL carry a `status.maintenanceHold` field that the controller alone writes.

### Requirement 2: The controller closes the gate and keeps it closed

**Objective:** As an operator, I want seid parked with the pod alive, so that I can run tools against the data.

**Traces to:** User Story 1, User Story 2, User Story 3, User Story 5

#### Acceptance Criteria

1. WHEN an `Immediate` hold is set on a `Running` node, THE controller SHALL run `mark-not-ready` and then `stop-seid`.
2. WHEN an `AfterExit` hold is set on a `Running` node whose hold in effect is empty, THE controller SHALL run `mark-not-ready` and SHALL NOT stop seid.
3. WHEN an `AfterExit` hold is set on a node whose hold in effect is `Immediate`, THE controller SHALL start seid once.
4. WHILE a hold is set and the node is `Running`, THE start guard SHALL refuse `mark-ready` on every path that spec 009 Requirement 3 names, except the start-once step of a hold plan. THE start-once step SHALL refuse unless the requested hold is still `AfterExit`.
5. WHILE a hold is set, THE controller SHALL NOT build a plan that contains `mark-ready`, except the start-once step of a hold plan.
6. WHILE a hold is set, THE controller SHALL keep applying the StatefulSet, so a template change rolls the pod and the new pod stays parked.
7. WHEN the hold plan completes, THE controller SHALL set `status.maintenanceHold` to the hold value the plan was built for.
8. WHILE `status.maintenanceHold` equals the hold and the sidecar does not report the start gate open, THE controller SHALL NOT build a hold plan.
9. WHERE a hold is set when the node initializes, THE controller SHALL build the init plan without its final `mark-ready`, and SHALL set `status.maintenanceHold` to `Immediate` when the plan completes.
10. WHILE a hold is requested, a hold is in effect, and the sidecar reports the start gate open, THE controller SHALL first build the plan that applies the hold in effect again, so the gate closes. The next plan then moves the hold to the requested value with the gate closed.

### Requirement 3: The hold composes with the data reset

**Objective:** As an operator, I want a reset to run on a held node, so that I can save files first and inspect the result after.

**Traces to:** User Story 4

#### Acceptance Criteria

1. WHILE a hold is set and a reset is pending, THE controller SHALL build the reset plan without its final `mark-ready`.
2. WHEN that reset plan completes, THE controller SHALL set `status.maintenanceHold` to `Immediate`, because the reset leaves seid parked.

### Requirement 4: The controller releases the node

**Objective:** As an operator, I want removing the hold to start seid, so that release is one merge.

**Traces to:** User Story 1

#### Acceptance Criteria

1. WHEN the hold is removed from a held node and no reset is pending, THE controller SHALL build a plan that marks the sidecar ready.
2. WHEN the hold is removed and a reset is pending, THE controller SHALL build the reset plan with its final `mark-ready`.
3. WHEN the plan that marks the sidecar ready completes, THE controller SHALL clear `status.maintenanceHold`.

### Requirement 5: The controller reports the hold

**Objective:** As an operator, I want status that tells me whether seid is parked, so that I know when exec is safe.

**Traces to:** User Story 1, User Story 2

#### Acceptance Criteria

1. The controller SHALL seed a `MaintenanceInProgress` condition on every SeiNode. `True` is the exception, `False` is the steady state.
2. WHILE no hold is set, THE condition SHALL be `False` with reason `NotHeld`, or `NotApplicable` on a node without `spec.nodeConfig`.
3. WHILE a hold is set and differs from `status.maintenanceHold`, WHILE a hold plan runs, or WHILE the sidecar reports the gate open under a hold, THE condition SHALL be `True` with reason `HoldPending`.
4. WHILE an `Immediate` hold is in effect, THE condition SHALL be `True` with reason `Held`.
5. WHILE an `AfterExit` hold is in effect, THE condition SHALL be `True` with reason `Armed`.
6. WHEN the hold takes effect or the node is released, THE controller SHALL record an event on the SeiNode.

### Key Entities

- **Hold** (`spec.maintenance.hold`): the operator's request that seid stay parked. `Immediate` or `AfterExit`.
- **Hold in effect** (`status.maintenanceHold`): the controller's record of the hold whose plan last completed.
- **`MaintenanceInProgress` condition**: the hold signal, with reasons `NotHeld`, `NotApplicable`, `HoldPending`, `Held`, `Armed`.
- **Start guard**: the spec 009 predicate, extended here to "no reset is pending and no hold is set".

## Success Criteria *(mandatory)*

- **SC-001**: The API server rejects a hold on a node without `spec.nodeConfig`, and a value other than `Immediate` or `AfterExit`.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the CEL envtest cases pass.
- **SC-002**: An `Immediate` hold builds a plan of `mark-not-ready` then `stop-seid`. An `AfterExit` hold on a running node builds a plan of `mark-not-ready` only. An `AfterExit` hold on a parked node builds the start-once plan. A hold on a new node removes the init plan's final `mark-ready`.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the planner tests for both hold values pass.
- **SC-003**: While held, the planner builds no plan that contains `mark-ready`, and the start guard fails a `mark-ready` from a stale plan and from a `MarkReady` SeiNodeTask.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the planner, executor, and SeiNodeTask guard tests pass.
- **SC-004**: A reset on a held node runs without its final `mark-ready`, and the hold in effect becomes `Immediate`. Release then builds a plan that marks the sidecar ready.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the planner tests for reset-while-held and release pass.
- **SC-005**: The `MaintenanceInProgress` condition reads `HoldPending`, then `Held` or `Armed`, then `NotHeld` after release.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the condition tests pass.
- **SC-006**: On a harbor node, a hold survives a pod delete and a controller restart, and `seid rollback` through exec works on the parked container.
  *Verifier:* judgement — a platform engineer runs the scenario on a harbor node and reports each step.
- **SC-007**: On a harbor chain, an `AfterExit` hold keeps seid stopped after it exits at `halt-height`.
  *Verifier:* judgement — a platform engineer sets a halt height and an `AfterExit` hold on a harbor node and confirms the height stops at H.

## Assumptions

- The start gate closes on a new pod, and only a completed `mark-ready` opens it. The sidecar keeps the flag in memory.
- `stop-seid` stops only the `seid start` process. It does not stop an exec'd tool.
- The sidecar can see the `seid start` process, because the pod shares one PID namespace. A new read-only sidecar task, `await-seid-start`, waits for it, for at most two minutes.
- Deploy order: the sidecar image first, then the controller. An older sidecar does not know `await-seid-start`. To roll the controller back, first release every hold: an older controller has no hold and would start seid.
- The seid container's startup probe targets the sidecar's healthz, not seid's RPC, so the kubelet does not kill a parked container for about five days.
- The pod template carries `karpenter.sh/do-not-disrupt`, so Karpenter does not move a held pod on its own.
- The operator's tools are in the seid image. `seidb` joins it through sei-protocol/sei-chain#4488.

## Out of scope

- Production RBAC for `pods/exec`. Exec into a validator's seid container gives access to its signing keys, so the platform repository MUST treat it as break-glass.
- A check on release that the sign state did not move back. The runbook rule covers it.
- A check on release that no exec'd process still runs. The runbook rule covers it.
- Typed SeiNodeTask kinds for diagnostics that return a result, such as a state dump to S3.
- A hold on a node without `spec.nodeConfig`. The `SeiNodeTaskWorkflow` hold covers those nodes.
