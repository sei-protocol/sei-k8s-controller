# Feature Specification: Declarative data reset for ConfigMap-configured nodes

**Feature Branch**: `009-declarative-data-reset`

**Created**: 2026-09-30

**Status**: Draft

**Tracking**: PLT-1342 (atlantic-2 migration)

**Unblocks**: state sync and the giga state-store migration on a SeiNode whose
config comes from GitOps-managed ConfigMaps (`spec.nodeConfig`). The team is
moving atlantic-2 and pacific-1 node config onto `spec.nodeConfig`. Those
operations follow one pattern: pause the node, clear the data directory, update
the config, restart the node. No GitOps-shaped way to run that pattern exists
today. The `SeiNodeTaskWorkflow` StateSync recipe refuses a `nodeConfig` target,
because it writes `config.toml` at run time and the ConfigMap mount is read-only.
A `nodeConfig` change rolls the pod as soon as the StatefulSet is applied, so an
imperative workflow cannot order a data wipe before the new config takes effect.

**Input**: the atlantic-2 Kubernetes requirements thread with Yiming, 2026-09-30,
and the design discussion that followed. The fence below holds the originators'
words. The writing rules govern this document, not its source.

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

This spec adds one typed, monotonically increasing counter to the SeiNode spec:
a request to reset the node's data. The controller records the last counter value
it handled in status and reports progress on a condition. An operator runs state
sync by merging one commit that publishes a new ConfigMap with the state-sync
trust point, points the node at it, and bumps the counter. The controller wipes
the data while seid is held at the sidecar start gate, then releases seid onto the
new config. Nothing outside git writes the node's config.

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | the counter field, its status mirror, the condition | whether the controller reconciles correctly |
| Kubernetes CEL validation rules (transition rules, `oldSelf`) | the counter's monotonicity at admission | writes that bypass the API server's validation |
| Flux `reconcile.fluxcd.io/requestedAt` / `status.lastHandledReconcileAt` | the request-in-spec, handled-in-status shape | the destructive action this spec attaches to it |
| CometBFT `priv_validator_state.json` | the double-sign guard a reset must preserve | where the validator key itself is stored or backed up |

## Glossary

- **Controller**: the sei-k8s-controller SeiNode reconciler.
- **Operator**: a person who runs a node and merges the GitOps commit that changes it.
- **SeiNode**: the CRD for a single node.
- **ConfigMap-configured node**: a SeiNode with `spec.nodeConfig` set. Its `config.toml` and `app.toml` come verbatim from operator ConfigMaps mounted read-only.
- **Config change**: publishing a ConfigMap under a new name and pointing `spec.nodeConfig` at it. Kubernetes rolls the pod when the controller applies the new pod template.
- **Reset counter**: the new `spec.dataResetGeneration` field, a non-negative integer. It is a SeiNode field, not a `DataVolumeSpec` field, because `DataVolumeSpec` is shared with the SeiNetwork, where no `spec.nodeConfig` gate applies.
- **Handled counter**: the new `status.dataResetGeneration` field, the last reset counter value the controller finished resetting for.
- **Pending reset**: the state in which the reset counter is greater than the handled counter.
- **Data reset**: the sidecar `reset-data` task. It wipes `<home>/data/` and keeps `<home>/config/`, the node identity, and the sidecar database.
- **Start gate**: the sidecar readiness flag that holds seid from starting until the controller issues `mark-ready`. A new pod starts with the gate closed.
- **Sign state**: the validator's `priv_validator_state.json`, which records the last height, round, and step it signed.
- **Trust point**: the state-sync `trust-height` and `trust-hash` a node needs to state sync.
- **Reset commit**: one GitOps commit that carries a config change and a reset counter increment together.

## Boundary Context

- **Sits within**: the SeiNode spec, the SeiNode status, and the Running-phase plan the controller builds for a ConfigMap-configured node.
- **Owns**: the reset counter, the handled counter, the `DataResetInProgress` condition, the ordering of the data reset against the start gate, and the preservation of the sign state across a reset.
- **Does not own**: the content of the ConfigMaps. The operator owns it, as `spec.nodeConfig` already states.
- **Does not own**: fetching the trust point from live witnesses and rendering the reset commit. seictl owns that; this spec states the commit it MUST emit.
- **Does not own**: operations that carry parameters or produce a result, such as a gov proposal, a vote, or a state dump. `SeiNodeTask` owns those.
- **Does not own**: the `SeiNodeTaskWorkflow` StateSync recipe on a node without `spec.nodeConfig`. That recipe is unchanged.
- **Does not own**: backup of the validator key itself.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - State sync a ConfigMap-configured node with one merge (Priority: P1)

