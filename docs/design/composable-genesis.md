# Composable Genesis Ceremony

## Status

Proposal — not yet scheduled for implementation.

## Problem

The current `SeiNodePool` genesis ceremony is a monolithic, centralized process:

1. A single Kubernetes Job (`{pool}-genesis`) runs an embedded shell script that
   sequentially calls `seid init` for every node, creates keys, gentx, collects
   gentx, patches genesis parameters, and distributes the final `genesis.json`.
2. Per-node prep Jobs copy each node's directory from a shared EFS PVC to the
   node's data PVC, rewrite peer addresses, and set validator-specific config.
3. SeiNodes are created with pre-populated PVCs. Their sidecars do almost nothing.

This approach has several drawbacks:

- **Rigid**: the entire ceremony is a single shell script. Adding a step (e.g.,
  oracle config, IBC relayer keys) means editing the script.
- **Sequential**: all N node identities are created in series within one Job pod.
- **EFS dependency**: a ReadWriteMany PVC (EFS) is required for the shared genesis
  artifact, adding infrastructure cost and operational complexity.
- **Not reusable**: the validator initialization logic is locked inside the pool's
  genesis Job. A standalone SeiNode joining an existing chain as a validator cannot
  reuse any of it.

## Proposal

Decompose the genesis ceremony into per-node sidecar tasks orchestrated by the
`SeiNodePool` controller. Each `SeiNode` independently bootstraps its own validator
identity. The pool controller coordinates the single fan-in step (genesis assembly)
that no individual node can perform alone.

### Architecture

```
  SeiNodePool Controller (orchestrator)
      │
      ├── Creates N SeiNodes with Validator config
      │
      ├── Watches SeiNode statuses for identity completion
      │
      ├── Triggers assembly when all identities are ready
      │   (lightweight Job or designated-node sidecar task)
      │
      └── Patches each SeiNode's Genesis.S3 to point to assembled genesis
          └── Each node's existing init plan machinery takes over
```

### Per-Node Init Plan (pool-created validator)

```
init-validator          → create keys, gentx, publish identity to S3
configure-genesis       → download assembled genesis.json from S3 (retries until available)
discover-peers          → resolve network peers
configure-state-sync    → (only if StateSync is set)
config-patch            → apply TOML config patches
mark-ready              → signal bootstrap complete
register-validator      → (only on existing chains) submit create-validator tx
```

### Per-Node Init Plan (standalone validator joining existing chain)

```
init-validator          → create keys (gentx not needed; genesis already exists)
configure-genesis       → download existing chain genesis from S3 (immediately available)
discover-peers          → resolve network peers
configure-state-sync    → sync to chain tip
config-patch            → apply TOML config patches
mark-ready              → signal bootstrap complete
register-validator      → submit create-validator tx on-chain
```

### S3 Ceremony Artifact Layout

Each node publishes its identity artifacts to a shared S3 prefix. The assembler
reads from this prefix to build genesis.

The ceremony prefix includes a unique identifier (the pool's UID or a user-supplied
value) so that multiple pools with the same `chainId` do not collide. For example,
two separate `arctic-1` test networks created at different times each get their own
isolated ceremony namespace.

```
s3://{bucket}/{chainId}/ceremony/{ceremonyId}/
├── node-0/
│   ├── node-id.json       {"node_id": "7ea2fc...", "address": "pool-0.pool.ns.svc:26656"}
│   ├── gentx.json         validator genesis transaction
│   └── account.json       {"address": "sei1abc...", "coins": ["10000000usei"]}
├── node-1/
│   ├── node-id.json
│   ├── gentx.json
│   └── account.json
├── ...
└── genesis.json           written by assembler after collect-gentxs
```

The `ceremonyId` is derived from the `SeiNodePool` resource UID by default, ensuring
uniqueness across pool recreations. Users can override it via the pool spec for
deterministic paths in CI/CD workflows.

### Determining Whether Gentx Is Needed

No explicit flag is required. The controller infers this from the existing spec:

```go
func needsGentx(node *SeiNode) bool {
    return node.Spec.Validator != nil &&
           node.Spec.Genesis.S3 == nil &&
           node.Spec.Genesis.PVC == nil
}
```

