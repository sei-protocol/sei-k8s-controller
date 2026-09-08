# Feature Specification: Selectable node resources for benchmarks

**Feature Branch**: `001-configurable-node-resources`

**Created**: 2026-09-08

**Status**: Draft

**Blocks**: the Barcelona benchmark event. The team named this a Barcelona
blocker, because a store that needs more cores cannot run on the harness today.

**Input**: The Benchmark Party transcript, 2026-09-04. The fence below holds the
originator's words. The writing rules govern this document, not its source.

```text
Make resources selectable. Prompt the user to choose the resources with a
default, the same way the harness prompts for topology and run shape. One of the
swappable parameters in benchmarking is exactly that: the type of disk, the
amount of CPU, and the amount of RAM. Today we cannot run the giga store on
standard validator hardware, because it needs far more cores to reach the latest
benchmark of nearly 200,000 transactions per second. For cost, the default
should not be as beefy as mainnet, because most runs are quick tests. This is a
blocker for Barcelona.
```

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach, because that gap is the honest part.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | resource field shape on the CRD | whether the controller reconciles it correctly |
| Google AIP | resource field naming and consistency | whether the resource model is the right one |

## Glossary

- **Node group**: the set of nodes of one role, validator or RPC, that share one resource shape.
- **Resource shape**: the CPU request, the memory request, and the storage size that every node in one node group receives.
- **Storage parameters**: the disk type, the IOPS, and the throughput of the volume a node mounts.
- **Profile default**: the resource shape the harness proposes when the operator states nothing.
- **Scenario**: one benchmark run with a fixed set of swappable parameters, which an engineer compares against another run.
- **Mainnet shape**: the resource shape a production validator receives today.
- **Harness**: the harbor tooling that prompts the operator and renders the CRD manifests.
- **Controller**: the sei-k8s-controller, which reconciles a CRD into child StatefulSets, volumes, and services.
- **CRD**: the SeiNetwork or SeiNode resource the operator declares and the controller reconciles.

## Boundary Context

- **Sits within**: the sei-k8s-controller CRD surface (SeiNetwork and SeiNode) and the harbor harness that renders those resources.
- **Owns**: the resource fields on each CRD, the harness prompt that fills them, and the profile defaults.
- **Does not own**: the correct resource values for a scenario. The operator decides those.
- **Does not own**: the mapping of a pod to a worker node. The `node-ec2-locality` work item owns that.
- **Does not own**: the capacity of the node group. The platform provisions that.

## User Scenarios & Testing *(mandatory)*

Order stories by priority. Each story stands as an independent test.

### User Story 1 - A heavy store needs more cores and a faster disk (Priority: P1)

An engineer benchmarks the giga store. The store needs more cores and a faster
disk than a standard validator. The harness prompts for the validator resource
shape and shows a default. The engineer raises the CPU, the memory, and the disk
throughput, then confirms. The rendered validators carry the raised shape.

**Why this priority**: this story gates the Barcelona event. Without it the giga
store cannot run on the harness, and the event compares stores.

**Independent Test**: Run the harness for a validator topology. Raise the CPU on
the prompt. Confirm that the rendered SeiNetwork carries the raised CPU request.

**Acceptance Scenarios**:

1. **Given** a validator topology and a giga-store profile, **When** the engineer raises the CPU request, **Then** every rendered validator carries that CPU request.
2. **Given** the same topology, **When** the engineer raises the disk throughput, **Then** every rendered validator volume carries that throughput.

---

### User Story 2 - A quick test accepts a light default (Priority: P2)

An engineer runs a quick test to confirm that a build starts. The engineer wants
a low cost, not a production shape. The harness proposes a default lighter than
the mainnet shape. The engineer accepts it and runs.

**Why this priority**: most runs are quick tests. A default at the mainnet shape
wastes money on every one of them.

**Independent Test**: Run the harness with no resource override. Confirm that the
rendered shape requests less than the mainnet shape on every dimension.

**Acceptance Scenarios**:

1. **Given** no resource override, **When** the harness renders the topology, **Then** the CPU, the memory, and the storage each request less than the mainnet shape.

---

### User Story 3 - Two scenarios differ only by disk (Priority: P2)

An engineer compares two runs that differ only in disk throughput. The engineer
sets one throughput for the first run and another throughput for the second run.
Each rendered topology differs only in that field.

**Why this priority**: a benchmark isolates one variable. A parameter the engineer
cannot set alone cannot be the isolated variable.

**Independent Test**: Render two topologies that differ only in disk throughput.
Compare the two manifests. Confirm that the throughput field is the only
difference.

**Acceptance Scenarios**:

1. **Given** two runs with one topology and two throughput values, **When** the harness renders both, **Then** the two manifests differ only in the throughput field.

### Edge Cases

- What happens when the requested shape exceeds the capacity of the node group?
- What happens when the requested IOPS or throughput exceeds the limit of the disk type?
- What happens when the operator overrides one node group and leaves the other on its default?

## Requirements *(mandatory)*

Each requirement carries its own acceptance criteria, so no requirement is an
orphan and no criterion floats free of a requirement.

### Requirement 1: Prompted resource selection with a default

**Objective:** As a benchmark engineer, I want a prompt for the resource shape
with a default, so that I set resources the way I set topology.

**Traces to:** User Story 1, User Story 2

#### Acceptance Criteria

