# Feature Specification: Switch a running SeiNode to nodeConfig

**Feature Branch**: `013-nodeconfig-switch`

**Created**: 2026-10-08

**Status**: Draft. Temporary: revert after the arctic-1 migration.

**Tracking**: PLT-1410 (atlantic-2 migration)

**Input**: the arctic-1 fleet (47 prod SeiNodes) is controller-configured. Moving it to `spec.nodeConfig` by replacing every node would mean detaching each data PVC and importing it into a new SeiNode. An in-place switch keeps each node, its data PVC, and its key Secrets.

`spec.nodeConfig` is fixed at creation because a `nodeConfig` node needs a StatefulSet with `podManagementPolicy: Parallel`, and the API server refuses to change that field on an existing StatefulSet. This spec lets a running node gain `spec.nodeConfig`, and makes the controller recreate the StatefulSet to apply the new policy. It is temporary: after the arctic-1 migration, a revert restores the create-only rule and removes the switch.

## Glossary

- **Switch**: adding `spec.nodeConfig` to an existing SeiNode that was created without it.
- **Orphan delete**: deleting a StatefulSet with `propagationPolicy: Orphan`. Its pods keep running, and the garbage collector removes their owner reference.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An operator moves a running node to git-managed config (Priority: P1)

An operator commits the node's rendered `config.toml` and `app.toml` as a ConfigMap, then adds `spec.nodeConfig` to the SeiNode in the same repository. The node restarts once on the same data PVC and reads its config from the ConfigMap.

**Independent Test**: on harbor, switch a running controller-configured node. Its pod restarts once, `seid` resumes from its previous height with no state sync, and the node returns to `Running` and ready.

**Acceptance Scenarios**:

1. **Given** a running controller-configured SeiNode, **When** `spec.nodeConfig` is added, **Then** the API server accepts the edit.
2. **Given** a SeiNode with `spec.nodeConfig`, **When** the field is removed, **Then** the API server rejects the edit.
3. **Given** a switched node whose StatefulSet uses `OrderedReady`, **When** the controller syncs it, **Then** it deletes the StatefulSet with orphan propagation, waits while it terminates, and applies a `Parallel`, `RollingUpdate` StatefulSet that adopts the running pod.

## Requirements *(mandatory)*

### Requirement 1: A running node can switch to nodeConfig

#### Acceptance Criteria

1. THE CRD SHALL accept adding `spec.nodeConfig` to an existing SeiNode, and SHALL reject removing it.
2. WHEN the live StatefulSet's pod management policy differs from the rendered one, THE controller SHALL delete the StatefulSet with orphan propagation and SHALL apply no StatefulSet in that reconcile.
3. WHEN the live and rendered policies are equal, THE controller SHALL NOT delete the StatefulSet. An empty rendered policy SHALL compare equal to the API default `OrderedReady`.
4. WHILE the StatefulSet the node would use is terminating, THE controller SHALL apply no StatefulSet.

## Success Criteria *(mandatory)*

- **SC-001**: Unit tests cover the orphan delete, the unchanged policy, and the wait on a terminating StatefulSet. *Verifier:* `make test`.
- **SC-002**: An envtest shows, against a real API server, that the in-place policy change is refused and that the switch completes with a `Parallel` StatefulSet. *Verifier:* `make test-integration`.
- **SC-003**: A harbor node switches with one pod restart and no state sync. *Verifier:* judgement — the pilot record on PLT-1410.

## Known limits

- The data PVC belongs to the SeiNode, not the StatefulSet, so the switch keeps it. The StatefulSet replaces the pod once: it deletes the old pod before it creates the new one, so two pods never run with one key.
- A switch edit must also remove `spec.peers`, `spec.configValues`, `spec.overrides`, `spec.externalAddress`, and a state-sync snapshot source, which `nodeConfig` forbids. The ConfigMap carries their rendered values.
- The drift-roll budget (spec 012) does not pace a switch. The merge order sets the pace.

## Out of scope

- Keeping the switch after the arctic-1 migration. The revert restores `has(self.nodeConfig) == has(oldSelf.nodeConfig)`.
- Removing `spec.nodeConfig` from a node.