If there is no pre-existing genesis source, the node is bootstrapping a new network
and must produce a gentx for the assembler. If genesis is already available, the node
only needs its key material.

## CRD Changes

### SeiNodeSpec additions

```go
// Validator configures this node to initialize as a validator.
// When set, the sidecar generates key material and (for new chains)
// a genesis transaction during bootstrap.
// +optional
Validator *ValidatorConfig `json:"validator,omitempty"`
```

### New types

```go
type ValidatorConfig struct {
    // Moniker is the human-readable validator name.
    // Defaults to the SeiNode name if unset.
    // +optional
    Moniker string `json:"moniker,omitempty"`

    // StakeDenom is the token denomination for staking (e.g. "usei").
    StakeDenom string `json:"stakeDenom"`

    // StakeAmount is the self-delegation amount in the gentx (e.g. "1000000usei").
    StakeAmount string `json:"stakeAmount"`

    // Identity configures where this node publishes its validator artifacts.
    Identity ValidatorIdentityStore `json:"identity"`
}

type ValidatorIdentityStore struct {
    // S3 is the prefix where identity artifacts are published.
    // Each node writes to {prefix}/node-{name}/ under this location.
    S3 S3Location `json:"s3"`
}
```

## New Sidecar Tasks

### `init-validator`

Creates validator key material and, when the node is bootstrapping a new chain,
produces a gentx and publishes identity artifacts to S3.

**Inputs**: moniker, chainId, stakeDenom, stakeAmount, S3 prefix
**Actions**:
1. `seid init {moniker} --chain-id {chainId}`
2. `seid keys add validator`
3. If gentx is needed:
   - `seid add-genesis-account`
   - `seid gentx validator {stakeAmount}`
   - Upload `node-id.json`, `gentx.json`, `account.json` to S3

### `assemble-genesis` (assembler only)

Collects all published identities and produces the final genesis.json.

**Inputs**: S3 ceremony prefix, expected node count, genesis parameter patches
**Actions**:
1. Download all `node-{N}/` artifacts from S3
2. `seid add-genesis-account` for each node
3. Copy all gentx into `config/gentx/`
4. `seid collect-gentxs`
5. Apply genesis parameter patches (staking, oracle, gov, etc.)
6. Upload final `genesis.json` to the ceremony prefix root

### `register-validator` (post-sync, existing chains only)

Submits a `create-validator` transaction on-chain after the node is synced.

**Inputs**: key name, stakeAmount, commission params
**Actions**:
1. Wait for node to report as catching up = false
2. `seid tx staking create-validator ...`

## Prerequisite: Task Retry Policy

**The current controller marks the entire init plan as `Failed` when any task
execution fails.** This is the single-failure-kills-plan policy established early
in the design.

The composable genesis model depends on tasks that naturally retry until an external
precondition is met (e.g., `configure-genesis` retrying until the assembler uploads
`genesis.json`). This requires changing the failure policy.

### Proposed change

Add a retry semantic to the task plan. When a task fails, instead of immediately
marking the plan as `Failed`:

1. Reset the task status from `Failed` back to `Pending`.
2. Increment a retry counter on the task.
3. Requeue with a backoff interval.
4. Only mark the plan as `Failed` after exceeding a configurable retry limit
   (or use unlimited retries for specific task types).

The simplest initial approach: **retry all failed tasks indefinitely** with the
existing poll interval. A failed task goes back to `Pending`, gets resubmitted on
the next reconcile. This matches the user's intent that tasks like `configure-genesis`
should keep trying until the precondition (genesis file exists) is satisfied.

A future refinement could introduce per-task retry limits or distinguishable error
categories (retryable vs. permanent), but unlimited retry is the correct starting
behavior for the scenarios this design enables.

### Affected code

`failTask()` in `plan_execution.go` currently sets `task.Status = PlannedTaskFailed`
and `plan.Phase = TaskPlanFailed`. The change would instead set
`task.Status = PlannedTaskPending` (clearing the task ID and error) and requeue.

## SeiNodePool Redesign

