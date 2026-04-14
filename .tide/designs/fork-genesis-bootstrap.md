# Fork Genesis Bootstrap Design

## Summary

This design formalizes how forked networks bootstrap into their own genesis state using the per-node bootstrap Job pattern. The fork genesis ceremony already handles state export, identity generation, and genesis assembly at the group level. The gap is in how individual nodes initialize their PVCs from the fork genesis -- today this runs on the StatefulSet pod, which is problematic for large fork genesis files (gigabytes of exported state). This design moves fork genesis node initialization to per-node bootstrap Jobs so each SeiNode gets its own PVC populated independently.

## Motivation

### What works today

The fork genesis ceremony has two coordinated plan levels:

**Group-level plan** (`genesisGroupPlanner.BuildPlan` in `planner/group.go`):
1. `CreateExporter` -- temporary SeiNode with source chain binary + snapshot bootstrap
2. `AwaitExporterRunning` -- waits for exporter to reach Running
3. `SubmitExportState` -- submits `seid export --height N` to exporter sidecar, uploads `exported-state.json` to S3
4. `TeardownExporter` -- deletes exporter
5. `AssembleGenesisFork` -- sidecar downloads exported state, strips old validators, rewrites chain identity with new validator gentxs, runs `collect-gentxs`, uploads assembled `genesis.json` to S3
6. `CollectAndSetPeers` -- collects node IDs and sets `persistent_peers` on each child
7. `AwaitNodesRunning` -- waits for all nodes to reach Running

**Per-node plan** (`buildGenesisPlan` in `planner/bootstrap.go`):
1. `GenerateIdentity` -- creates validator keys on PVC
2. `GenerateGentx` -- creates gentx with new chain ID
3. `UploadGenesisArtifacts` -- uploads per-node artifacts to S3
4. `ConfigureGenesis` -- retries until group-level assembler uploads genesis.json to S3 (up to 180 retries at 10s intervals = ~30 min)
5. `ConfigApply` -- applies sei-config for validator mode
6. `SetGenesisPeers` -- configures peers from S3
7. `ConfigValidate` -- validates config
8. `MarkReady` -- sidecar flips `/v0/healthz` to 200, seid starts

### What's wrong

The per-node plan runs entirely on the StatefulSet pod. For a fork genesis, `genesis.json` contains the full exported state from the source chain. When seid starts with this genesis:

1. **Memory pressure.** `seid start` with a multi-gigabyte genesis file requires substantial RAM to parse and initialize state. The StatefulSet pod's resource limits may be tuned for steady-state operation, not genesis initialization. OOM kills during genesis init are hard to diagnose.

2. **Liveness probe failures.** The StatefulSet has liveness probes configured. Genesis initialization from exported state can take 30+ minutes on large chains. The liveness probe fires before seid is healthy, Kubernetes kills the pod, and the cycle repeats.

3. **No halt-height control.** The StatefulSet pod starts seid without `--halt-height`. For a fork, we need seid to process the genesis state and then halt so we can verify the state root before allowing the chain to produce blocks. Without halt-height, seid starts producing blocks immediately after genesis init, which is the wrong behavior for a coordinated fork launch.

4. **PVC multiplicity.** If we tried to solve this with a single group-level bootstrap Job, that Job can only mount ONE PVC. Each SeiNode needs its own PVC populated with initialized state. The bootstrap must happen per-node.

### Why bootstrap Jobs solve this

The per-node bootstrap Job pattern (already used for snapshot-based nodes) addresses all four problems:

- **Separate resource limits.** `bootstrapResourcesForMode()` configures Job pods with higher CPU/memory appropriate for initialization workloads.
- **No probes.** Jobs don't have liveness/readiness probes. The seid container runs until `--halt-height` is reached, however long that takes.
- **Halt-height.** The bootstrap Job runs seid with `--halt-height=N`, where N is `exportHeight + 1` (the first block the fork chain should produce). Seid processes the genesis state, reaches the halt height, and exits.
- **PVC-per-node.** Each SeiNode's bootstrap Job mounts that node's PVC (`data-{node-name}`). After the Job completes, the StatefulSet pod inherits the pre-populated PVC.

