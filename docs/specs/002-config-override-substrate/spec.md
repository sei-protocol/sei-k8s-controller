# Feature Specification: Arbitrary config override substrate

**Feature Branch**: `002-config-override-substrate`

**Created**: 2026-09-08

**Status**: Draft

**Blocks**: the Barcelona benchmark event. This spec adds a field of config
values, each with a file name, a key, and a value. The controller merges those
values over the base config. The event needs a new seid key, and the team ships
no new controller code for it.

**Input**: the Benchmark Party transcript, 2026-09-04. Review on the pull request
refined it, including the merge mechanism. The fence below holds the originator's
words. The writing rules govern this document, not its source.

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
| Kubernetes API conventions | CRD field shape and placement | whether the controller reconciles correctly |
| Google AIP | consistency with the existing overrides field | whether the resource model is the right one |

## Glossary

- **Controller**: the sei-k8s-controller reconciler that renders and updates a SeiNode.
- **SeiNode**: the CRD for a single node. This spec adds the config-value field to it.
- **CRD schema**: the SeiNode schema that validates a config value before the controller reads it.
- **Config file**: a seid config file such as config.toml or app.toml.
- **Config value**: one override entry, with a file name, a key, and a value.
- **Existing overrides**: the current override map on the node. Each key is a dotted path. The config-value field sits beside this map.
- **Base config**: the config file the controller writes from its own defaults, before the overlay.
- **Overlay**: the set of config values the controller merges over the base config before seid starts.
- **Merge**: the key-by-key application of the overlay over a base config file, from the existing tomlpatch engine.
- **Config-apply**: the controller step that writes the overlay before seid starts.
- **Register**: the controller's map from a known config key to its file. A config value bypasses it, because the operator names the file.
- **Allow-list**: the sei-config rule that accepts only a known set of keys. The config-value path does not apply it.

## Boundary Context

- **Sits within**: the override path in the sei-k8s-controller, for a SeiNode — the existing overrides, the config-apply step, the restart-seid task, and the tomlpatch merge engine.
- **Owns**: the new config-value field on a SeiNode, and its merge as an overlay over the base config. It also owns the unvalidated write path that carries an arbitrary key to the file, and the materialization of a change.
- **Does not own**: the config-value field on the SeiNetwork, and its propagation to every validator child. The `config-substrate-parity-seinetwork` work item owns that.
- **Does not own**: genesis overrides, which write genesis.json chain state on a different lifecycle.
- **Does not own**: the seid config schema. seid owns which keys are valid.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - A new config key works with no new controller code (Priority: P1)

The team arrives at Barcelona with a new seid executable. The executable needs
one new config key, `EVM-only enable=true` in config.toml. The operator adds a
config value that names config.toml, the key, and the value. The node runs the
new executable, and the team ships no new controller code for the key.

**Why this priority**: this story gates the Barcelona event. Without it, a new key
needs a controller change, and the team cannot swap the executable in time.

**Independent Test**: Add a config value that names config.toml and an arbitrary
key. Confirm that the node config file holds the key. Confirm that the node runs.

**Acceptance Scenarios**:

1. **Given** a node and a config value that sets `EVM-only enable=true` in config.toml, **When** the controller applies the overlay, **Then** the node config.toml holds that key.
2. **Given** the same config value and a controller with no register entry for the key, **When** the controller applies the overlay, **Then** the key still reaches the file.

---

### User Story 2 - An overlay merges over the controller's own config (Priority: P2)

An operator adds one config value to app.toml. The controller already writes
app.toml with its own defaults. The operator wants the controller to add the one
key and keep the rest. The overlay merges over the base config, so the operator
key and the controller defaults both survive.

**Why this priority**: this ranks below Story 1 but above Story 3. A
replace-not-merge overlay would drop the controller defaults and break the node.
The merge keeps the surface safe to use.

**Independent Test**: Add a config value on a file the controller already writes.
Confirm that the merged file holds both the config value and the controller keys.

**Acceptance Scenarios**:

1. **Given** a controller-written app.toml and a config value that adds one key, **When** the controller merges the overlay, **Then** the merged file holds both keys.
2. **Given** a config value for a file the controller does not write, **When** the controller applies the overlay, **Then** the controller creates the file from the value.
3. **Given** a config value for a key the base config already holds, **When** the controller merges the overlay, **Then** the merged file holds the config value.
4. **Given** a config value for a key the controller derives, **When** the controller merges the overlay, **Then** the merged file holds the config value.

---

### User Story 3 - A changed or removed value takes effect (Priority: P3)

An operator changes or removes a config value on a running node. The operator
wants the change to take effect. The controller recomputes the overlay, updates
the node config, and restarts seid, so the node reads the new config.

**Why this priority**: this ranks below Story 2. A change cannot take effect until
the overlay exists.

**Independent Test**: Remove a config value from a running node. Confirm that the
controller updates the config. Confirm that the controller restarts seid.

**Acceptance Scenarios**:

1. **Given** a running node with a config value, **When** the operator removes the value, **Then** the controller recomputes the overlay without it.
2. **Given** the recomputed overlay, **When** the controller updates the config, **Then** the controller restarts seid.
3. **Given** the restarted node, **When** seid reads the config, **Then** the key holds the base value.

---

### User Story 4 - A wrong value is visible (Priority: P3)

An operator sets a config value that seid refuses at load. The operator is the
expert, so the controller does not block the value. The node fails to start, and
the controller reports the failed start, so the operator sees the failure and not
a silent stall.

**Why this priority**: this ranks with Story 3 as a safety net, below the core
add, merge, and change path. It keeps a bad value visible.

**Independent Test**: Set a config value that seid refuses. Confirm that the
controller reports the failed start.

**Acceptance Scenarios**:

1. **Given** a config value that seid refuses, **When** the node starts, **Then** the controller reports the failed start to the operator.

### Edge Cases

- What happens when a config value is malformed and seid refuses it at load? The controller reports the failed start — see Requirement 6, criterion 2.
- What happens when a config value omits its value? The CRD schema rejects it, so the merge never receives an empty value — see Requirement 1, criterion 3.
- What happens when two config values name the same file and the same key? The CRD schema rejects the pair, so the overlay has one value per key — see Requirement 1, criterion 4.
- What happens when a config value names a key the controller derives? The config value wins — see Requirement 3.
- What happens when a config value names a key the existing field's guards block, such as a freeze height? The controller applies it; the operator owns the result — see Requirement 6, criterion 1, and the Assumptions.

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: A config-value field that accepts any file and any key

**Objective:** As a node operator, I want a field of config values that name a
file, a key, and a value, so that a new seid key works with no new controller code.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL accept a field of config values on a SeiNode, beside the existing overrides.
2. THE controller SHALL read a file name, a key, and a value from each config value.
3. THE CRD schema SHALL reject a config value whose value is null or absent.
4. THE CRD schema SHALL reject two config values that name the same file and the same key.
5. THE controller SHALL treat the key as a dotted path into the file.
6. IF a section on the path is missing, THEN THE controller SHALL create that section.
7. THE controller SHALL apply a config value for any key, without a check against the sei-config allow-list.
8. THE controller SHALL apply a config value with no code change to sei-config, the controller, or the sidecar.
9. WHEN the controller writes the overlay, THE controller SHALL preserve the type of each config value.

### Requirement 2: The overlay merges over the base config

**Objective:** As a node operator, I want the controller to merge my config values
over the file it already writes, so that I add a key without losing the rest.

**Traces to:** User Story 2

#### Acceptance Criteria

Criteria 1 through 3 apply when the controller applies the overlay.

1. WHEN the controller applies the overlay, THE controller SHALL merge each config value over the base config, key by key.
2. IF a config value names a file the controller does not write, THEN THE controller SHALL create that file from the value.
3. IF a config value names a key the base config already holds, THEN THE controller SHALL replace that key with the config value.

### Requirement 3: A config value wins a collision

**Objective:** As a node operator, I want my config value to win over a
controller-derived value, so that the controller writes my value and not its
derived one.

**Traces to:** User Story 2

#### Acceptance Criteria

1. IF a config value names a key the controller also derives, THEN THE controller SHALL keep the config value.

### Requirement 4: A changed or removed value materializes

**Objective:** As a node operator, I want a changed or removed config value to take
effect, so that I do not need a manual restart.

**Traces to:** User Story 3

#### Acceptance Criteria

1. WHEN the operator adds, changes, or removes a config value, THE controller SHALL recompute the overlay over the base config. Recomputation requires an observed configuration baseline. Until the controller has observed one, the change is deferred and reported on the node rather than materialized, so that deploying the controller does not restart every node whose baseline is not yet observed.
2. WHEN the overlay changes, THE controller SHALL write the new overlay to the node config.
3. WHEN the controller writes a new overlay to a running node, THE controller SHALL restart the seid container.
4. WHEN the operator removes a config value for a file the controller generates, THE controller SHALL return that key to its base value. A file the controller does not generate has no base to return to, so a removed key persists in that file until the operator removes it. The config-value field documents this.

### Requirement 5: The overlay sits in place before seid starts

**Objective:** As a node operator, I want the overlay in place before seid starts,
so that the node reads it on the first boot.

**Traces to:** User Story 1

#### Acceptance Criteria

1. WHEN the controller creates a node, THE controller SHALL write the overlay before seid starts.

### Requirement 6: The controller reports a wrong value

**Objective:** As a node operator, I want the controller to report a wrong config
value, so that a bad value is visible and not silent.

**Traces to:** User Story 4

#### Acceptance Criteria

1. THE controller SHALL apply a config value for a key the existing field's guards block.
2. IF seid refuses the config, THEN THE controller SHALL report the failed start to the operator.

### Key Entities