The current `SeiNodePool` CRD and controller are entirely unused in production and
carry no established contracts. This design replaces the pool implementation
wholesale, reshaping it around the composable SeiNode and sidecar architecture.

### New SeiNodePool spec (sketch)

A `SeiNodePool` is a generic grouping of N identical `SeiNode` instances. It is
not inherently a "validator pool" — it can stamp out any type of node: snapshotters,
RPC full nodes, validators, etc. The `NodeTemplate` determines what kind of nodes
the pool creates.

The optional `Ceremony` field is what triggers the genesis ceremony flow. A pool of
5 snapshotters joining an existing chain would have no `Ceremony` at all — just a
`NodeTemplate` with `StateSync` and `SnapshotGeneration` configured. A pool of 10
validators bootstrapping a fresh test network would include `Ceremony` to orchestrate
identity generation and genesis assembly.

```go
type SeiNodePoolSpec struct {
    // ChainID is the chain identifier shared by all nodes in the pool.
    ChainID string `json:"chainId"`

    // NodeCount is the number of nodes in the pool.
    // +kubebuilder:validation:Minimum=1
    NodeCount int32 `json:"nodeCount"`

    // NodeTemplate is the full SeiNodeSpec used to generate each node.
    // The pool controller copies this into every SeiNode it creates,
    // overriding ChainID and injecting per-node values (name, ordinal,
    // PVC references). When Ceremony is set, the controller also injects
    // Validator config and defers Genesis.S3 until assembly completes.
    NodeTemplate SeiNodeSpec `json:"nodeTemplate"`

    // Ceremony configures a genesis ceremony for the pool. When set, the pool
    // controller orchestrates validator identity generation across all nodes
    // and triggers genesis assembly when all identities are ready.
    // When nil, nodes bootstrap independently using the sources already
    // configured in the NodeTemplate (e.g., StateSync, Genesis.S3).
    // +optional
    Ceremony *CeremonyConfig `json:"ceremony,omitempty"`

    // Storage controls PVC lifecycle for pool-managed nodes.
    // +optional
    Storage SeiNodeStorageConfig `json:"storage,omitempty"`
}

// SeiNodeSpec is used directly as the template — every field available to a
// standalone SeiNode is available to pool-created nodes. There is no parallel
// type to maintain. The pool controller overrides ChainID from the pool-level
// field and injects per-node values; everything else passes through as-is.

type CeremonyConfig struct {
    // Validator configures the validator identity for each node.
    // The pool controller sets ValidatorConfig.Identity on each SeiNode
    // using the ceremony S3 prefix and the node's ordinal.
    Validator ValidatorCeremonyConfig `json:"validator"`

    // S3 is the bucket and prefix used for the ceremony artifact exchange.
    // A unique ceremonyId is appended automatically (derived from pool UID).
    S3 S3Location `json:"s3"`

    // CeremonyID is an optional user-supplied identifier appended to the S3
    // prefix. When empty, the pool's resource UID is used. Useful for
    // deterministic paths in CI/CD.
    // +optional
    CeremonyID string `json:"ceremonyId,omitempty"`

    // GenesisParams configures chain-level genesis parameters applied during
    // assembly (staking denom, gov params, oracle config, etc.).
    // +optional
    GenesisParams *GenesisParams `json:"genesisParams,omitempty"`
}

type ValidatorCeremonyConfig struct {
    // StakeDenom is the token denomination for staking (e.g. "usei").
    StakeDenom string `json:"stakeDenom"`

    // StakeAmount is the self-delegation amount per validator (e.g. "1000000usei").
    StakeAmount string `json:"stakeAmount"`
}
```

### Controller reconciliation

The pool controller is a pure orchestrator with no data-plane responsibilities:

1. **Create**: stamp out N SeiNodes from `NodeTemplate`, injecting per-node values
   (name, ordinal, PVC names). If `Ceremony` is set, each node also gets a
   `Validator` config pointing to the ceremony S3 prefix, and no genesis source
   is set yet. If `Ceremony` is nil, nodes are created exactly as the template
   specifies and bootstrap independently (e.g., a pool of snapshotters with
   `StateSync` and `SnapshotGeneration` already configured).