## How It Works

### Fork Genesis with Bootstrap Job Lifecycle

The fork genesis ceremony involves two coordinated levels. The per-node plans and the group plan execute concurrently -- the per-node plans generate identities and upload artifacts while the group plan exports source chain state. The `ConfigureGenesis` step on each node naturally blocks (with retries) until the group-level assembly completes.

**Per-node plan** (SeiNode controller, runs as bootstrap Job):

```
Phase 1: Bootstrap infrastructure
  DeployBootstrapService → DeployBootstrapJob

Phase 2: Sidecar tasks on bootstrap pod
  GenerateIdentity → GenerateGentx → UploadGenesisArtifacts →
  ConfigureGenesis (retries until group assembles) → ConfigApply →
  ConfigValidate → MarkReady

Phase 3: seid processes fork genesis
  (seid starts via /v0/healthz gate, processes genesis state, halts at exportHeight+1)
  AwaitBootstrapComplete → TeardownBootstrap

Phase 4: Post-bootstrap on StatefulSet pod
  ConfigureGenesis → ConfigApply → SetGenesisPeers →
  ConfigValidate → MarkReady
```

**Group-level plan** (SeiNodeDeployment controller, unchanged from today):

```
CreateExporter → AwaitExporterRunning → SubmitExportState →
TeardownExporter → AssembleGenesisFork → CollectAndSetPeers →
AwaitNodesRunning
```

### Key Design Decisions

#### 1. Genesis ceremony nodes route through bootstrap when fork is configured

Today, `validatorPlanner.BuildPlan` calls `buildGenesisPlan` for all genesis ceremony nodes. With this change, when the node's genesis ceremony is part of a fork, it routes through `buildBootstrapPlan` instead -- the same code path used by snapshot-based nodes.

The fork is detectable from the SeiNode spec: if `validator.genesisCeremony` is set AND the parent SeiNodeDeployment has `genesis.fork` configured, this is a fork genesis node. We propagate this signal by adding a `ForkExportHeight` field to `GenesisCeremonyNodeConfig` -- when non-zero, the node planner knows to use the bootstrap path.

```go
// In validatorPlanner.BuildPlan:
if isGenesisCeremonyNode(node) {
    if node.Spec.Validator.GenesisCeremony.ForkExportHeight > 0 {
        return buildForkGenesisPlan(node)
    }
    return buildGenesisPlan(node)
}
```

#### 2. Bootstrap Job halt-height is `exportHeight + 1`

For a fork genesis, seid needs to:
1. Initialize state from the fork genesis.json (this IS the exported state, rewritten with new identities)
2. Halt before producing any blocks on the new chain

The halt-height should be `exportHeight + 1` -- the first block height the new fork chain would produce. Cosmos SDK's halt-height sends SIGINT after committing the target block, so `exportHeight + 1` means seid processes the genesis state (which represents state at `exportHeight`), prepares for block `exportHeight + 1`, and halts.

This halt-height is already available on the `GenesisCeremonyNodeConfig` via the new `ForkExportHeight` field.

#### 3. Identity generation happens IN the bootstrap Job

The bootstrap Job's sidecar runs the genesis ceremony tasks:
- `GenerateIdentity` creates validator keys on the PVC
- `GenerateGentx` creates the gentx with the new chain ID
- `UploadGenesisArtifacts` uploads to S3

These MUST run on the bootstrap Job pod (not the StatefulSet pod) because:
- The identity keys need to be on the PVC BEFORE seid starts with the fork genesis
- The fork genesis assembler needs all nodes' artifacts uploaded before it can produce the final genesis.json
- The bootstrap Job's seid container waits for `/v0/healthz` (mark-ready) before starting, so all sidecar tasks complete first

This is the same sequencing the existing bootstrap plan uses -- the sidecar tasks run before mark-ready, mark-ready gates seid startup.

#### 4. Post-bootstrap phase re-applies config for the StatefulSet

