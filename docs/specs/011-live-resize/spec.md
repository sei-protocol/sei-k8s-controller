# Feature Specification: Resize CPU, memory, and disk on a live ConfigMap-configured node

**Feature Branch**: `011-live-resize`

**Created**: 2026-10-06

**Status**: Draft

**Tracking**: PLT-1391 (parent PLT-1389, atlantic-2 migration)

**Unblocks**: CPU, memory, and disk changes on atlantic-2 and pacific-1 nodes
without replacing the node. Today `spec.resources` and
`spec.dataVolume.storage.resources.requests.storage` are create-only, so a resize
means a new node and a data import. The FlatKV migration runbook also asks the
operator to "expand the volume before a node reaches the limit" while the
migration runs.

**Input**: PLT-1391 and the readiness review on PLT-1389.

The create-only rules give two reasons: the StatefulSet is `OnDelete`, and
`ensure-data-pvc` has no update path. The first is not true for a
ConfigMap-configured node. Its StatefulSet is `RollingUpdate`, so a change to the
pod template rolls the pod with no controller plan. The second is true, and this
spec adds the update path.

## Semantic Anchors

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| Kubernetes CEL validation rules (transition rules, `oldSelf`) | which changes the API server accepts | whether the cluster can satisfy the change |
| Kubernetes volume expansion (`allowVolumeExpansion`, PVC `status.capacity`, resize conditions) | how a PVC grows and how it reports progress | the cloud provider's limits on volume changes |

## Glossary

- **Controller**: the sei-k8s-controller SeiNode reconciler.
- **Operator**: a person who runs a node and merges the GitOps commit that changes it.
- **ConfigMap-configured node**: a SeiNode with `spec.nodeConfig` set.
- **Footprint**: the seid container's CPU and memory requests, from `spec.resources`.
- **Disk size**: `spec.dataVolume.storage.resources.requests.storage`.
- **Owned PVC**: the data PVC the controller created for the node. An imported PVC is not owned.
- **Grow**: raising the disk size above its previous value.

## Boundary Context

- **Sits within**: the SeiNode schema, the StatefulSet pod template, and the owned PVC.
- **Owns**: which footprint and disk-size changes the API server accepts on a ConfigMap-configured node, the PVC update, and the `DataVolumeResizeInProgress` condition.
- **Does not own**: node capacity. Karpenter and the NodePools own it.
- **Does not own**: an imported PVC. The operator owns its class and size, and `dataVolume.import` excludes `dataVolume.storage`.
- **Does not own**: the SeiNetwork schema. Its rules stay create-only.
- **Does not own**: a resize on a node without `spec.nodeConfig`. Its StatefulSet is `OnDelete`, which is later work.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Change CPU or memory with one merge (Priority: P1)

An operator changes `spec.resources` on a ConfigMap-configured node. The pod rolls
onto the new footprint, the same way it rolls for an image change.

**Why this priority**: today a footprint change means a new node and a data import.

**Independent Test**: raise the memory request of a harbor ConfigMap-configured
node and confirm the pod rolls onto the new value and the node catches up.

**Acceptance Scenarios**:

1. **Given** a running ConfigMap-configured node, **When** the operator changes the CPU or memory request, **Then** the API server accepts it and the pod rolls onto the new footprint.
2. **Given** a node without `spec.nodeConfig`, **When** the operator changes `spec.resources`, **Then** the API server rejects it.

---

### User Story 2 - Grow the disk with no restart (Priority: P1)

An operator raises the disk size on a ConfigMap-configured node. The controller
raises the owned PVC's request, and the volume grows with no pod roll.

**Why this priority**: the FlatKV migration keeps two state stores and fills
disks while it runs.

**Independent Test**: raise the disk size of a harbor ConfigMap-configured node
and confirm the PVC's capacity and the file system grow while seid runs.

**Acceptance Scenarios**:

1. **Given** a running ConfigMap-configured node, **When** the operator raises the disk size, **Then** the controller raises the owned PVC's request. The condition reads `True/Resizing` until the capacity reaches the request.
2. **Given** a ConfigMap-configured node, **When** the operator lowers the disk size, **Then** the API server rejects it.

### Edge Cases

- No NodePool can place the new footprint in the volume's availability zone: the new pod stays Pending, and the node is down. The runbook MUST tell the operator to provision capacity first for a validator.
- Several validators change footprint in one commit: they all roll at once. The runbook MUST limit one commit to the voting power the chain can lose.
- The storage provider refuses or delays the expansion, for example while a previous volume change completes: the condition stays `True/Resizing` and carries the PVC's message.
- A node created without a disk size uses the mode default. The operator adds a size to grow it. A size below the PVC's request changes nothing, because the controller never lowers a request.
- The operator raises the disk size again before the first growth completes: the controller raises the PVC request again, and the provider applies it when it can.
- The PVC's StorageClass does not allow expansion: the API server refuses the PVC update, and the condition reads `True/ResizeFailed` with that error.
- An imported PVC: the node has no disk-size field, so this spec does not apply. The runbook owns it.

## Requirements *(mandatory)*

### Requirement 1: The footprint is mutable on a ConfigMap-configured node

**Objective:** As an operator, I want to change CPU and memory in git, so that I do not replace a node to resize it.

