# Feature Specification: Config-value parity on the validator network

**Feature Branch**: `003-config-substrate-parity-seinetwork`

**Created**: 2026-09-08

**Status**: Draft

**Blocks**: the Barcelona benchmark event. The benchmark runs load against the
validators, so the validators need the same arbitrary config the RPC nodes get.
This spec puts the config-value field on the SeiNetwork and sends it to every
validator child.

**Input**: the Benchmark Party transcript, 2026-09-04. The fence below holds the
originator's words. The writing rules govern this document, not its source.

```text
These two should just operate the same. The config substrate needs to be
homogeneous. Whatever we do for the RPC overrides, do the same for the network,
so I can change the config inside the network the same way. This is also a
blocker for Barcelona, because folks will want to change the validator config.
```

This spec depends on the `config-override-substrate` work item, which defines the
config-value substrate for a SeiNode. This spec adds the same field to the
SeiNetwork and sends it to every validator child. It does not redefine the
substrate.

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach, because that gap is the honest part.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | CRD field shape and placement | whether the controller reconciles correctly |
| Google AIP | resource field naming and consistency | whether the resource model is the right one |

## Glossary

- **SeiNetwork**: the CRD that bootstraps a chain and owns a pool of validators. Every child is a validator.
- **Controller**: the sei-k8s-controller reconciler that creates and updates the SeiNetwork's validator children.
- **Validator child**: a SeiNode the SeiNetwork creates and owns, in validator mode.
- **SeiNode**: the CRD for a single node. The `config-override-substrate` work item adds the config-value field to it.
- **RPC node**: a SeiNode in RPC mode. The config-value field reaches it through the `config-override-substrate` work item.
- **Config value**: one override entry, with a file name, a key, and a value. The `config-override-substrate` work item defines it.
- **Config-value substrate**: the merge, overlay, and restart behavior the `config-override-substrate` work item defines for a SeiNode.
- **Overlay**: the merged set of config values a validator child's config file holds. The `config-override-substrate` work item defines it.
- **Existing config overrides**: the current dotted-key override map on the SeiNetwork, beside which the config-value field sits.
- **Allow-list**: the sei-config rule that accepts only a known set of keys. The config-value path does not apply it.
- **Key mapping**: the controller's map from a known config key to its file. A config value bypasses it, because the operator names the file.

## Boundary Context

In this section, **Does not own** names a behavior this spec does not define. The
controller may still perform it.

- **Sits within**: the SeiNetwork CRD and the controller that creates its validator children.
- **Owns**: the config-value field on the SeiNetwork, and its propagation to every validator child.
- **Owns**: the network's verdict on its own set — whether the values build an overlay at all, and how that verdict reaches the operator. The substrate defines what a buildable set is; this spec decides the network answers the question once, before the set fans out.
- **Does not own**: the config-value substrate — the merge, the overlay, the unvalidated path, and the restart behavior. The `config-override-substrate` work item defines that substrate. This spec starts it on each validator child.
- **Does not own**: genesis overrides, which write genesis.json chain state on a different lifecycle.
- **Does not own**: the SeiNode field itself. The `config-override-substrate` work item owns it.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - Fleet-wide config for a validator benchmark (Priority: P1)

The team benchmarks load against the validators. The operator needs one config
key across the whole validator set, such as `EVM-only enable=true` in config.toml.
The operator adds a config value on the SeiNetwork. Every validator child gets the
key, and the controller needs no new code.

**Why this priority**: this story gates the Barcelona event. The benchmark runs on
the validators, so the validators need the arbitrary config the RPC nodes take.

**Independent Test**: Add a config value on a SeiNetwork. Confirm that every
validator child holds the key in its config file.

**Acceptance Scenarios**:

1. **Given** a SeiNetwork with three validators and a config value that sets a config.toml key, **When** the controller reconciles, **Then** all three validator children hold the key.
2. **Given** the same config value and a controller with no key mapping for it, **When** the controller reconciles, **Then** the key still reaches every child.

---

### User Story 2 - The network field matches the node field (Priority: P2)

