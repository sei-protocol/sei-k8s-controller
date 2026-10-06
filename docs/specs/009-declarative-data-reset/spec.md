# Feature Specification: Declarative data reset for ConfigMap-configured nodes

**Feature Branch**: `009-declarative-data-reset`

**Created**: 2026-09-30

**Revised**: 2026-10-06, after the operational-readiness review (PLT-1389)

**Status**: Draft

**Tracking**: PLT-1342 (atlantic-2 migration)

**Unblocks**: state sync and the giga state-store migration on a SeiNode whose
config comes from GitOps-managed ConfigMaps (`spec.nodeConfig`). The team is
moving atlantic-2 and pacific-1 node config onto `spec.nodeConfig`. Those
operations follow one pattern: pause the node, clear the data directory, update
the config, restart the node. No GitOps-shaped way to run that pattern exists
today. The `SeiNodeTaskWorkflow` StateSync recipe refuses a `nodeConfig` target,
because it writes `config.toml` at run time and the ConfigMap mount is read-only.
A `nodeConfig` change rolls the pod as soon as the controller applies the
StatefulSet, so an imperative workflow cannot order a data wipe before the new
config takes effect.

**Input**: the atlantic-2 Kubernetes requirements thread with Yiming, 2026-09-30,
the design discussion that followed, and the readiness review on PLT-1389. The
fence below holds the originators' words. The writing rules govern this document,
not its source.

```text
Being able to restart the node with a different config. State Sync the nodes.
Never lose the private keys for the validator node. Manage config via git is
actually fine, as long as we have an example or runbook for how to do that. It's
also a good thing to move to, since it makes config updates public, and require
approval. Maybe someone just need to run a command, and that could automatically
generate the PR we need for state sync or gov proposal.

A use-case that I fear becomes more common is something like state sync or giga
state migration: [pause node] -> [clear data dir] -> [update config] -> [restart
node]. I love the idea of having this be a single workflow that executes.
Managing config in git throws a wrench in this. The general consensus is to move
towards ConfigMap node configs managed in gitops.
```

This spec adds one typed counter to the SeiNode spec: a request to reset the
node's data. The value can only increase. The controller records the last value
it handled in status and reports progress on a condition. An operator runs state
sync by merging one commit. The commit publishes a new ConfigMap with the
state-sync trust point, points the node at it, and increments the counter. The
controller wipes the data while seid waits at the sidecar start gate. It then
releases seid onto the new config. Nothing outside git writes the node's config.

The spec also adds the start guard. It is one rule that every path which can start
seid obeys. Spec 010 adds the maintenance hold to the same rule.

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| Kubernetes API conventions | the counter field, its status mirror, the condition | whether the controller reconciles correctly |
| Kubernetes CEL validation rules (transition rules, `oldSelf`) | the counter's monotonicity at admission | writes that bypass the API server's validation |
| Flux `reconcile.fluxcd.io/requestedAt` / `status.lastHandledReconcileAt` | the request-in-spec, handled-in-status shape | the destructive action this spec attaches to it |
| CometBFT `priv_validator_state.json` | the double-sign guard a reset must keep | where the validator key itself is stored, and a second signer with the same key |

## Glossary

- **Controller**: the sei-k8s-controller SeiNode reconciler.
- **Operator**: a person who runs a node and merges the GitOps commit that changes it.
- **ConfigMap-configured node**: a SeiNode with `spec.nodeConfig` set. Its `config.toml` and `app.toml` come verbatim from operator ConfigMaps, mounted read-only.
- **Config change**: publishing a ConfigMap under a new name and pointing `spec.nodeConfig` at it. Kubernetes rolls the pod when the controller applies the new pod template.
- **Reset counter**: the new `spec.dataResetGeneration` field, a non-negative integer. It is a SeiNode field, not a `DataVolumeSpec` field, because `DataVolumeSpec` is shared with the SeiNetwork, where no `spec.nodeConfig` gate applies.
- **Handled counter**: the new `status.dataResetGeneration` field. It holds the last reset counter value the controller finished resetting for.
- **Pending reset**: the state in which the reset counter is greater than the handled counter.
- **Data reset**: the sidecar `reset-data` task. It wipes `<home>/data/` and keeps `<home>/config/`, the node identity, the sidecar database, and the sign state.
- **Start gate**: the sidecar readiness flag that holds seid from starting until the sidecar receives `mark-ready`. A new pod starts with the gate closed.
- **Start guard**: the controller rule that decides whether `mark-ready` may reach the sidecar. Requirement 3 defines it.
- **Reset plan**: the Running-phase plan that performs one data reset.
- **Sign state**: the validator's `priv_validator_state.json`. It records the last height, round, and step the validator signed.
- **Trust point**: the state-sync `trust-height` and `trust-hash` a node needs to state sync.
- **Reset commit**: one GitOps commit that carries a config change and a reset counter increment together.

