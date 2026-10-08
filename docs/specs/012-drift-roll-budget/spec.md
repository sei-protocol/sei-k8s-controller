# Feature Specification: Drift-roll budget

**Feature Branch**: `012-drift-roll-budget`

**Created**: 2026-10-07

**Status**: Draft

**Tracking**: PLT-1399 (parent PLT-1389, atlantic-2 migration)

**Input**: the 2026-10-07 cell sidecar rollouts. Every drifted node in a cell rolled at once: arctic-1 paused about 30 seconds in each cell, and every pacific-1 RPC node in the `prod` cell restarted together.

A pod-template drift (a new seid image, a new cell sidecar image, an isolation change) makes every affected SeiNode build its update plan in the same reconcile. Each node decides alone, so nothing limits how many nodes in a namespace roll at once. This spec adds a per-namespace budget that the controller config sets.

## Glossary

- **Drift update**: the plan a Running node builds when `podTemplateDrifted` is true. `classifyPlan` labels it `node-update`.
- **Roll slot**: permission to start a drift update. A namespace has `max(1, N * percent / 100)` slots, where N counts its Running, unpaused SeiNodes.
- **Slot order**: nodes whose `NodeUpdateInProgress` is `True`, sorted by name, then drifted nodes waiting for a slot, sorted by name. A node holds a slot when its position in the order is below the slot count.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A cell sidecar bump rolls a share of each namespace at a time (Priority: P1)

An operator bumps the cell sidecar image with the budget at 25%. In each namespace, at most a quarter of the nodes (and at least one) roll at a time. The others wait and report why, then start as slots free.

**Independent Test**: set the budget on harbor, bump the cell sidecar, and confirm from the roll log that no namespace has more than its slot count of nodes with `NodeUpdateInProgress=True`.

**Acceptance Scenarios**:

1. **Given** a namespace with N Running nodes and budget B, **When** a drift lands on all of them, **Then** at most `max(1, N*B/100)` nodes update at once, and the rest report `NodeUpdateInProgress=False/UpdateDeferred` naming the slot holders.
2. **Given** a waiting node, **When** a node holding a slot finishes, **Then** the waiting node re-checks its slot at once, not on its next status poll (30 seconds).

### User Story 2 - The budget changes nothing until a cell sets it (Priority: P1)

**Independent Test**: with `rollout.driftUpdateBudgetPercent` unset, a drift rolls every node at once, as before.

## Requirements *(mandatory)*

### Requirement 1: The budget gates drift updates

#### Acceptance Criteria

1. WHERE `rollout.driftUpdateBudgetPercent` is above 0, THE controller SHALL build a drift update only for a node that holds a roll slot.
2. WHEN a drifted node holds no slot, THE controller SHALL build no plan for it and SHALL set `NodeUpdateInProgress=False` with reason `UpdateDeferred` and a message that names the slot holders.
3. THE controller SHALL list the namespace's SeiNodes with an uncached read. Because the node controller reconciles one node at a time, each slot decision then sees every earlier node's persisted status, and the slots never overfill.
4. THE budget SHALL NOT gate a data reset, a hold change, a config update, an init plan, or a resize.
5. WHEN a node leaves `NodeUpdateInProgress=True`, changes `spec.paused`, changes phase, or is deleted, THE controller SHALL enqueue every node in its namespace that reports `UpdateDeferred`, so the next node in slot order does not wait for its status poll.

### Requirement 2: A ConfigMap-configured node keeps its running images while it waits

#### Acceptance Criteria

1. WHILE a `nodeConfig` node is drifted and holds no slot, THE controller SHALL render its StatefulSet from a copy pinned to the running image, sidecar image, and isolation, because that StatefulSet is `RollingUpdate` and the drifted template would roll the pod.
2. THE pinned render SHALL apply every other field, so `spec.paused`, a resize, and a config ref still take effect.
3. THE controller SHALL pin only when no plan is active, no reset or hold change is pending, and the node is not paused. `observe-image` stamps the spec image when a rollout completes, so a plan that ran on a pinned template would record an image the pod does not run.

### Requirement 3: The budget is controller config

#### Acceptance Criteria

1. THE controller SHALL read the budget from `rollout.driftUpdateBudgetPercent` in its config file. 0 or unset SHALL leave drift unpaced.
2. THE controller SHALL refuse to start with a value outside 0..100.

## Success Criteria *(mandatory)*

- **SC-001**: Unit tests cover the slot count and order, the exclusions, the plan gate, and the template gate; a reconciler test shows a waiting nodeConfig node keeps its StatefulSet template. *Verifier:* `make test`.
- **SC-002**: A harbor sidecar bump with the budget at 25% never exceeds the slot count in any namespace. *Verifier:* judgement — the roll log in PLT-1399.
- **SC-003**: Unit tests cover the slot-release predicate and the peer mapping (Req 1.5). *Verifier:* `make test`.

## Known limits

- While a `nodeConfig` node runs another plan, or has a reset or hold change pending, the controller applies its drifted template. The pod then rolls with that plan, outside the budget, and `observe-image` records the new images, so no second roll follows.
- A drift update stuck in progress keeps its slot, so a namespace's further drift waits until an operator clears it. The `UpdateDeferred` message names the holder.
- The node controller reconciles one node at a time, so a woken node still waits behind the nodes queued before it. In the 2026-10-08 rollout, before Req 1.5, the median wait from a freed slot to the next start was 1 to 28 seconds per namespace, and the longest was 52 seconds in the 51-node prod cell.
- Cells still roll through separate pull requests; the budget is per namespace, not per chain across cells.

## Out of scope

- Pacing config updates (`restart-seid` on `configValues` drift), resets, holds, init plans, and resizes.
