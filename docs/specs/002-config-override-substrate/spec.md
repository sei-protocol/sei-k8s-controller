# Feature Specification: Arbitrary config override substrate

**Feature Branch**: `002-config-override-substrate`

**Created**: 2026-09-08

**Status**: Draft

**Blocks**: the Barcelona benchmark event. This spec adds a config-value field
that names a file, a key, and a value. The controller merges that field over its
own config. The event needs a new seid key to work without new controller code.

**Input**: the Benchmark Party transcript, 2026-09-04. Review on the pull request
refined it. The fence below holds the originator's words. The writing rules govern
this document, not its source.

```text
Change overrides so the first top-level key is a file name. Then I can add
whatever I want. The contract is: the first key is the file name, and after that,
whatever you give me I dump into that file. If the file exists already, you get a
JSON merge-patch of both. Then no matter what configuration anybody throws at us
— EVM-only enable=true or anything — we run that container without writing extra
Go code inside the controller. Also make overrides mutable, so a change
materializes. I am happy to let the user shoot themselves in the foot: a wrong
config means the node does not run, but they have full control.
```

Review refined the shape. The override arrives as a new field of config values,
each with a file name, a key, and a value. The controller merges the values as an
overlay over the base config before seid starts. When the operator removes a
value, the controller recomputes the overlay, updates the config, and restarts
seid.

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach, because that gap is the honest part.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | CRD field shape, status, conditions | whether the controller reconciles correctly |
| Google AIP | resource field naming and consistency | whether the resource model is the right one |

## Glossary

- **Config file**: a seid config file such as config.toml or app.toml.
- **Config value**: one override entry, with a file name, a key, and a value.
- **Existing overrides**: the current dotted-key override map on the node, beside which the config-value field sits.
- **Base config**: the config file the controller writes from its own defaults, before the overlay.
- **Overlay**: the set of config values the controller merges over the base config before seid starts.
- **Merge**: the key-by-key application of the overlay onto a base config file, from the existing tomlpatch engine.
- **Config-apply**: the controller step that writes the overlay before seid starts.
- **Materialize**: make a config change take effect on the node.
- **Register**: the controller's current map from a known config key to its file, which a config value bypasses.

## Boundary Context

- **Sits within**: the override path in the sei-k8s-controller, for a SeiNode — the existing overrides, the config-apply step, the restart-seid task, and the tomlpatch merge engine.
- **Owns**: the new config-value field on a SeiNode, its merge as an overlay over the base config, and the materialization of a change.
- **Does not own**: the same surface on SeiNetwork validators. The `config-substrate-parity-seinetwork` work item owns that.
- **Does not own**: genesis overrides, which write genesis.json chain state on a different lifecycle.
- **Does not own**: the seid config schema. seid owns which keys are valid.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - A new config key runs with no controller code (Priority: P1)

The team arrives at Barcelona with a new seid executable. The executable needs
one new config key, `EVM-only enable=true` in config.toml. The operator adds a
config value that names config.toml, the key, and the value. The node runs the
new executable, and the controller ships no new code for the key.

**Why this priority**: this story gates the Barcelona event. Without it, a new key
needs a controller change, and the team cannot swap the executable in time.

**Independent Test**: Add a config value that names config.toml and an arbitrary
key. Confirm that the node config file holds the key. Confirm that the node runs.

**Acceptance Scenarios**:

1. **Given** a node and a config value that sets `EVM-only enable=true` in config.toml, **When** the controller renders the node, **Then** the node config.toml holds that key.
2. **Given** the same config value and a controller with no register entry for the key, **When** the controller applies the overlay, **Then** the key reaches the file.

---

### User Story 2 - An overlay merges onto the controller's own config (Priority: P2)

An operator adds one config value to app.toml. The controller already writes
app.toml with its own defaults. The operator wants the controller to add the one
key and keep the rest. The overlay merges onto the base config, so the operator
key and the controller defaults both survive.