An operator state syncs a full node by merging one commit. The commit adds a
ConfigMap with `[statesync] enable = true`, the `rpc-servers`, and a fresh trust
point. It points `spec.nodeConfig.configRef` at the new ConfigMap and increments
`spec.dataResetGeneration`. The node restarts once, on empty data, and
state syncs.

**Why this priority**: state sync is a hard requirement for retiring sei-infra,
and a ConfigMap-configured node has no way to state sync today.

**Independent Test**: on a running ConfigMap-configured full node, merge a reset
commit. Confirm the pod rolls once, seid never serves on the old data with the new
config, the data directory is empty before seid starts, the node state syncs, and
the handled counter reaches the new value.

**Acceptance Scenarios**:

1. **Given** a running ConfigMap-configured node with the handled counter at 0, **When** the operator merges a reset commit that sets the reset counter to 1, **Then** the controller wipes the data on the new pod before it issues `mark-ready`, and sets the handled counter to 1.
2. **Given** the handled counter at 1, **When** the pod restarts or the controller restarts, **Then** the controller does not wipe the data again.

---

### User Story 2 - Run the giga state-store migration with the same commit shape (Priority: P1)

An operator runs the giga migration by merging a reset commit whose new ConfigMap
also carries `[state-store] ss-enable = true`, `[state-store] evm-ss-split = true`,
and `[state-commit] sc-enable = true` in `app.toml`. The migration keys live in
git, so no later render reverts them.

**Why this priority**: it is the migration the team runs today, and on the
configValues path its keys are written outside git and are reverted by the next
config render (known defect B1, `internal/planner/config_update.go`).

**Independent Test**: merge a giga reset commit on a ConfigMap-configured node,
then merge an unrelated config change. Confirm the migration keys are still set in
the running node's `app.toml`.

**Acceptance Scenarios**:

1. **Given** a reset commit with the giga keys, **When** the node restarts on it, **Then** the running `app.toml` carries the keys and the data directory was empty when seid started.

---

### User Story 3 - A reset never double-signs a validator (Priority: P1)

An operator resets a validator's data. The reset keeps the validator's sign state,
so the validator cannot sign again at a height it already signed.

**Why this priority**: atlantic-2 and pacific-1 validators are in scope, and a
double sign is slashed. The data reset today writes a zero sign state, which is
only safe because the StateSync recipe excludes validators.

**Independent Test**: on a validator whose sign state records height H, merge a
reset commit. Confirm the sign state after the reset still records height H.

**Acceptance Scenarios**:

1. **Given** a validator with a sign state at height H, **When** the controller runs the data reset, **Then** the sign state still records height H, round, and step after the wipe.

---

### User Story 4 - Watch and wait on a reset (Priority: P2)

An operator or a runbook waits for reset N to finish with
`kubectl wait seinode/<name> --for=jsonpath='{.status.dataResetGeneration}'=N`,
and reads the `DataResetInProgress` condition to see why a reset is stuck. The
wait compares counters, not the condition: right after a merge the condition can
still read `False` from the previous reset, because the controller has not yet
seen the new counter, and `kubectl wait` does not check `observedGeneration`.

**Why this priority**: the merge is the only operator action, so status is the
only feedback an operator gets.

**Independent Test**: merge a reset commit, wait on the handled counter, and
read the `DataResetInProgress` condition through its transitions.

**Acceptance Scenarios**:

1. **Given** a pending reset, **When** the operator reads the condition, **Then** it is `True` with reason `ResetPending` or `ResetRunning`, and its message names the reset counter value.
2. **Given** a data reset task that fails, **When** the operator reads the condition, **Then** it is `True` with reason `ResetFailed`, its message carries the task error, and seid is still held.
3. **Given** the operator merged reset N, **When** the wait on `status.dataResetGeneration` returns, **Then** the data reset for N has succeeded.

---

### User Story 5 - A reverted commit does not rearm the wipe (Priority: P2)

An operator reverts a reset commit in git to undo the config change. The revert
does not lower the reset counter and does not trigger a second wipe.

**Why this priority**: a git revert is the normal undo, and a wipe MUST NOT happen
by accident.

**Independent Test**: apply a SeiNode update that lowers the reset counter.
Confirm the API server rejects it.

**Acceptance Scenarios**:

1. **Given** a reset counter at 2, **When** an update sets it to 1 or removes it, **Then** the API server rejects the update.

### Edge Cases