After the bootstrap Job completes and is torn down, the StatefulSet pod starts. It needs to:
- Re-download genesis.json (the bootstrap pod already has it on the PVC, but the StatefulSet sidecar needs to verify/re-apply it)
- Apply config for the StatefulSet's runtime context (may differ from bootstrap)
- Set genesis peers (which may not have been available during bootstrap)
- Validate config
- Mark ready (seid starts and resumes from the halted state)

This is exactly what `buildPostBootstrapProgression` already provides, with the addition of `SetGenesisPeers`.

#### 5. No changes to the group-level plan

The group-level fork genesis plan is unchanged. It still:
1. Creates an exporter, exports source chain state, tears down exporter
2. Assembles fork genesis with new validator identities
3. Collects and sets peers
4. Awaits all nodes running

The only change is that each per-node plan now uses bootstrap Jobs, which means the `AwaitNodesRunning` step takes longer (bootstrap + StatefulSet startup instead of just StatefulSet startup).

## CRD Changes

### GenesisCeremonyNodeConfig (validator_types.go)

Add `ForkExportHeight` to signal that this node should use the bootstrap path:

```go
type GenesisCeremonyNodeConfig struct {
    // ... existing fields ...

    // ForkExportHeight is the block height at which the source chain state was
    // exported. When non-zero, the node planner uses the bootstrap Job path
    // instead of running genesis directly on the StatefulSet pod. The bootstrap
    // Job runs seid with --halt-height=ForkExportHeight+1 so the node initializes
    // state from the fork genesis and halts before producing blocks.
    // Set by the SeiNodeDeployment controller when genesis.fork is configured.
    // +optional
    ForkExportHeight int64 `json:"forkExportHeight,omitempty"`
}
```

This field is set by `generateSeiNode` in `nodedeployment/nodes.go` when the parent SeiNodeDeployment has `genesis.fork` configured:

```go
if gc := group.Spec.Genesis; gc != nil && spec.Validator != nil {
    // ... existing genesis ceremony config ...
    if gc.Fork != nil {
        spec.Validator.GenesisCeremony.ForkExportHeight = gc.Fork.ExportHeight
    }
}
```

No other CRD changes are needed. The bootstrap Job infrastructure (Service, Job, PVC naming) is already defined.

## Planner Changes

### New: `buildForkGenesisPlan` (planner/bootstrap.go)

A new plan builder that combines the genesis ceremony task progression with the bootstrap Job lifecycle:

```go
func buildForkGenesisPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
    gc := node.Spec.Validator.GenesisCeremony
    planID := uuid.New().String()
    planIndex := 0

    jobName := task.BootstrapJobName(node)
    serviceName := node.Name

    // Bootstrap sidecar tasks: genesis ceremony + config (no mark-ready yet --
    // buildBootstrapProgression excludes it, then we prepend genesis tasks)
    bootstrapProg := buildForkGenesisBootstrapProgression()
    postProg := buildForkGenesisPostBootstrapProgression()
    tasks := make([]seiv1alpha1.PlannedTask, 0, 2+len(bootstrapProg)+2+len(postProg))

    appendTask := func(taskType string, params any) error {
        t, err := buildPlannedTask(planID, taskType, planIndex, params)
        if err != nil {
            return err
        }
        tasks = append(tasks, t)
        planIndex++
        return nil
    }

    // Phase 1: Deploy bootstrap infrastructure
    appendTask(task.TaskTypeDeployBootstrapSvc,
        &task.DeployBootstrapServiceParams{ServiceName: serviceName, Namespace: node.Namespace})
    appendTask(task.TaskTypeDeployBootstrapJob,
        &task.DeployBootstrapJobParams{JobName: jobName, Namespace: node.Namespace})

    // Phase 2: Genesis ceremony + config on bootstrap pod
    for _, taskType := range bootstrapProg {
        appendTask(taskType, forkGenesisParamsForTaskType(node, gc, taskType))
    }

    // Phase 3: Await seid genesis init + halt, then tear down
    appendTask(task.TaskTypeAwaitBootstrapComplete,
        &task.AwaitBootstrapCompleteParams{JobName: jobName, Namespace: node.Namespace})
    appendTask(task.TaskTypeTeardownBootstrap,
        &task.TeardownBootstrapParams{JobName: jobName, ServiceName: serviceName, Namespace: node.Namespace})

    // Phase 4: Post-bootstrap config on StatefulSet pod
    for _, taskType := range postProg {
        appendTask(taskType, forkGenesisParamsForTaskType(node, gc, taskType))
    }

    return &seiv1alpha1.TaskPlan{ID: planID, Phase: seiv1alpha1.TaskPlanActive, Tasks: tasks}, nil
}

func buildForkGenesisBootstrapProgression() []string {
    return []string{
        TaskGenerateIdentity,
        TaskGenerateGentx,
        TaskUploadGenesisArtifacts,
        TaskConfigureGenesis,  // retries until group assembles fork genesis
        TaskConfigApply,
        TaskConfigValidate,
        TaskMarkReady,         // gates seid startup in bootstrap Job
    }
}

func buildForkGenesisPostBootstrapProgression() []string {
    return []string{
        TaskConfigureGenesis,
        TaskConfigApply,
        TaskSetGenesisPeers,
        TaskConfigValidate,
        TaskMarkReady,
    }
}
```