**Why this priority**: this ranks below Story 1 but above Story 3. A
replace-not-merge overlay would drop the controller defaults and break the node.
The merge is what keeps the surface safe to use.

**Independent Test**: Add a config value on a file the controller already writes.
Confirm that the merged file holds both the config value and the controller keys.

**Acceptance Scenarios**:

1. **Given** a controller-written app.toml and a config value that adds one key, **When** the controller merges the overlay, **Then** the merged file holds both keys.
2. **Given** a config value for a file the controller does not write, **When** the controller applies the overlay, **Then** the controller creates the file from the value.

---

### User Story 3 - A changed or removed value takes effect (Priority: P3)

An operator changes or removes a config value on a running node. The operator
wants the change to take effect. The controller recomputes the overlay, updates
the node config, and restarts seid, so the node reads the new config.

**Why this priority**: this ranks below Story 2. A change cannot take effect until
the overlay exists.

**Independent Test**: Remove a config value from a running node. Confirm that the
controller updates the config and restarts seid. Confirm that the key returns to
the base value.

**Acceptance Scenarios**:

1. **Given** a running node with a config value, **When** the operator removes the value, **Then** the controller recomputes the overlay without it.
2. **Given** the recomputed overlay, **When** the controller updates the config, **Then** the controller restarts seid.
3. **Given** the restarted node, **When** seid reads the config, **Then** the key holds the base value.

---

### User Story 4 - A guarded key is caught (Priority: P3)

An operator sets a config value for a guarded key by mistake, such as a freeze
height. The controller already blocks that key on the existing overrides. The
controller rejects the config value, so the operator does not wedge the node.

**Why this priority**: this ranks with Story 3 as a safety backstop, below the
core add, merge, and change path. It keeps a known landmine from returning through
the new field.

**Independent Test**: Set a config value for a guarded key. Confirm that the
controller rejects the config value.

**Acceptance Scenarios**:

1. **Given** a config value that names a guarded key, **When** the controller validates the node, **Then** the controller rejects the config value.

### Edge Cases

- What happens when a config value is malformed and seid refuses it at load? The controller reports the failed start — see Requirement 6, criterion 3.
- What happens when a config value names a key the controller derives? The config value wins — see Requirement 3.
- What happens when a config value names a guarded key, such as a freeze height? The controller rejects the value — see Requirement 6, criterion 2.

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: A config-value field that accepts any file and any key

**Objective:** As a node operator, I want a field of config values that name a
file, a key, and a value, so that a new seid key works without controller code.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL accept a field of config values on a SeiNode, beside the existing overrides.
2. THE controller SHALL read a file name, a key, and a value from each config value.
3. THE controller SHALL accept a config value whose file name and key are absent from the register.
4. THE controller SHALL preserve the type of a config value when it writes the overlay.

### Requirement 2: The overlay merges onto the base config

**Objective:** As a node operator, I want the controller to merge my config values
over the file it already writes, so that I add a key without losing the rest.

**Traces to:** User Story 2

#### Acceptance Criteria

1. WHEN the controller starts a node, THE controller SHALL merge the config values as an overlay over the base config.
2. WHEN the controller applies the overlay, IF a config value names a file the controller does not write, THEN THE controller SHALL create that file from the value.
3. WHEN the controller applies the overlay, IF a config value names a key the base config already holds, THEN THE controller SHALL replace that key with the config value.

### Requirement 3: A config value wins a collision

**Objective:** As a node operator, I want my config value to win over a
controller-derived value, so that the controller writes my value and not its
derived one.

**Traces to:** User Story 2

#### Acceptance Criteria

1. WHEN the controller applies the overlay, IF a config value names a key the controller also derives, THEN THE controller SHALL keep the config value.

### Requirement 4: A changed or removed value materializes

**Objective:** As a node operator, I want a changed or removed config value to take
effect, so that I do not need a manual restart.

**Traces to:** User Story 3