An operator configures a validator network and an RPC node in one session. The
operator wants one field shape for both, not two. The SeiNetwork config-value
field carries the same name and shape as the SeiNode field.

**Why this priority**: this ranks below Story 1. A second shape for the same job
forces the operator to learn the surface twice.

**Independent Test**: Compare the SeiNetwork config-value field against the SeiNode
field. Confirm that the name and the shape match.

**Acceptance Scenarios**:

1. **Given** the SeiNetwork field and the SeiNode field, **When** the operator compares the two, **Then** the name and the shape match.

---

### User Story 3 - A network change reaches every validator (Priority: P3)

An operator changes or removes a config value on a running SeiNetwork. The
operator wants the change on every validator. The controller copies the change to
every validator child. Each child updates its config and restarts seid through the
config-value substrate, so the whole set reads the new config.

**Why this priority**: this ranks below Story 2. A change reaches a validator only
after the field exists and the controller propagates it.

**Independent Test**: Change a config value on a SeiNetwork, then remove another.
Confirm that every validator child updates and restarts seid.

**Acceptance Scenarios**:

1. **Given** a running SeiNetwork with a config value, **When** the operator changes the value, **Then** the controller copies the change to every validator child.
2. **Given** the updated children, **When** each child's config values change, **Then** the controller restarts each validator's seid container.

### Edge Cases

- What happens when an operator edits one validator child's config value directly? The controller reconciles it back to the network value — see Requirement 2, criterion 3.
- What happens when a network config value names a key the existing config overrides guard, such as a freeze height? The controller applies it, the same as a SeiNode. The `config-override-substrate` work item owns this behavior. See the Assumptions.
- What happens when an operator adds a validator after setting a network config value? The controller copies the network values into the new child — see Requirement 2, criterion 1.
- What happens when the CRD admits a set the overlay cannot build — a null nested in a table, a number no TOML number type holds, two config values whose dotted keys overlap under one file? The schema checks shape, not representability, so the set arrives intact and fails the same way on every validator at once, holding up their unrelated updates too. The controller rejects it on the network instead — see Requirement 5.

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: A config-value field on the SeiNetwork that matches the SeiNode field

**Objective:** As a node operator, I want the SeiNetwork to carry the same
config-value field as the SeiNode, so that I learn one surface.

**Traces to:** User Story 2

#### Acceptance Criteria

1. THE SeiNetwork CRD SHALL hold a config-value field with the same name as the SeiNode field.
2. THE SeiNetwork CRD SHALL hold a config-value field with the same shape as the SeiNode field.
3. THE SeiNetwork CRD SHALL hold the config-value field beside the existing config overrides.

### Requirement 2: Propagation to every validator child

**Objective:** As a node operator, I want a network config value on every
validator child, so that one entry configures the whole set.

**Traces to:** User Story 1

#### Acceptance Criteria

1. WHEN the controller creates a validator child, THE controller SHALL copy the network config values into that child.
2. WHEN the network config values change, THE controller SHALL copy the new values into every validator child.
3. WHEN an operator edits a validator child's config values directly, THE controller SHALL reconcile them back to the network values.

### Requirement 3: The controller applies a network config value through the same substrate

**Objective:** As a node operator, I want a validator to take any config key with
no new controller code, so that a new seid key works at the benchmark.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE controller SHALL pass a network config value to a validator child through the same config-value substrate a SeiNode uses.
2. THE controller SHALL apply a network config value for any key, with no allow-list check, as the `config-override-substrate` work item defines.
3. THE controller SHALL apply a network config value with no code change to sei-config, the controller, or the sidecar, as the `config-override-substrate` work item defines.

### Requirement 4: A changed or removed value takes effect across the set

**Objective:** As a node operator, I want a changed or removed network config value
to reach every validator, so that every validator child holds the same config values.

**Traces to:** User Story 3

#### Acceptance Criteria

1. WHEN the operator changes or removes a network config value, THE controller SHALL write the new set of values to every validator child.
2. WHEN a validator child's config values change, THE controller SHALL restart that child's seid container, as the `config-override-substrate` work item defines.