## Boundary Context

- **Sits within**: the SeiNode spec, the SeiNode status, the Running-phase plans the controller builds for a ConfigMap-configured node, and the sidecar `reset-data` task.
- **Owns**: the reset counter, the handled counter, the `DataResetInProgress` condition, the reset plan, the start guard, and the sign state across a reset.
- **Does not own**: the content of the ConfigMaps. The operator owns it, as `spec.nodeConfig` already states.
- **Does not own**: fetching the trust point and rendering the reset commit. seictl owns that, as a later work item.
- **Does not own**: the maintenance hold. Spec 010 owns it, and extends the start guard.
- **Does not own**: operations that carry parameters or produce a result, such as a gov proposal, a vote, or a state dump. `SeiNodeTask` owns those.
- **Does not own**: the `SeiNodeTaskWorkflow` StateSync recipe on a node without `spec.nodeConfig`. That recipe is unchanged.
- **Does not own**: backup of the validator key, and the move of a validator from EC2. The cutover runbook owns both.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - State sync a ConfigMap-configured node with one merge (Priority: P1)

An operator state syncs a full node by merging one commit. The commit adds a
ConfigMap with `[statesync] enable = true`, the `rpc-servers`, and a fresh trust
point. It points `spec.nodeConfig.configRef` at the new ConfigMap and increments
`spec.dataResetGeneration`. The node restarts on empty data and state syncs.

**Why this priority**: state sync is a hard requirement for retiring sei-infra,
and a ConfigMap-configured node has no way to state sync today.

**Independent Test**: on a running ConfigMap-configured full node, merge a reset
commit. Confirm seid never serves the old data with the new config, the data
directory is empty before seid starts, the node state syncs, and the handled
counter reaches the new value.

**Acceptance Scenarios**:

1. **Given** a running ConfigMap-configured node with the handled counter at 0, **When** the operator merges a reset commit that sets the reset counter to 1, **Then** the controller wipes the data before it releases the start gate. It sets the handled counter to 1.
2. **Given** the handled counter at 1, **When** the pod restarts or the controller restarts, **Then** the controller does not wipe the data again.

---

### User Story 2 - Run the giga state-store migration with the same commit shape (Priority: P1)

An operator runs the giga migration by merging a reset commit whose new ConfigMap
also carries `[state-store] ss-enable = true`, `[state-store] evm-ss-split = true`,
and `[state-commit] sc-enable = true` in `app.toml`. The migration keys live in
git, so no later render reverts them.

**Why this priority**: it is the migration the team runs today. On the
configValues path a later config render reverts its keys (known defect B1,
`internal/planner/config_update.go`).

**Independent Test**: merge a giga reset commit on a ConfigMap-configured node,
then merge an unrelated config change. Confirm the migration keys are still set in
the running node's `app.toml`.

**Acceptance Scenarios**:

1. **Given** a reset commit with the giga keys, **When** the node restarts on it, **Then** the running `app.toml` carries the keys and the data directory was empty when seid started.

---

### User Story 3 - A reset never double-signs a validator (Priority: P1)

An operator resets a validator's data. The reset keeps the validator's sign state,
so the validator cannot sign again at a height, round, and step it already signed.

**Why this priority**: atlantic-2 and pacific-1 validators are in scope. Double-sign
evidence jails and tombstones a validator, and an unjail cannot reverse a tombstone.
The data reset today deletes the sign state and writes a zero one. That is only
safe because the StateSync recipe excludes validators.

**Independent Test**: on a validator whose sign state records height H, merge a
reset commit. Confirm the sign state after the reset is the same file, byte for
byte.

**Acceptance Scenarios**:

1. **Given** a validator with a sign state at height H, **When** the controller runs the data reset, **Then** the sign state is unchanged after the wipe.
2. **Given** a data reset that stops after the wipe starts, **When** the sidecar runs it again, **Then** the sign state is still unchanged.

---

### User Story 4 - Nothing starts seid while a reset is pending (Priority: P1)

A plan built before the reset commit, or a `MarkReady` SeiNodeTask, can try to
release the start gate after the pod rolls onto the new config. The start guard
refuses it, so seid starts only on reset data.