**Traces to:** User Story 1

#### Acceptance Criteria

1. WHERE a SeiNode has `spec.nodeConfig`, THE API server SHALL accept a change to the CPU or memory request in `spec.resources`, up or down.
2. WHERE a SeiNode has no `spec.nodeConfig`, THE API server SHALL keep `spec.resources` create-only.
3. THE API server SHALL keep the existing value rules: positive values, no CPU limit, and a memory limit equal to the memory request.

### Requirement 2: The disk size can grow on a ConfigMap-configured node

**Objective:** As an operator, I want to grow the disk in git, so that a node does not fill its volume.

**Traces to:** User Story 2

#### Acceptance Criteria

1. WHERE a SeiNode has `spec.nodeConfig`, THE API server SHALL accept a disk size equal to or greater than the previous value.
2. WHERE a SeiNode has `spec.nodeConfig`, THE API server SHALL reject a disk size lower than the previous value.
3. WHERE a SeiNode has no `spec.nodeConfig`, THE API server SHALL keep the disk size create-only.
4. THE API server SHALL keep the disk size create-only on the SeiNetwork.
5. WHERE a SeiNode has `spec.nodeConfig`, THE API server SHALL accept an update that adds the disk size, and SHALL reject one that removes it.
6. WHERE a SeiNode has no `spec.nodeConfig`, THE API server SHALL keep rejecting an update that adds or removes the disk size.

### Requirement 3: The controller grows the owned PVC

**Objective:** As an operator, I want the volume to follow the spec, so that one merge grows the disk.

**Traces to:** User Story 2

#### Acceptance Criteria

1. WHILE the disk size is greater than the owned PVC's storage request, THE controller SHALL raise the request to the disk size.
2. THE controller SHALL NOT lower a PVC's storage request.
3. THE controller SHALL NOT change an imported PVC.

### Requirement 4: The controller reports the disk resize

**Objective:** As an operator, I want status that tells me where a resize is, so that I know when the space is usable.

**Traces to:** User Story 2

#### Acceptance Criteria

1. The controller SHALL seed a `DataVolumeResizeInProgress` condition on every SeiNode. `True` is the exception, `False` is the steady state.
2. WHILE the owned PVC's capacity is lower than the disk size, THE condition SHALL be `True` with reason `Resizing`. Its message SHALL carry the PVC's resize condition when one is present.
3. IF the PVC update fails, THEN THE condition SHALL be `True` with reason `ResizeFailed`, and its message SHALL carry the error.
4. WHILE the owned PVC's capacity is equal to or greater than the disk size, THE condition SHALL be `False` with reason `ResizeComplete`.
5. WHERE a SeiNode has no `spec.nodeConfig`, or uses an imported PVC, THE condition SHALL be `False` with reason `NotApplicable`.

### Key Entities

- **Footprint** (`spec.resources`): mutable on a ConfigMap-configured node. A change rolls the pod.
- **Disk size** (`spec.dataVolume.storage.resources.requests.storage`): grow-only on a ConfigMap-configured node.
- **`DataVolumeResizeInProgress` condition**: the disk resize signal, with reasons `Resizing`, `ResizeFailed`, `ResizeComplete`, `NotApplicable`.

## Success Criteria *(mandatory)*

- **SC-001**: The API server accepts a footprint change and a disk growth on a ConfigMap-configured node. It rejects a disk shrink, any footprint or disk change without `spec.nodeConfig`, and any disk change on the SeiNetwork.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the CEL envtest cases pass.
- **SC-002**: The controller raises the owned PVC's request to a grown disk size, never lowers it, and never touches an imported PVC.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the PVC growth tests pass.
- **SC-003**: The `DataVolumeResizeInProgress` condition reads `Resizing` while capacity is below the spec, `ResizeFailed` on a refused update, `ResizeComplete` after, and `NotApplicable` where the spec does not apply.
  *Verifier:* judgement — a reviewer runs `make test` and confirms the condition tests pass.
- **SC-004**: On a harbor ConfigMap-configured node, a memory change rolls the pod onto the new footprint, and a disk growth completes with no pod roll.
  *Verifier:* judgement — a platform engineer runs both changes on a harbor node and reports the pod restarts and the final capacity.

## Assumptions

- A footprint change reaches the pod through the StatefulSet's `RollingUpdate`. The new pod starts with the start gate closed, and the controller re-marks the sidecar ready as it does after any roll. The start guard from specs 009 and 010 still applies.
- The platform's `gp3` StorageClass sets `allowVolumeExpansion: true`, and the EBS CSI driver grows the volume and the file system online.
- A Pending validator has hours before downtime jailing. On 2026-10-06 both chains used a window of 108,000 blocks, a 5% minimum, and a 600 s jail with no downtime slash.

## Out of scope

- A capacity check before the roll. The runbook adds a manual provisioning step for validators.
- Tracking the cloud provider's limits on volume changes. The condition repeats the PVC's own message.
- Growing an imported PVC, including a static PV with no StorageClass. The runbook owns it.
- A `NodeUpdateInProgress` signal for a footprint roll.
- A change to the volume's performance class.
- A resize on a node without `spec.nodeConfig`.
