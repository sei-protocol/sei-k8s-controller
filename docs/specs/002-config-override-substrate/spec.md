# Feature Specification: Arbitrary config override substrate

**Feature Branch**: `002-config-override-substrate`

**Created**: 2026-09-08

**Status**: Draft

**Blocks**: the Barcelona benchmark event. This spec makes an override key a
config file name with an arbitrary body, merged onto the controller's own file.
A new seid key then works without new controller code, which the event needs.

**Input**: The Benchmark Party transcript, 2026-09-04. The fence below holds the
originator's words. The writing rules govern this document, not its source.

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

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach, because that gap is the honest part.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | CRD field shape, status, conditions | whether the controller reconciles correctly |
| controller-runtime | reconcile loop, client cache, owner refs | idempotence of a specific reconcile |

## Glossary

- **Config file**: a seid config file such as config.toml or app.toml.
- **Override substrate**: the CRD surface that carries an operator config override.
- **File-scoped override**: an override that uses a config file name as its key, with an arbitrary body.
- **Body**: the content the operator supplies under a file name, with typed values.
- **Merge-patch**: the key-by-key merge of an override body onto a file, from the existing tomlpatch engine.
- **Config-patch**: the controller task that patches a config file on a running node.
- **Config-apply**: the controller step that applies overrides before seid starts.
- **Materialize**: make a config change take effect on the node.
- **Register**: the controller's current map from a known config key to its file, which a file-scoped override bypasses.

## Boundary Context

- **Sits within**: the override path in the sei-k8s-controller, for a SeiNode — `spec.overrides`, the config-apply and config-patch tasks, and the tomlpatch merge engine.
- **Owns**: the file-scoped arbitrary override surface on a SeiNode, its merge onto the existing file, and the materialization of a change.
- **Does not own**: the same surface on SeiNetwork validators. The `config-substrate-parity-seinetwork` work item owns that.
- **Does not own**: genesis overrides, which write genesis.json chain state on a different lifecycle.
- **Does not own**: the seid config schema. seid owns which keys are valid.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - A new config key runs with no controller code (Priority: P1)

The team arrives at Barcelona with a new seid executable. The executable needs
one new config key, `EVM-only enable=true` in config.toml. The operator sets that
key through a file-scoped override on the node. The node runs the new executable,
and the controller ships no new Go code for the key.

**Why this priority**: this story gates the Barcelona event. Without it, a new key
needs a controller change, and the team cannot swap the executable in time.

**Independent Test**: Set an arbitrary config.toml key through an override on a
node. Confirm that the node config file holds the key. Confirm that the node runs.

**Acceptance Scenarios**:

1. **Given** a node and an override that sets `EVM-only enable=true` in config.toml, **When** the controller renders the node, **Then** the node config.toml holds that key.
2. **Given** the same override, **When** the controller ships no new key mapping, **Then** the key still reaches the file.

---

### User Story 2 - An override merges onto the controller's own config (Priority: P2)

An operator adds one key to app.toml. The controller already writes app.toml with
its own defaults. The operator wants the controller to add the one key and keep
the rest. The override merges onto the controller file, so the operator key and
the controller defaults both survive.

**Why this priority**: this ranks below Story 1 but above Story 3. A
replace-not-merge override would drop the controller defaults and break the node,
so merge is what makes the surface safe to use at all.

**Independent Test**: Set an override on a file the controller already writes.
Confirm that the merged file holds both the override key and the controller keys.

**Acceptance Scenarios**:

1. **Given** a controller-written app.toml and an override that adds one key, **When** the controller merges the override, **Then** the merged file holds both keys.
2. **Given** an override for a file the controller does not write, **When** the controller applies it, **Then** the controller creates the file from the override.

---

### User Story 3 - A changed override takes effect (Priority: P3)

An operator changes an override on a running node. The operator wants the change
to take effect without a manual restart. The controller re-applies the merged
config and materializes the change on the node.

**Why this priority**: this ranks below Story 2. A change can only materialize
once the merge exists, so the surface must work before a change to it can take
effect.

**Independent Test**: Change an override on a running node. Confirm that seid
re-reads the changed value with no manual step.

**Acceptance Scenarios**:

1. **Given** a running node with an override, **When** the operator changes the override, **Then** the controller re-applies the merged config to the node.
2. **Given** the re-applied config, **When** the node continues, **Then** seid reads the changed value.

### Edge Cases

- What happens when the override body is malformed and seid refuses it at load? The controller reports the failed start — see Requirement 6, criterion 3.
- What happens when an override key collides with a key the controller derives? The override wins — see Requirement 3.
- What happens when an operator sets a guarded key, such as a freeze height, through an arbitrary body? The substrate does not re-validate it — see Requirement 6, criterion 2.

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: File-scoped, arbitrary overrides