**Why this priority**: a reset that a stale path can skip gives no guarantee.

**Independent Test**: start an image update, and merge a reset commit before the
update plan finishes. Confirm the update plan does not release the gate, and the
reset plan runs next.

**Acceptance Scenarios**:

1. **Given** an update plan in progress, **When** a reset becomes pending, **Then** the update plan's `mark-ready` fails, and the next plan the controller builds is the reset plan.
2. **Given** a pending reset, **When** an operator creates a `MarkReady` SeiNodeTask for the node, **Then** the task fails and the gate stays closed.

---

### User Story 5 - Watch and wait on a reset (Priority: P2)

An operator or a runbook waits for reset N to finish with
`kubectl wait seinode/<name> --for=jsonpath='{.status.dataResetGeneration}'=N`,
and reads the `DataResetInProgress` condition to see why a reset is stuck. The
wait compares counters, not the condition. Right after a merge the condition can
still read `False` from the previous reset, because the controller has not yet
seen the new counter. `kubectl wait` does not check `observedGeneration`.

**Why this priority**: the merge is the only operator action, so status is the
only feedback an operator gets.

**Independent Test**: merge a reset commit, wait on the handled counter, and
read the `DataResetInProgress` condition through its transitions.

**Acceptance Scenarios**:

1. **Given** a pending reset, **When** the operator reads the condition, **Then** it is `True` with reason `ResetPending` or `ResetRunning`, and its message names the reset counter value.
2. **Given** a data reset task that fails, **When** the operator reads the condition, **Then** it is `True` with reason `ResetFailed`, its message carries the task error, and seid is still held.
3. **Given** the operator merged reset N, **When** the wait on `status.dataResetGeneration` returns, **Then** the data reset for N has succeeded.

---

### User Story 6 - A reverted commit does not rearm the wipe (Priority: P2)

An operator reverts a reset commit in git to undo the config change. The revert
does not lower the reset counter and does not cause a second wipe.

**Why this priority**: a git revert is the normal undo, and a wipe MUST NOT happen
by accident.

**Independent Test**: apply a SeiNode update that lowers the reset counter.
Confirm the API server rejects it.

**Acceptance Scenarios**:

1. **Given** a reset counter at 2, **When** an update sets it to 1 or removes it, **Then** the API server rejects the update.

### Edge Cases

- A commit that only increments the reset counter, with no config change, resets the data on the running pod.
- A commit that only changes the config restarts the node on its existing data. That is today's behaviour and needs no counter.
- A config change and a counter increment in two separate commits roll the pod twice. The first roll starts seid on the old data with the new config. The controller does not detect this. The runbook MUST tell the operator to put both in one commit.
- The counter jumps by more than one: the controller runs one data reset and sets the handled counter to the value the reset plan was built for.
- The counter increments again while a reset is in progress: the controller finishes the current reset. The start guard then refuses `mark-ready`, because the counter is still ahead. The controller runs one more reset.
- A git revert of a reset commit fails the Flux apply for its Kustomization, because the API server rejects the lower counter. The operator fixes it forward: keep the counter, revert only the config.
- `spec.paused` is true: the StatefulSet has no replicas, so no reset runs. The condition stays `ResetPending`.
- The node is not `Running`: the controller keeps the handled counter equal to the reset counter, so creating or initializing a node never wipes it.
- The data reset fails: the start gate stays closed and seid stays held. The reset plan fails and the controller builds it again on a later reconcile. The operator does not bump the counter to retry; a bump asks for a second wipe.
- The trust point in the ConfigMap is older than the trust period when the commit merges: seid fails to state sync. The controller does not validate the trust point.
- An operator process such as `seid rollback` or `seidb` runs in the pod when the reset starts: the data reset refuses to wipe, and the plan retries.
- A validator's `config.toml` sets `[priv-validator] state-file` to a path other than `data/priv_validator_state.json`: the data reset refuses to run.

## Requirements *(mandatory)*

The controller already holds seid at the start gate on a new pod until it issues
`mark-ready`. It already has a data reset task that refuses to run while seid
serves RPC. This spec orders those two pieces against a counter, and guards every
path that releases the gate.

### Requirement 1: The SeiNode carries a monotonic reset counter

**Objective:** As an operator, I want a reset request in git, so that a reset is reviewed like any other change.

**Traces to:** User Story 1, User Story 6

#### Acceptance Criteria