### Requirement 5: The network rejects a set the substrate cannot build

**Objective:** As a node operator, I want a set the substrate cannot apply to
fail on the object I edited, so that I read one error instead of finding the
same error N times across the validator set.

**Traces to:** User Story 1, User Story 3

#### Acceptance Criteria

1. WHEN the network config values do not build an overlay, THE controller SHALL report the failure on the SeiNetwork, carrying the reason the operator has to act on.
2. WHEN the network config values do not build an overlay, THE controller SHALL NOT write them to any validator child, and each child SHALL keep the values it already holds.
3. WHEN the network config values do not build an overlay, THE controller SHALL continue to propagate the network's other fields to every validator child.
4. WHEN the operator corrects the network config values, THE controller SHALL clear the failure and write the corrected set to every validator child.

### Key Entities

- **Network config value**: a config value the operator declares on the SeiNetwork, which the controller copies to every validator child.

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: A config value on the SeiNetwork reaches every validator child's config file.
  *Verifier:* judgement — a platform engineer reads each validator's merged file and confirms the key.
- **SC-002**: The SeiNetwork config-value field carries the same name and shape as the SeiNode field.
  *Verifier:* judgement — a platform engineer compares the two CRD fields and confirms the match.
- **SC-003**: The validators take a network config value with no new controller code, on the same path a SeiNode uses.
  *Verifier:* judgement — the release owner sets a network config value and confirms the validators take it with no new code, through the same path a SeiNode uses.
- **SC-004**: A changed network config value updates every validator child and restarts each seid.
  *Verifier:* judgement — a platform engineer changes a network config value and confirms the update and the restart on every child.
- **SC-005**: A direct edit to a child's config value returns to the network value.
  *Verifier:* judgement — a platform engineer edits a child directly and confirms the controller reconciles it back.
- **SC-006**: A network config value for any key applies with no allow-list check.
  *Verifier:* judgement — a platform engineer sets an unknown key on the network and confirms it reaches every child.
- **SC-007**: The SeiNetwork carries the new config-value field beside the existing config overrides.
  *Verifier:* judgement — a platform engineer reads the SeiNetwork CRD and confirms both fields exist.
- **SC-008**: A removed network config value updates every validator child and returns the key to its base value.
  *Verifier:* judgement — a platform engineer removes a network config value and confirms the base value on every child.
- **SC-009**: A set the substrate cannot build fails on the SeiNetwork, reaches no validator child, and holds up none of the network's other propagated fields.
  *Verifier:* judgement — a platform engineer sets two overlapping dotted keys under one file, reads the failure on the SeiNetwork, and confirms every child keeps its previous set and still takes an image change.

## Assumptions

- The `config-override-substrate` work item defines the config-value substrate for a SeiNode. This spec adds the field to the SeiNetwork and propagates it; it does not redefine the substrate.
- The SeiNetwork already clones its config into each validator child today, from the existing config overrides (the re-sync in nodes.go). This spec follows the same propagation for the config-value field, which the `config-override-substrate` work item adds to the SeiNode.
- Every SeiNetwork child is a validator, so a network config value applies to the whole validator set.
- The network config values are authoritative for the children. A direct child edit reconciles back to the network value.
- A network config value can set a key the existing config overrides guard, the same as a SeiNode. The controller applies it, and the operator owns the result.
- CRD admission is a shape check, not a representability check. A set can satisfy the schema and still fail the substrate's overlay build, so the network runs that build itself before it copies anything. Requirement 5 asks the substrate's own question earlier; it does not define a second notion of a valid value.

## Out of scope

- The config-value substrate — the merge, the overlay, the unvalidated path, and the restart. The `config-override-substrate` work item owns it.
- Rejecting a set at admission. The network's verdict is a reported one: the write succeeds, and the failure is readable on the object.
- Genesis overrides, which write genesis.json chain state on a different lifecycle.
- A per-validator config value that differs across the set. This spec sends one config to the whole set.
- A rename of the existing config-override fields. This spec makes the new field homogeneous and leaves the old fields as they are.