- **Config value**: one entry the operator declares on a SeiNode. It belongs to exactly one config file.
- **Overlay**: every config value on one node, grouped by config file. It merges over one base config per file.
- **Base config**: what the controller writes before the overlay. The overlay merges over it.

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: The merged config file on the node holds an arbitrary key from a config value.
  *Verifier:* judgement — a platform engineer reads the merged file on the node and confirms the key.
- **SC-002**: An overlay over an existing file keeps the other keys, adds a new one, and replaces a key the base config already holds.
  *Verifier:* judgement — a platform engineer compares the merged file against the base config.
- **SC-003**: The node runs with an `EVM-only` config value and no new controller code.
  *Verifier:* judgement — the release owner runs the Barcelona rehearsal image and confirms the node runs.
- **SC-004**: A changed override takes effect on the running node.
  *Verifier:* judgement — a platform engineer changes an override and confirms seid re-reads the value.
- **SC-005**: A colliding key resolves to the config value.
  *Verifier:* judgement — a platform engineer sets a key the controller also derives and confirms the config value wins.
- **SC-006**: A node reads the overlay on its first boot.
  *Verifier:* judgement — a platform engineer confirms the overlay sits in the config file before seid starts.
- **SC-007**: A config value applies even for a key the existing field's guards block.
  *Verifier:* judgement — a platform engineer sets a key the existing override field blocks and confirms the controller applies the config value.
- **SC-008**: A typed value survives into the config file.
  *Verifier:* judgement — a platform engineer sets a boolean value and confirms the file holds a boolean, not a string.
- **SC-009**: A config value for a file the controller does not write creates that file.
  *Verifier:* judgement — a platform engineer names a new file and confirms the controller creates it from the value.
- **SC-010**: A config value that makes seid refuse the config produces a reported failed start.
  *Verifier:* judgement — a platform engineer sets a bad value and confirms the controller reports the failed start.
- **SC-011**: A dotted key writes into its section, and the controller creates a missing section.
  *Verifier:* judgement — a platform engineer sets a dotted key for a missing section and confirms the file holds the nested section.
- **SC-012**: A key that is not in the sei-config allow-list still reaches the file.
  *Verifier:* judgement — a platform engineer sets an unknown key and confirms it reaches the file.
- **SC-013**: A config value with no value never reaches the merge.
  *Verifier:* judgement — a platform engineer applies a config value with no value and confirms the CRD schema rejects it.
- **SC-014**: Two config values that name the same file and key never both apply.
  *Verifier:* judgement — a platform engineer sets two config values with the same file and key and confirms the CRD schema rejects them.

## Assumptions

- The controller already carries the tomlpatch merge engine and the config-apply and restart-seid tasks. This spec extends the override surface and its routing; it does not add a merge engine.
- The config-value field sits beside the existing overrides, so the existing field and its guards stay unchanged and backward compatibility survives.
- The sei-config package accepts only allow-listed keys today. sei-config, the controller, and the sidecar each pin and statically link the same sei-config version, so they share one allow-list rather than hand-maintained copies. This spec adds a path that needs no entry in it, so an arbitrary key reaches the file. seid decides at load whether the key is meaningful.
- The config-value path applies no allow-list and no denylist. A config value can set a key the existing field's freeze and halt guards block. The controller applies it. The operator accepts the outcome, including any conflict with the freeze height the controller sets itself.
- The owner signed off on this trade-off. The config-value path does not apply the freeze and halt guards that the existing field applies. The existing field keeps those guards.
- Requirement 6 reports a value seid refuses at load. It does not cover a valid but wrong value on a consensus key, such as a freeze or halt height. Such a value loads cleanly and can halt the node later, which is a liveness event and not a reported failed start.
- The existing field treats a freeze or halt height as create-only, set on the bootstrap plan. The config-value path is mutable, so it offers a mutable route to that key. The plan reconciles this, and the operator owns the result.
- The controller recomputes the overlay over the base config on every start, and on every change once it has observed a configuration baseline. Before that first observation a change is deferred and reported rather than materialized. A removed config value returns its key to the base value for the files the controller generates, because those are regenerated wholesale from the typed model. A file the controller does not generate has no base, so a removed key persists there.
- The value in a config value is a typed value, not a string, so its type survives the merge. The plan chooses the representation. A string-only field could not preserve a boolean or a number.
- The tomlpatch engine deletes a key when a patch gives that key a null value, as RFC 7386 merge-patch defines. The config-value path never sends a null: the CRD schema rejects a null or absent value, and a null nested inside a value is admitted by the schema but rejected when the plan is built. To stop overriding a key, the operator removes its config value, and the controller returns the key to its base value for the files it generates.
- The operator owns the correctness of a config value. A wrong value fails at seid load, which the team accepts in exchange for full control.

## Out of scope

- The config-value field on the SeiNetwork, and its propagation to every validator child. That work lives in the `config-substrate-parity-seinetwork` work item.
- Genesis overrides, which write genesis.json chain state on a different lifecycle.
- The seid config schema. seid owns which keys are valid.
- The mechanism that recycles a config map on a content hash. Requirement 4, criterion 3 holds the obligation: the controller restarts the seid container. The plan chooses the mechanism.