- A commit that only increments the reset counter, with no config change, resets the data on the running pod: the controller closes the start gate, stops seid, runs the data reset, and reopens the gate.
- A commit that only changes the config restarts the node on its existing data. That is today's behaviour and needs no counter.
- A config change and a counter increment in two separate commits roll the pod twice. The first roll starts seid on the old data with the new config. The controller cannot detect this; seictl MUST emit one reset commit.
- The counter jumps by more than one: the controller runs one data reset and sets the handled counter to the spec value.
- The counter increments again while a reset is in progress: the controller finishes the current reset, then sees the handled counter behind the spec and runs one more.
- `spec.paused` is true: the reset waits, and the condition stays `ResetPending`.
- The data reset fails: the start gate stays closed and seid stays held. The controller retries the reset with backoff while the reset stays pending, the same way a failed Running plan is retried on a later reconcile. The operator does not bump the counter to retry; a bump asks for a second wipe.
- The trust point in the ConfigMap is older than the trust period when the commit merges: seid fails to state sync. The controller does not validate the trust point.

## Requirements *(mandatory)*

The controller already holds seid at the start gate on a new pod until it issues
`mark-ready`, and already has a data reset task that refuses to run while seid
serves RPC. This spec orders those two pieces against a counter.

### Requirement 1: The SeiNode carries a monotonic reset counter

#### Acceptance Criteria

1. The SeiNode spec SHALL carry an optional `spec.dataResetGeneration` field, a 64-bit integer with a minimum of 0.
2. WHEN an update lowers the reset counter or removes a set reset counter, THE API server SHALL reject the update.
3. WHEN a SeiNode without `spec.nodeConfig` sets the reset counter, THE API server SHALL reject it.
4. The SeiNode status SHALL carry a `status.dataResetGeneration` field that the controller alone writes.

### Requirement 2: The controller resets the data once per counter value, before seid starts

#### Acceptance Criteria

1. WHILE the reset counter is greater than the handled counter, THE controller SHALL NOT issue `mark-ready` on the node's pod until the data reset for that value has succeeded.
2. WHEN a reset is pending and the pod template changed in the same spec generation, THE controller SHALL run the data reset on the new pod, with seid held at the start gate.
3. WHEN a reset is pending and the pod template did not change, THE controller SHALL close the start gate and stop seid before it runs the data reset.
4. WHEN the data reset succeeds, THE controller SHALL set the handled counter to the reset counter value before it issues `mark-ready`.
5. WHILE the handled counter equals the reset counter, THE controller SHALL NOT run a data reset, across pod restarts and controller restarts.
6. IF the data reset fails, THEN THE controller SHALL keep the start gate closed, leave the handled counter unchanged, and retry the reset with backoff while the reset stays pending.
7. WHILE `spec.paused` is true, THE controller SHALL NOT start a data reset.

### Requirement 3: A reset preserves the sign state

#### Acceptance Criteria

1. WHEN the data reset runs on a node whose data directory holds a sign state, THE sidecar SHALL leave that sign state's height, round, and step unchanged.
2. WHEN the data reset runs on a node with no sign state, THE sidecar SHALL write the zero sign state that seid requires to start.
3. THE data reset SHALL NOT delete or rewrite `priv_validator_key.json` or `node_key.json`.

### Requirement 4: The controller reports the reset on a condition and a counter

#### Acceptance Criteria

1. The controller SHALL seed a `DataResetInProgress` condition on every SeiNode, following the `<Subject>InProgress` convention: `True` is the exception, `False` is the steady state.
2. WHILE no reset is pending, THE condition SHALL be `False`, with reason `ResetComplete` once a reset has run, `NoResetRequested` before one has, and `NotApplicable` on a node without `spec.nodeConfig`.
3. WHILE a reset is pending and has not started, THE condition SHALL be `True` with reason `ResetPending`.
4. WHILE the data reset runs, THE condition SHALL be `True` with reason `ResetRunning`.
5. IF the data reset fails, THEN THE condition SHALL be `True` with reason `ResetFailed`, and its message SHALL carry the task error.
6. The condition message SHALL name the reset counter value it refers to.
7. THE completion contract for reset N SHALL be `status.dataResetGeneration >= N`. Runbooks and seictl SHALL wait on that field, not on the condition.
8. WHEN the data reset starts, succeeds, or fails, THE controller SHALL record an event on the SeiNode.

### Requirement 5: The config comes only from git

#### Acceptance Criteria

1. WHILE a node is ConfigMap-configured, THE controller SHALL NOT write `config.toml` or `app.toml` as part of a data reset.
2. A state sync or a giga migration on a ConfigMap-configured node SHALL need only a reset commit: no `SeiNodeTaskWorkflow` and no out-of-band config write.

### Key Entities