#### Acceptance Criteria

1. WHEN the operator adds, changes, or removes a config value, THE controller SHALL recompute the overlay over the base config.
2. WHEN the overlay changes, THE controller SHALL write the new overlay to the node config.
3. WHEN the controller writes a new overlay to a running node, THE controller SHALL restart the seid container.
4. WHEN the operator removes a config value, THE controller SHALL return that key to its base value.

### Requirement 5: The overlay sits in place before seid starts

**Objective:** As a node operator, I want the overlay in place before seid starts,
so that the node reads it on the first boot.

**Traces to:** User Story 1

#### Acceptance Criteria

1. WHEN the controller creates a node, THE controller SHALL write the overlay before seid starts.

### Requirement 6: The controller keeps the existing safety guards

**Objective:** As a node operator, I want the controller to keep the existing
guards, so that a dangerous known key is still caught.

**Traces to:** User Story 4

#### Acceptance Criteria

1. THE controller SHALL validate the existing override keys against the existing CEL guards.
2. IF a config value names a key the existing guards block, THEN THE controller SHALL reject that config value.
3. IF seid refuses the config, THEN THE controller SHALL report the failed start to the operator.

### Key Entities

- **Config value**: a file name, a key, and a value that the operator declares on a node.
- **Overlay**: the set of config values the controller merges over the base config.
- **Base config**: the config file the controller writes from its own defaults, before the overlay.

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: The merged config file on the node holds an arbitrary key that a config value set.
  *Verifier:* judgement — a platform engineer reads the merged file on the node and confirms the key.
- **SC-002**: An overlay onto an existing file keeps the other keys and adds the new one.
  *Verifier:* judgement — a platform engineer compares the merged file against the base config.
- **SC-003**: An `EVM-only` config value runs the node with no controller code change.
  *Verifier:* judgement — the release owner runs the Barcelona rehearsal image and confirms the node runs.
- **SC-004**: A removed config value updates the node config, restarts seid, and returns the key to the base value.
  *Verifier:* judgement — a platform engineer removes a config value and confirms the restart and the base value.
- **SC-005**: A colliding key resolves to the config value.
  *Verifier:* judgement — a platform engineer sets a key the controller also derives and confirms the config value wins.
- **SC-006**: A node reads the overlay on its first boot.
  *Verifier:* judgement — a platform engineer confirms the overlay sits in the config file before seid starts.
- **SC-007**: A config value that names a guarded key fails the existing guard.
  *Verifier:* judgement — a platform engineer sets a guarded key through a config value and confirms the controller rejects it.
- **SC-008**: A typed value survives into the config file.
  *Verifier:* judgement — a platform engineer sets a boolean value and confirms the file holds a boolean, not a string.
- **SC-009**: A config value for a file the controller does not write creates that file.
  *Verifier:* judgement — a platform engineer names a new file and confirms the controller creates it from the value.
- **SC-010**: A config value that makes seid refuse the config produces a reported failed start.
  *Verifier:* judgement — a platform engineer sets a bad value and confirms the controller reports the failed start.

## Assumptions

- The controller already carries the tomlpatch merge engine and the config-apply and restart-seid tasks. This spec extends the override surface and its routing; it does not add a merge engine.
- The config-value field sits beside the existing overrides, so the CEL guards and backward compatibility survive.
- The controller recomputes the overlay over the base config on every start and on every change. A removed config value therefore returns its key to the base value. There is no in-value delete marker.
- The operator owns the correctness of a config value the guards do not block. A wrong value fails at seid load, which the team accepts in exchange for full control.

## Out of scope

- The same substrate on SeiNetwork validators. That work lives in the `config-substrate-parity-seinetwork` work item.
- Genesis overrides, which write genesis.json chain state on a different lifecycle.
- The seid config schema. seid owns which keys are valid.
- The mechanism that recycles a config map on a content hash. The requirement is that a change takes effect through a seid restart; the controller picks the mechanism.