1. The SeiNode spec SHALL carry an optional `spec.dataResetGeneration` field, a 64-bit integer with a minimum of 0.
2. WHEN an update lowers the reset counter or removes a set reset counter, THE API server SHALL reject the update.
3. WHEN a SeiNode without `spec.nodeConfig` sets the reset counter, THE API server SHALL reject it.
4. The SeiNode status SHALL carry a `status.dataResetGeneration` field that the controller alone writes.
5. WHILE the node is not `Running`, THE controller SHALL set the handled counter to the reset counter.

### Requirement 2: The controller resets the data once per counter value, before seid starts

**Objective:** As an operator, I want one merge to wipe the data before seid starts, so that seid never serves old data with a new config.

**Traces to:** User Story 1, User Story 2

#### Acceptance Criteria

1. WHILE a reset is pending and the node is `Running`, THE controller SHALL build the reset plan before any other Running-phase plan.
2. THE reset plan SHALL wait for the StatefulSet rollout, then run `mark-not-ready`, `stop-seid`, `reset-data`, a step that sets the handled counter, and `mark-ready`, in that order.
3. THE step that sets the handled counter SHALL set it to the reset counter value the plan was built for.
4. WHILE the handled counter equals the reset counter, THE controller SHALL NOT run a data reset, across pod restarts and controller restarts.
5. IF the data reset fails, THEN THE controller SHALL keep the start gate closed, leave the handled counter unchanged, and build the reset plan again on a later reconcile.
6. WHILE `spec.paused` is true, THE controller SHALL NOT start a data reset.
7. WHILE a node is ConfigMap-configured, THE controller SHALL NOT write `config.toml` or `app.toml` as part of a data reset.

### Requirement 3: The start guard refuses `mark-ready` while a reset is pending

**Objective:** As an operator, I want one rule between every path and the start gate, so that no stale plan or task can skip a reset.

**Traces to:** User Story 4

#### Acceptance Criteria

1. WHILE a reset is pending, THE controller SHALL NOT submit `mark-ready` to the node's sidecar from a plan.
2. IF a plan reaches `mark-ready` while a reset is pending, THEN THE controller SHALL fail that task. The planner then builds the next plan from the current spec.
3. WHILE a reset is pending, THE controller SHALL fail a `MarkReady` SeiNodeTask that targets the node, and SHALL NOT submit it to the sidecar.
4. WHILE a reset is pending, THE controller SHALL NOT build a plan that re-marks sidecar readiness.

### Requirement 4: A reset keeps the sign state

**Objective:** As a validator operator, I want the sign state to survive every reset, so that a reset cannot cause a double sign.

**Traces to:** User Story 3

#### Acceptance Criteria

1. WHEN the data reset runs on a node whose data directory holds a sign state, THE sidecar SHALL leave that file in place, unchanged.
2. WHEN the data reset runs on a node with no sign state, THE sidecar SHALL write the zero sign state that seid requires to start.
3. THE data reset SHALL NOT delete or rewrite `priv_validator_key.json` or `node_key.json`.
4. IF `config.toml` sets `[priv-validator] state-file` to a path other than `data/priv_validator_state.json`, THEN THE sidecar SHALL refuse the data reset.
5. IF a `seid` or `seidb` process runs in the pod, THEN THE sidecar SHALL refuse the data reset.

### Requirement 5: The controller reports the reset on a condition and a counter

**Objective:** As an operator, I want status that tells me where a reset is, so that I can wait on it and see why it is stuck.

**Traces to:** User Story 5

#### Acceptance Criteria

1. The controller SHALL seed a `DataResetInProgress` condition on every SeiNode, following the `<Subject>InProgress` convention: `True` is the exception, `False` is the steady state.
2. WHILE no reset is pending, THE condition SHALL be `False`, with reason `ResetComplete` once a reset has run, `NoResetRequested` before one has, and `NotApplicable` on a node without `spec.nodeConfig`.
3. WHILE a reset is pending and its plan has not started, THE condition SHALL be `True` with reason `ResetPending`.
4. WHILE the reset plan runs, THE condition SHALL be `True` with reason `ResetRunning`.
5. IF the reset plan fails, THEN THE condition SHALL be `True` with reason `ResetFailed`, and its message SHALL carry the task error.
6. The condition message SHALL name the reset counter value it refers to.
7. THE completion contract for reset N SHALL be `status.dataResetGeneration >= N`. Runbooks and seictl SHALL wait on that field, not on the condition.
8. WHEN the reset plan starts, succeeds, or fails, THE controller SHALL record an event on the SeiNode.

### Key Entities