1. WHEN the harness prepares a topology, THE harness SHALL prompt for the resource shape of each node group.
2. WHEN the operator states no shape, THE harness SHALL apply the profile default.
3. WHEN the operator states a shape, THE harness SHALL write that shape into the manifest of every node in the group.
4. THE harness SHALL present the resource prompt in the flow that already holds the topology prompt and the run-shape prompt.

### Requirement 2: Resources use the shape an operator already knows

**Objective:** As an operator who knows Kubernetes, I want to set node resources
in the field shape I already know, so that my knowledge transfers.

**Traces to:** User Story 1

#### Acceptance Criteria

1. THE harness SHALL accept the CPU request and the memory request in the field shape of a pod resource request.
2. WHEN the operator sets a storage size, THE harness SHALL accept it in the field shape of a volume claim request.
3. IF the operator sets a value the schema rejects, THEN THE controller SHALL refuse the change.
4. IF the operator sets a value the schema rejects, THEN THE controller SHALL name the rejected field.
5. THE resource surface SHALL be a typed CRD field that mirrors the pod resources tree, with requests, limits, and the volume claim.
6. THE controller SHALL stamp that field onto the child StatefulSet.

### Requirement 3: Selectable storage parameters

**Objective:** As a benchmark engineer, I want to select a supported storage type
and its fields per node group, so that I compare storage-bound scenarios.

**Traces to:** User Story 3

#### Acceptance Criteria

1. THE harness SHALL expose a fixed set of supported storage types.
2. WHEN the operator selects a supported storage type, THE harness SHALL accept the standard fields for that type, such as the IOPS and the throughput.
3. WHEN the operator sets a storage field, THE controller SHALL apply it to the volume of every node in the group.
4. IF the operator selects a storage type outside the supported set, THEN THE harness SHALL refuse the selection.
5. IF the operator selects a storage type outside the supported set, THEN THE harness SHALL name the supported set.
6. THE supported set SHALL hold the gp3 EBS volume type, with the IOPS and the throughput as configurable fields.

### Requirement 4: A default lighter than the mainnet shape

**Objective:** As a benchmark engineer who runs a quick test, I want a default
lighter than the mainnet shape, so that a routine run costs less.

**Traces to:** User Story 2

#### Acceptance Criteria

1. THE harness SHALL set a default whose CPU request, memory request, and storage size each sit at about one quarter of the mainnet shape.
2. THE harness SHALL let the operator raise the resource shape to the mainnet shape or above it.

### Requirement 5: An independent shape per node group

**Objective:** As a benchmark engineer, I want each node group to hold its own
shape, so that validators and RPC nodes can differ.

**Traces to:** User Story 3

#### Acceptance Criteria

1. THE harness SHALL hold a separate resource shape for each node group.
2. WHEN the operator overrides one group, THE harness SHALL leave every other group on its own shape.

### Requirement 6: The harness reports an oversize shape

**Objective:** As a benchmark engineer, I want the harness to report an oversize
shape, so that a pending pod does not read as slow.

**Traces to:** User Story 1

#### Acceptance Criteria

1. IF a requested shape exceeds the capacity of the node group, THEN THE harness SHALL report the pending pod and the reason.

### Key Entities

- **Resource shape**: the CPU request, the memory request, and the storage size of one node group.
- **Storage parameters**: the disk type, the IOPS, and the throughput of a node volume.

## Success Criteria *(mandatory)*

Every criterion names the command that checks it, or says `judgement` with the
role that decides.

- **SC-001**: The rendered CRD carries the CPU, the memory, and the storage the operator set.
  *Verifier:* judgement — the benchmark owner reads the rendered SeiNetwork with kubectl and confirms every validator child carries the set values.
- **SC-002**: A default run requests about one quarter of the mainnet shape on CPU, memory, and storage.
  *Verifier:* judgement — the benchmark owner compares the default render against one quarter of the mainnet shape on all three dimensions.
- **SC-003**: Two runs that differ only in disk throughput render two manifests that differ only in that field.
  *Verifier:* judgement — the benchmark owner compares the two rendered manifests and confirms the throughput field is the only difference.
- **SC-004**: An oversize shape produces a reported pending pod, not a silent wait.
  *Verifier:* judgement — a platform engineer requests a shape above node capacity and confirms the harness reports the pending pod.

## Assumptions

- The node group holds enough capacity for the default shape. Capacity and pod placement live in the `node-ec2-locality` work item.
- The controller already reconciles child StatefulSets and volumes from the CRD. This spec adds fields; it does not add a controller.
- This iteration exposes resources as a typed CRD field that mirrors the pod resources tree. The field plumbs values through and adds no new semantics, which the team accepts for the first controller iteration.
- The first iteration supports the gp3 EBS volume type, with configurable IOPS and throughput. The validators use a 125 throughput today. The storage-class review may add types or tune ranges before Barcelona. It raised 10,000 IOPS and 750 throughput as candidates to reconsider.
- The default shape suits a quick test, not a production comparison. The operator raises it for a production comparison.
- The team set the default at about one quarter of the mainnet shape as a safe first value. The team tunes it later from cost and run data.

## Out of scope

- The mapping of a pod to a worker node, and the anti-affinity that keeps validators apart. That work lives in the `node-ec2-locality` work item.
- The override of a config file inside a node. That work lives in the `config-override-substrate` work item.
- The correct resource values for a named scenario. The operator or the consuming team decides those.