2. **Watch** (ceremony only): observe child SeiNode statuses. Each node's init
   plan completes `init-validator` and then retries `configure-genesis` against
   S3 (which 404s until assembly is done).

3. **Assemble** (ceremony only): when all N nodes show `init-validator` as
   `Complete`, the pool controller triggers genesis assembly. This is a lightweight
   Job that runs `assemble-genesis` using the ceremony S3 prefix. It collects all
   identities, applies `GenesisParams` patches, and uploads the final `genesis.json`.

4. **Status**: aggregate child SeiNode statuses into pool-level status and
   conditions. This applies to all pools regardless of whether a ceremony is
   configured.

### What the current pool implementation becomes

The existing pool code (genesis Job, prep Jobs, EFS PVC, genesis script ConfigMap,
`reconcileDataPreparation`) is removed entirely. The embedded `generate.sh` script
is replaced by the `assemble-genesis` sidecar task / assembly Job. The prep Jobs
are unnecessary because each node owns its data PVC from creation and populates
it through its own sidecar tasks.

### How the pool detects identity completion

The SeiNode status already tracks per-task status in `InitPlan.Tasks`. The pool
controller checks whether `init-validator` is `Complete` for each of its child
SeiNodes. A dedicated status condition (e.g., `ValidatorIdentityReady`) could
make this more explicit and decouple the pool from init plan internals.

## Examples

### Pool of snapshotters (no ceremony)

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodePool
metadata:
  name: pacific-1-snapshotters
spec:
  chainId: pacific-1
  nodeCount: 5
  nodeTemplate:
    image: sei:latest
    entrypoint:
      command: ["seid"]
      args: ["start", "--home", "/sei"]
    genesis:
      chainId: pacific-1
      s3:
        uri: s3://sei-genesis/pacific-1/genesis.json
        region: us-east-2
    snapshotRestore:
      bucket:
        uri: s3://pacific-1-snapshots/state-sync/
      region: eu-central-1
    peers:
      sources:
        - ec2Tags:
            region: eu-central-1
            tags: { ChainIdentifier: pacific-1, Component: snapshotter }
    snapshotGeneration:
      interval: 2000
      keepRecent: 5
      destination:
        s3:
          bucket: sei-node-mvp
          prefix: snapshot/
          region: eu-central-1
```

No `ceremony` — each of the 5 nodes boots independently using snapshot restore,
state sync, and peer discovery. The pool controller just creates them and
aggregates status.

### Pool of validators bootstrapping a fresh chain (with ceremony)

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodePool
metadata:
  name: arctic-1-validators
spec:
  chainId: arctic-1
  nodeCount: 10
  nodeTemplate:
    image: sei:latest
    entrypoint:
      command: ["seid"]
      args: ["start", "--home", "/sei"]
  ceremony:
    validator:
      stakeDenom: usei
      stakeAmount: "10000000usei"
    s3:
      bucket: sei-ceremony
      prefix: arctic-1/
      region: us-east-2
```

The pool controller creates 10 SeiNodes, each with a `Validator` config and no
genesis source. Each node runs `init-validator`, publishes identity to S3, and
retries `configure-genesis` until the assembler completes. The pool triggers
assembly after all 10 identities are ready.

## What This Enables

- **Generic pooling**: `SeiNodePool` works for any node type — validators,
  snapshotters, RPC nodes — not just genesis ceremonies.
- **Parallel identity generation**: N nodes init concurrently instead of one Job
  doing N sequential `seid init` calls.
- **No EFS dependency**: nodes publish to S3; no ReadWriteMany storage needed.
- **No prep jobs**: each node owns its own data PVC from the start.
- **Reusable validator bootstrap**: the same `init-validator` task works for pool
  nodes and standalone validators joining an existing chain.
- **Extensible**: new ceremony steps (oracle config, IBC relayer keys, etc.) are
  just additional sidecar tasks in the plan, not edits to a shell script.
- **Post-sync hooks**: tasks like `register-validator` use the same plan/retry
  machinery. No new "hook" abstraction needed — a task that depends on an external
  condition simply retries until the condition is met.