- **Reset counter** (`spec.dataResetGeneration`): the operator's request. It carries no parameters; what the node restarts into is the ConfigMap and the rest of the spec.
- **Handled counter** (`status.dataResetGeneration`): the controller's record of the last value it reset for.
- **Start guard**: the predicate the controller checks before any `mark-ready`. In this spec it is "no reset is pending". Spec 010 adds "no hold is set".
- **`DataResetInProgress` condition**: the progress and failure signal, with reasons `NoResetRequested`, `NotApplicable`, `ResetPending`, `ResetRunning`, `ResetComplete`, `ResetFailed`.
- **Reset commit**: the ConfigMap, the `spec.nodeConfig` reference, and the counter increment, in one commit.

## Success Criteria *(mandatory)*

Every criterion names the check, or says `judgement` with the role that decides.
Test names are the ones the implementation adds; each test names its requirement.

- **SC-001**: The API server rejects an update that lowers or removes the reset counter, and one that sets it on a node without `spec.nodeConfig`. The SeiNetwork schema carries no reset counter.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the CEL envtest cases for each rejected update pass.
- **SC-002**: A node that is not `Running` keeps the handled counter equal to the reset counter, and runs no reset.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the planner test for a node created with a non-zero counter passes.
- **SC-003**: With a reset pending on a `Running` node, the planner builds the reset plan in the order of Requirement 2. It builds no update or readiness plan.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the planner tests for the reset plan and its precedence pass.
- **SC-004**: A plan built before the counter increment does not release the gate: its `mark-ready` fails, and the next plan is the reset plan.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the executor test that bumps the counter under an active update plan passes.
- **SC-005**: A `MarkReady` SeiNodeTask fails while a reset is pending, and submits nothing to the sidecar.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the SeiNodeTask controller test for the guard passes.
- **SC-006**: A pod restart or a controller restart with the handled counter equal to the reset counter runs no data reset.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the planner test with equal counters builds no reset plan.
- **SC-007**: A data reset leaves an existing sign state byte for byte, also when it runs a second time after a partial wipe. It leaves the validator key and node key untouched. It refuses a non-default `state-file`, and refuses while a `seid` or `seidb` process runs.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the `ResetDataer` tests in `sidecar/tasks` pass.
- **SC-008**: The `DataResetInProgress` condition moves through `True/ResetPending`, `True/ResetRunning`, and `False/ResetComplete` for one reset, and names the counter value.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the condition tests pass.
- **SC-009**: A reset commit on a harbor ConfigMap-configured full node wipes the data before seid starts, and the node state syncs.
  *Verifier:* judgement — a platform engineer merges a reset commit on a harbor node and confirms an empty data directory at seid start and a completed state sync.
- **SC-010**: The runbooks repository holds a runbook for state sync and for the giga migration on a ConfigMap-configured node, each a single reset commit.
  *Verifier:* judgement — an atlantic-2 operator follows the runbook on a harbor node without help, and confirms it covers the reset commit, the wait, and the failure path.

## Assumptions

- The start gate closes on a new pod. The sidecar's readiness flag lives in memory and starts false, and only a completed `mark-ready` sets it. The seid container does not start seid while the gate is closed.
- seid state syncs only when its block store is empty. A ConfigMap that keeps `[statesync] enable = true` after a successful state sync does not re-sync on a later restart.
- A sign state kept across a state sync is safe. After the sync the node signs only above the snapshot height, and the kept sign state blocks any height, round, and step at or below the last one signed.
- `stop-seid` succeeds as a no-op when seid is already parked at the start gate. The reset plan can therefore use one sequence whether or not the pod rolled.
- `spec.nodeConfig` stays create-only. Moving an existing node onto it means replacing the node and carrying its data over with `spec.dataVolume.import`. The cutover runbook MUST copy the final sign state from the old host.

## Out of scope

- The seictl command that fetches a trust point from live witnesses and renders the reset commit.
- Detection of a config change that needs a reset but arrives without a counter increment. The runbook rule covers it.
- A CI check in the GitOps repository that refuses a lowered counter.
- Reading a non-default `state-file` path. The sidecar refuses instead.
- The maintenance hold, `seid rollback`, and state dumps. Spec 010 owns the hold; exec runs the tools.
- Changes to the `SeiNodeTaskWorkflow` StateSync recipe for nodes without `spec.nodeConfig`, including the fix for B1 on that path.
- Validator key backup and restore, remote signing, and a second signer with the same key.
- Validating the trust point or the ConfigMap content.