**Objective:** As a node operator, I want to override any config file by name with
arbitrary content, so that a new seid key works without controller code.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE override substrate SHALL key each override by a config file name.
2. THE override substrate SHALL accept an arbitrary body under each file name.
3. THE override substrate SHALL accept a config key it does not hold in its register.
4. THE override substrate SHALL preserve the type of each value in the body.
5. THE override substrate SHALL arrive as [NEEDS CLARIFICATION: a new file-scoped field beside the existing dotted-key overrides, or an evolution of the existing field. A new field keeps the CEL guards and backward compatibility. Which one, and who decides?]

### Requirement 2: Merge onto the existing file

**Objective:** As a node operator, I want the controller to merge my override onto
the file it already writes, so that I add a key without losing the rest.

**Traces to:** User Story 2

#### Acceptance Criteria

1. WHEN the controller applies an override AND the named file exists, THE controller SHALL merge the override onto that file key by key.
2. WHEN the controller applies an override AND the named file does not exist, THE controller SHALL create the file from the override.
3. THE controller SHALL apply the same merge semantics as the existing config override path.
4. THE merge SHALL handle key removal as [NEEDS CLARIFICATION: can an operator remove a key from an existing file through an override body, or does the merge only add and replace? The source names JSON merge-patch, which removes on null.]

### Requirement 3: An operator override wins a collision

**Objective:** As a node operator, I want my override to win over a
controller-derived value, so that I keep the final say.

**Traces to:** User Story 2

#### Acceptance Criteria

1. WHEN an override key collides with a controller-derived key, THE controller SHALL keep the override value.

### Requirement 4: A changed override materializes

**Objective:** As a node operator, I want a changed override to take effect, so
that I do not need a manual restart.

**Traces to:** User Story 3

#### Acceptance Criteria

1. WHEN the operator changes an override, THE controller SHALL re-apply the merged config to the node.
2. WHEN the controller re-applies the merged config, THE controller SHALL make seid read the changed value with no manual operator step.

### Requirement 5: The merged config sits in place before seid starts

**Objective:** As a node operator, I want the merged config in place before seid
starts, so that the node reads it on the first boot.

**Traces to:** User Story 1

#### Acceptance Criteria

1. WHEN the controller creates a node, THE controller SHALL write the merged config before seid starts.
2. THE controller SHALL apply the override on the init path and on the running path.

### Requirement 6: The controller keeps the existing safety guards

**Objective:** As a node operator, I want the substrate to keep the existing
guards, so that a dangerous known key is still caught.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL continue to validate the known override keys against the existing CEL guards.
2. IF an operator sets a value through an arbitrary body, THEN THE controller SHALL NOT re-validate that value against the CEL guards.
3. IF a body makes seid refuse the config, THEN THE controller SHALL report the failed start to the operator.

### Key Entities

- **Override substrate**: the CRD surface that carries file-scoped overrides on a node.
- **File-scoped override**: a config file name and the arbitrary body under it.
- **Merged config**: the config file after the controller merges the override onto its own defaults.

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: The merged config file on the node holds an arbitrary key that an operator set through an override.
  *Verifier:* judgement — a platform engineer reads the merged file on the node and confirms the key.
- **SC-002**: An override onto an existing file keeps the other keys and adds the new one.
  *Verifier:* judgement — a platform engineer compares the merged file against the file before the override.
- **SC-003**: An `EVM-only` key set through an override runs the node with no controller code change.
  *Verifier:* judgement — the team runs the Barcelona rehearsal image and confirms the node runs.
- **SC-004**: A changed override takes effect on the running node.
  *Verifier:* judgement — a platform engineer changes an override and confirms seid re-reads the value.
- **SC-005**: A colliding key resolves to the operator value.
  *Verifier:* judgement — a platform engineer sets a key the controller also derives and confirms the operator value wins.
- **SC-006**: A node reads the merged config on its first boot.
  *Verifier:* judgement — a platform engineer confirms the merged config sits in the config file before seid starts.
- **SC-007**: A known guarded key still fails the existing guard.
  *Verifier:* judgement — a platform engineer sets a guarded key through the known override field and confirms the CEL guard rejects it.

## Assumptions

- The controller already carries the tomlpatch merge engine and the config-apply and config-patch tasks. This spec extends the override surface and its routing; it does not add a merge engine.
- The override substrate does not re-validate an arbitrary body against the CEL guards. A wrong or malformed body fails at seid load. The team accepts that risk in exchange for full control.

## Out of scope

- The same substrate on SeiNetwork validators. That work lives in the `config-substrate-parity-seinetwork` work item.
- Genesis overrides, which write genesis.json chain state on a different lifecycle.
- The seid config schema. seid owns which keys are valid.
- The mechanism that recycles a config map on a content hash. The requirement is that a change takes effect; the controller picks the mechanism, which is the running-path config-patch today.