### validatorPlanner changes (planner/validator.go)

Route fork genesis ceremony nodes through the bootstrap path:

```go
func (p *validatorPlanner) BuildPlan(node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
    if isGenesisCeremonyNode(node) {
        if isForkGenesisCeremonyNode(node) {
            return buildForkGenesisPlan(node)
        }
        return buildGenesisPlan(node)
    }
    // ... existing snapshot/base plan logic ...
}

func isForkGenesisCeremonyNode(node *seiv1alpha1.SeiNode) bool {
    return node.Spec.Validator != nil &&
        node.Spec.Validator.GenesisCeremony != nil &&
        node.Spec.Validator.GenesisCeremony.ForkExportHeight > 0
}
```

## Bootstrap Job Changes

### Halt-height for fork genesis (task/bootstrap_resources.go)

`GenerateBootstrapJob` currently derives halt-height from `snap.S3.TargetHeight`. For fork genesis nodes, the halt-height comes from `GenesisCeremony.ForkExportHeight + 1`. The bootstrap Job generator needs to handle this case.

The cleanest approach: `GenerateBootstrapJob` already receives the `SeiNode` and `SnapshotSource`. For fork genesis, `SnapshotSource` is nil (no snapshot -- we're bootstrapping from genesis). We add a variant or extend the generator to accept halt-height directly:

```go
// GenerateBootstrapJobWithHaltHeight creates a bootstrap Job with an explicit halt-height.
// Used for fork genesis where halt-height comes from ForkExportHeight, not a snapshot.
func GenerateBootstrapJobWithHaltHeight(
    node *seiv1alpha1.SeiNode,
    haltHeight int64,
    platformCfg platform.Config,
) (*batchv1.Job, error) {
    // Same structure as GenerateBootstrapJob but uses the provided haltHeight
    // instead of snap.S3.TargetHeight
}
```

The `DeployBootstrapJob` task execution needs to detect fork genesis and call the appropriate generator. The fork export height is available on the SeiNode's `GenesisCeremony.ForkExportHeight` field.

### Bootstrap Job seid init for fork genesis

The existing `bootstrapSeidInitContainer` runs `seid init --chain-id`. For fork genesis, this is still correct -- seid init creates the base directory structure. The fork genesis.json is downloaded later by the `ConfigureGenesis` sidecar task, which overwrites the default genesis.json.

No changes needed to the init container.

## SeiNodeDeployment Controller Changes

### `generateSeiNode` (nodedeployment/nodes.go)

Propagate `ForkExportHeight` from the group's fork config to each child SeiNode:

```go
if gc := group.Spec.Genesis; gc != nil && spec.Validator != nil {
    // ... existing genesis ceremony config population ...
    if gc.Fork != nil {
        spec.Validator.GenesisCeremony.ForkExportHeight = gc.Fork.ExportHeight
    }
}
```

No other controller changes needed. The group-level plan is unchanged.

## Sequencing and Coordination

### Timeline of a fork genesis ceremony

```
Time ──────────────────────────────────────────────────────────►

Group plan:
  [CreateExporter]──[AwaitExporterRunning]──[SubmitExportState]──[TeardownExporter]──[AssembleGenesisFork]──[CollectSetPeers]──[AwaitNodesRunning]

Node-0 plan (bootstrap Job):
  [DeployBootstrapSvc]──[DeployBootstrapJob]──[GenerateIdentity]──[GenerateGentx]──[UploadArtifacts]──[ConfigureGenesis ↻ retries]──[ConfigApply]──[ConfigValidate]──[MarkReady]──(seid runs to halt-height)──[AwaitBootstrapComplete]──[TeardownBootstrap]──[ConfigureGenesis]──[ConfigApply]──[SetGenesisPeers]──[ConfigValidate]──[MarkReady]──(seid starts)

Node-1 plan (bootstrap Job):
  [DeployBootstrapSvc]──[DeployBootstrapJob]──[GenerateIdentity]──[GenerateGentx]──[UploadArtifacts]──[ConfigureGenesis ↻ retries]──[ConfigApply]──[ConfigValidate]──[MarkReady]──(seid runs to halt-height)──[AwaitBootstrapComplete]──[TeardownBootstrap]──[ConfigureGenesis]──[ConfigApply]──[SetGenesisPeers]──[ConfigValidate]──[MarkReady]──(seid starts)

Coordination points:
  ① Node plans block at ConfigureGenesis until group AssembleGenesisFork uploads genesis.json
  ② Group plan blocks at AwaitNodesRunning until all node StatefulSet pods reach Running
```

### Identity propagation correctness

The fork genesis assembly depends on each node's artifacts being uploaded to S3 before the assembler runs. The sequencing ensures this:

1. Per-node `GenerateIdentity` → `GenerateGentx` → `UploadGenesisArtifacts` runs on bootstrap Jobs
2. Per-node `ConfigureGenesis` polls S3 for the assembled genesis.json (retries)
3. Group `AssembleGenesisFork` runs on one node's sidecar -- it downloads all uploaded artifacts from S3 and combines them with the exported state
4. The assembled genesis.json includes the new validators' gentxs, replacing the old validator set

The S3 artifact paths are deterministic: `{chainID}/{nodeName}/gentx.json` and `{chainID}/{nodeName}/nodekey.json`. The assembler knows all node names from the plan params. This is unchanged from the current non-bootstrap flow.

## Failure Modes

### Bootstrap Job OOMKill during genesis init

The fork genesis.json can be large. If seid OOMs during initialization:
- The Job fails (backoffLimit=0)
- `AwaitBootstrapComplete` detects `JobFailed` condition
- The per-node plan fails
- The group plan's `AwaitNodesRunning` eventually times out

**Mitigation:** `bootstrapResourcesForMode` should be configurable via the SeiNodeDeployment spec for fork genesis. A fork from a chain with 50GB+ state needs substantially more memory than a standard validator bootstrap. Consider adding `genesis.fork.bootstrapResources` or using the archive resource tier for fork bootstraps.

### Assembler runs before all artifacts are uploaded

If the group plan's `AssembleGenesisFork` runs before all per-node plans have uploaded artifacts, the assembler produces an incomplete genesis. This is already handled: `AssembleGenesisFork` has `maxRetries=180` and retries until all expected node artifacts are present on S3. The assembler task in the sidecar checks that all expected gentxs exist before proceeding.

### Post-bootstrap ConfigureGenesis fails

The post-bootstrap `ConfigureGenesis` re-downloads genesis.json to the StatefulSet pod's PVC. If S3 is temporarily unavailable, this task retries. The genesis.json is already on the PVC from the bootstrap phase, but the sidecar's configure-genesis handler re-downloads to ensure consistency.

### Bootstrap Job completes but StatefulSet pod can't start

If the PVC has initialized state but the StatefulSet pod fails to start (wrong image, resource limits, etc.), the SeiNode stays in Initializing phase. The per-node plan blocks at the post-bootstrap `MarkReady` step. Standard controller-runtime requeue handles this.

## File-by-File Changes

| File | Change |
|------|--------|
| `api/v1alpha1/validator_types.go` | Add `ForkExportHeight int64` to `GenesisCeremonyNodeConfig` |
| `api/v1alpha1/zz_generated.deepcopy.go` | Regenerated |
| `manifests/crd/bases/sei.io_seinodes.yaml` | Regenerated |
| `internal/controller/nodedeployment/nodes.go` | `generateSeiNode`: propagate `ForkExportHeight` from `genesis.fork.exportHeight` |
| `internal/planner/bootstrap.go` | Add `buildForkGenesisPlan`, `buildForkGenesisBootstrapProgression`, `buildForkGenesisPostBootstrapProgression`, `forkGenesisParamsForTaskType` |
| `internal/planner/planner.go` | Add `isForkGenesisCeremonyNode` predicate |
| `internal/planner/validator.go` | Route fork genesis ceremony nodes through `buildForkGenesisPlan` |
| `internal/task/bootstrap_resources.go` | Add `GenerateBootstrapJobWithHaltHeight` for fork genesis (no snapshot source) |
| `internal/task/bootstrap_job.go` | `deployBootstrapJobExecution.Execute`: detect fork genesis and call appropriate Job generator |

## Test Plan

| Test | File | Description |
|------|------|-------------|
| `TestBuildForkGenesisPlan_BootstrapPhases` | `bootstrap_test.go` | Fork genesis plan has bootstrap deploy, genesis ceremony tasks, await/teardown, post-bootstrap |
| `TestBuildForkGenesisPlan_HaltHeight` | `bootstrap_test.go` | Bootstrap Job halt-height is `forkExportHeight + 1` |
| `TestValidatorPlanner_ForkGenesis_UsesBootstrap` | `validator_test.go` | Node with `ForkExportHeight > 0` routes through `buildForkGenesisPlan` |
| `TestValidatorPlanner_StandardGenesis_NoBootstrap` | `validator_test.go` | Node with `ForkExportHeight == 0` uses `buildGenesisPlan` (unchanged) |
| `TestGenerateSeiNode_PropagatesForkExportHeight` | `nodes_test.go` | `generateSeiNode` sets `ForkExportHeight` when `genesis.fork` is configured |
| `TestGenerateBootstrapJobWithHaltHeight` | `bootstrap_resources_test.go` | Job spec has correct halt-height, no snapshot dependency |
| `TestForkGenesisBootstrapProgression` | `bootstrap_test.go` | Progression includes identity/gentx/upload before config |
| `TestForkGenesisPostBootstrapProgression` | `bootstrap_test.go` | Post-bootstrap includes SetGenesisPeers |

## Implementation Order

1. **CRD change.** Add `ForkExportHeight` to `GenesisCeremonyNodeConfig`. Run `make manifests generate`.
2. **Propagate ForkExportHeight.** Update `generateSeiNode` to set the field when `genesis.fork` is configured.
3. **Bootstrap Job generator.** Add `GenerateBootstrapJobWithHaltHeight` to `bootstrap_resources.go`. Update `deployBootstrapJobExecution` to detect fork genesis.
4. **Fork genesis plan builder.** Add `buildForkGenesisPlan` and its progression builders to `planner/bootstrap.go`.
5. **Validator planner routing.** Update `validatorPlanner.BuildPlan` to route fork genesis through bootstrap.
6. **Tests.** Unit tests for each step.
7. **Integration test.** End-to-end fork genesis ceremony with bootstrap Jobs in a test namespace.

Steps 1-2 are safe to ship independently (additive field, no behavior change until planner routing is added). Steps 3-5 should ship together.