- **Reset counter** (`spec.dataResetGeneration`): the operator's request. It carries no parameters; what the node restarts into is the ConfigMap and the rest of the spec.
- **Handled counter** (`status.dataResetGeneration`): the controller's record of the last value it reset for.
- **`DataResetInProgress` condition**: the progress and failure signal, with reasons `NoResetRequested`, `NotApplicable`, `ResetPending`, `ResetRunning`, `ResetComplete`, `ResetFailed`.
- **Reset commit**: the ConfigMap, the `spec.nodeConfig` reference, and the counter increment, in one commit.

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: The API server rejects an update that lowers or removes the reset counter, and one that sets it on a node without `spec.nodeConfig`. The SeiNetwork schema carries no reset counter.
  *Verifier:* `go test ./api/... ./internal/...` — an envtest case applies each rejected update and asserts the admission error.
- **SC-002**: With a reset pending, the controller does not issue `mark-ready` until the data reset has succeeded, and it sets the handled counter before it issues `mark-ready`.
  *Verifier:* `go test ./internal/controller/node/...` — a reconciler test with a fake sidecar asserts the task order `reset-data` → status write → `mark-ready`.
- **SC-003**: A reset commit rolls the pod once and wipes the data on the new pod before seid starts.
  *Verifier:* judgement — a platform engineer merges a reset commit on a harbor ConfigMap-configured full node and confirms one pod roll, an empty data directory at seid start, and a completed state sync.
- **SC-004**: A counter increment with no config change closes the gate, stops seid, and resets the data on the running pod.
  *Verifier:* `go test ./internal/planner/...` — a planner test asserts the plan `mark-not-ready` → `stop-seid` → `reset-data` → `mark-ready`.
- **SC-005**: A pod restart or a controller restart with the handled counter equal to the reset counter runs no data reset.
  *Verifier:* `go test ./internal/controller/node/...` — a reconciler test restarts the reconciler and deletes the pod, and asserts no `reset-data` task is submitted.
- **SC-006**: A failed data reset keeps seid held, leaves the handled counter unchanged, reads `ResetFailed` with the task error, and is retried with no spec change.
  *Verifier:* `go test ./internal/controller/node/...` — a reconciler test fails the fake sidecar's `reset-data` once, asserts the gate, the status, and the condition, then lets it succeed on a later reconcile and asserts the handled counter advances.
- **SC-007**: A data reset leaves an existing sign state's height, round, and step unchanged, and leaves the validator key and node key untouched.
  *Verifier:* `go test ./sidecar/tasks/...` — a `ResetDataer` test seeds a sign state at a non-zero height and both key files, runs the reset, and asserts them byte for byte.
- **SC-008**: The `DataResetInProgress` condition moves through `True/ResetPending`, `True/ResetRunning`, and `False/ResetComplete` for one reset, and names the counter value.
  *Verifier:* `go test ./internal/controller/node/...` — a reconciler test asserts the condition after each step.
- **SC-009**: A giga reset commit leaves the migration keys in the running `app.toml` after a later unrelated config change.
  *Verifier:* judgement — a platform engineer runs the giga reset commit on a harbor node, merges an unrelated ConfigMap change, and reads `app.toml` in the pod.
- **SC-010**: The platform repo holds a runbook for state sync and for the giga migration on a ConfigMap-configured node, each a single reset commit.
  *Verifier:* judgement — an atlantic-2 operator follows the runbook on a harbor node without help and confirms it covers the reset commit, the wait, and the failure path.

## Assumptions

- The start gate closes on a new pod: the sidecar's readiness flag starts false, and the controller re-issues `mark-ready` when it reads `SidecarReady=False`. The pod's seid container does not start while the gate is closed.
- seid state syncs only when its block store is empty. A ConfigMap that keeps `[statesync] enable = true` after a successful state sync does not re-sync on a later restart. SC-003 checks this.
- A sign state preserved across a state sync is safe: after the sync the node signs only at heights above the snapshot height, and the preserved sign state blocks any height at or below the last one signed.
- `spec.nodeConfig` stays create-only. Moving an existing node onto it means replacing the node and carrying its data over with `spec.dataVolume.import`.
- The controller already rolls the pod on a `spec.nodeConfig` reference change, because the references are in the pod template.

## Out of scope

- The seictl command that fetches a trust point from two or more live witnesses and renders the reset commit. It follows this spec as a separate work item.
- A rollback counter for `seid rollback`. It would reuse this counter's shape and is a separate spec.
- Diagnostics that return a result, such as a state dump on a chain halt. Those are new `SeiNodeTask` kinds.
- Changes to the `SeiNodeTaskWorkflow` StateSync recipe for nodes without `spec.nodeConfig`, including the fix for B1 on that path.
- Validator key backup and restore, and remote signing.
- Validating the trust point or the ConfigMap content.
