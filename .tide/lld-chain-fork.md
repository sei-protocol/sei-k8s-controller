# LLD: Fork Existing Chain to Private Network

## Overview

Extends the SeiNodeGroup genesis ceremony to support forking an existing chain's state into a new private network. A fork preserves all on-chain state (accounts, contracts, EVM state) from a source chain at a specific height, but boots under a new chain ID with a fresh validator set.

## Scope: System Tier

Two repos, two new sidecar tasks, one new planner, CRD additions, 3 Phase 2 controller-side tasks.

## Phased Delivery

- **Phase 1**: Pre-exported state in S3. User runs `seid export` manually. Controller handles fork ceremony from exported state.
- **Phase 2**: Automated export via bootstrap Job. Controller runs snapshot restore → seid export → S3 upload.
- **Phase 3**: Genesis mutations (SetParam, FundAccount).

---

## 1. CRD Types

### File: `api/v1alpha1/seinodegroup_types.go`

### ForkConfig

```go
// +kubebuilder:validation:XValidation:rule="has(self.stateExport) || has(self.exportJob)",message="one of stateExport or exportJob must be set"
// +kubebuilder:validation:XValidation:rule="!(has(self.stateExport) && has(self.exportJob))",message="stateExport and exportJob are mutually exclusive"
type ForkConfig struct {
    SourceChainID string            `json:"sourceChainId"`  // +kubebuilder:validation:MinLength=1
    SourceHeight  int64             `json:"sourceHeight"`   // +kubebuilder:validation:Minimum=1
    StateExport   *ForkStateExport  `json:"stateExport,omitempty"`   // Phase 1
    ExportJob     *ForkExportJob    `json:"exportJob,omitempty"`     // Phase 2
    Mutations     []GenesisMutation `json:"mutations,omitempty"`     // Phase 3
}
```

### ForkStateExport (Phase 1)

```go
type ForkStateExport struct {
    Bucket string `json:"bucket"` // +kubebuilder:validation:MinLength=1
    Key    string `json:"key"`    // +kubebuilder:validation:MinLength=1
    Region string `json:"region"` // +kubebuilder:validation:MinLength=1
}
```

### ForkExportJob (Phase 2)

```go
type ForkExportJob struct {
    Image    string           `json:"image"`    // +kubebuilder:validation:MinLength=1
    Snapshot S3SnapshotSource `json:"snapshot"`
}
```

### GenesisMutation (Phase 3)

```go
// +kubebuilder:validation:Enum=SetParam;FundAccount
type GenesisMutationType string

// +kubebuilder:validation:XValidation:rule="self.type == 'SetParam' ? (has(self.key) && has(self.value)) : true"
// +kubebuilder:validation:XValidation:rule="self.type == 'FundAccount' ? (has(self.address) && has(self.balance)) : true"
type GenesisMutation struct {
    Type    GenesisMutationType `json:"type"`
    Key     string              `json:"key,omitempty"`     // SetParam: dotted JSON path
    Value   string              `json:"value,omitempty"`   // SetParam: JSON-encoded value
    Address string              `json:"address,omitempty"` // FundAccount: bech32 address
    Balance string              `json:"balance,omitempty"` // FundAccount: coin notation
}
```

### Field on GenesisCeremonyConfig

```go
type GenesisCeremonyConfig struct {
    // ... existing fields ...
    Fork *ForkConfig `json:"fork,omitempty"`
}
```

### New condition

```go
ConditionForkNeeded = "ForkNeeded"
```

---

## 2. Controller: Condition Detection

### File: `internal/controller/nodegroup/nodes.go`

Add `detectForkNeeded` after `detectDeploymentNeeded`:

```go
func (r *SeiNodeGroupReconciler) detectForkNeeded(group *seiv1alpha1.SeiNodeGroup) {
    if group.Spec.Genesis == nil || group.Spec.Genesis.Fork == nil {
        removeCondition(group, seiv1alpha1.ConditionForkNeeded)
        return
    }
    if hasConditionTrue(group, seiv1alpha1.ConditionGenesisCeremonyComplete) {
        removeCondition(group, seiv1alpha1.ConditionForkNeeded)
        return
    }
    if hasConditionTrue(group, seiv1alpha1.ConditionPlanInProgress) {
        return
    }
    setCondition(group, seiv1alpha1.ConditionForkNeeded, metav1.ConditionTrue,
        "ForkConfigured", fmt.Sprintf("Fork from %s at height %d",
            group.Spec.Genesis.Fork.SourceChainID, group.Spec.Genesis.Fork.SourceHeight))
}
```

Call in `reconcileSeiNodes` after `detectDeploymentNeeded`:

```go
r.detectDeploymentNeeded(group)
r.detectForkNeeded(group)
```

---

## 3. Planner

### File: `internal/planner/planner.go`

Update `ForGroup` dispatch (fork before genesis):

```go
func ForGroup(group *seiv1alpha1.SeiNodeGroup) (GroupPlanner, error) {
    if needsForkPlan(group) {
        return &forkGroupPlanner{}, nil
    }
    if needsGenesisPlan(group) {
        return &genesisGroupPlanner{}, nil
    }
    // ... deployment ...
}
```

Add `needsForkPlan`:

```go
func needsForkPlan(group *seiv1alpha1.SeiNodeGroup) bool {
    if group.Spec.Genesis == nil || group.Spec.Genesis.Fork == nil { return false }
    if group.Status.Plan != nil { return false }
    for _, c := range group.Status.Conditions {
        if c.Type == seiv1alpha1.ConditionForkNeeded && c.Status == metav1.ConditionTrue {
            return int32(len(group.Status.IncumbentNodes)) >= group.Spec.Replicas
        }
    }
    return false
}
```

Guard `needsGenesisPlan` against fork:

```go
func needsGenesisPlan(group *seiv1alpha1.SeiNodeGroup) bool {
    if group.Spec.Genesis == nil { return false }
    if group.Spec.Genesis.Fork != nil { return false } // fork handled by forkGroupPlanner
    // ... rest unchanged
}
```

### File: `internal/planner/fork.go` (new)

```go
type forkGroupPlanner struct{}

func (p *forkGroupPlanner) BuildPlan(group *seiv1alpha1.SeiNodeGroup) (*seiv1alpha1.TaskPlan, error)
```

#### Phase 1 task sequence (stateExport set):

```
assemble-fork-genesis → collect-and-set-peers → await-nodes-running
```

#### Phase 2 task sequence (exportJob set):

```
deploy-fork-job → await-fork-export → teardown-fork-job →
assemble-fork-genesis → collect-and-set-peers → await-nodes-running
```

---

## 4. Controller Task Types

### File: `internal/task/fork.go` (new)

```go
const (
    TaskTypeDeployForkJob   = "deploy-fork-job"
    TaskTypeAwaitForkExport = "await-fork-export"
    TaskTypeTeardownForkJob = "teardown-fork-job"
)

type AssembleForkGenesisParams struct { ... }  // sidecar task params
type GenesisMutationParam struct { ... }
type GenesisAccountParam struct { ... }
type DeployForkJobParams struct { ... }       // Phase 2 controller tasks
type AwaitForkExportParams struct { ... }
type TeardownForkJobParams struct { ... }
```

### File: `internal/task/fork_job.go` (new, Phase 2)

Controller-side task executions following bootstrap job pattern:
- `deployForkJobExecution` — creates Job + headless Service
- `awaitForkExportExecution` — polls Job status
- `teardownForkJobExecution` — deletes Job + Service

### File: `internal/task/task.go`

Add to `Deserialize` switch:

```go
case "assemble-fork-genesis":
    return deserializeSidecar[AssembleForkGenesisParams](id, params, buildSC, false)
case TaskTypeDeployForkJob:
    return deserializeDeployForkJob(id, params, cfg)
case TaskTypeAwaitForkExport:
    return deserializeAwaitForkExport(id, params, cfg)
case TaskTypeTeardownForkJob:
    return deserializeTeardownForkJob(id, params, cfg)
```

---

## 5. Sidecar Tasks

### File: `seictl/sidecar/engine/types.go`

```go
TaskExportState         TaskType = "export-state"
TaskAssembleForkGenesis TaskType = "assemble-fork-genesis"
```

### File: `seictl/sidecar/tasks/export_state.go` (new)

```go
type ExportStateRequest struct {
    Height   int64  `json:"height,omitempty"`
    ChainID  string `json:"chainId"`
    S3Bucket string `json:"s3Bucket"`
    S3Key    string `json:"s3Key,omitempty"`
    S3Region string `json:"s3Region"`
}

type StateExporter struct { homeDir string; cmdRunner CommandRunner; s3UploaderFactory UploaderFactory }
```

Implementation: runs `seid export --height N --home $HOME`, uploads stdout to S3. Marker: `.sei-sidecar-export-done`.

### File: `seictl/sidecar/tasks/assemble_fork_genesis.go` (new)

```go
type AssembleForkGenesisRequest struct {
    ExportedStateBucket string                `json:"exportedStateBucket"`
    ExportedStateKey    string                `json:"exportedStateKey"`
    ExportedStateRegion string                `json:"exportedStateRegion"`
    ChainID             string                `json:"chainId"`
    InitialHeight       int64                 `json:"initialHeight,omitempty"`
    AccountBalance      string                `json:"accountBalance"`
    Namespace           string                `json:"namespace"`
    Nodes               []AssembleNodeEntry   `json:"nodes"`
    Mutations           []GenesisMutation     `json:"mutations,omitempty"`
}

type ForkGenesisAssembler struct { homeDir, bucket, region string; ... }
```

Implementation steps:
1. Download exported state from S3 → `config/genesis.json`
2. Rewrite `chain_id`, `initial_height` (sourceHeight+1), `genesis_time` (now), clear `validators`
3. Strip validator state: staking (validators, delegations, powers), slashing (signing_infos), distribution (validator records)
4. Apply mutations: SetParam (dot-path JSON navigation), FundAccount (add to auth+bank modules)
5. Download per-node gentx files (reuses existing pattern)
6. Add missing genesis accounts for new validators
7. Run collect-gentxs (`genutil.GenAppStateFromConfig`)
8. Upload `genesis.json` + `peers.json` to S3

Marker: `.sei-sidecar-fork-assemble-done`

### File: `seictl/sidecar/client/tasks.go`

Add `ExportStateTask` and `AssembleForkGenesisTask` builders with `Validate()` and `ToTaskRequest()`.

### File: `seictl/serve.go`

Register both handlers in the handler map.

---

## 6. Per-Node Init Plans: UNCHANGED

Each validator SeiNode follows the existing genesis ceremony init plan:

```
generate-identity → generate-gentx → upload-genesis-artifacts →
configure-genesis (retry until genesis.json appears in S3) →
config-apply → set-genesis-peers → config-validate → mark-ready
```

The `assemble-fork-genesis` task produces `genesis.json` at the same S3 path that `configure-genesis` polls. The per-node planner doesn't know or care that the genesis came from a fork.

---

## 7. Example YAML

### Phase 1 (pre-exported state)

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodeGroup
metadata:
  name: private-fork
spec:
  replicas: 4
  genesis:
    chainId: private-fork-1
    stakingAmount: "10000000usei"
    accountBalance: "1000000000000000000000usei"
    fork:
      sourceChainId: pacific-1
      sourceHeight: 200000000
      stateExport:
        bucket: sei-chain-exports
        key: pacific-1/export-200000000.json
        region: us-east-1
  template:
    spec:
      image: ghcr.io/sei-protocol/seid:v5.9.0
      validator: {}
```

### Phase 2 (automated export)

```yaml
genesis:
  chainId: private-fork-1
  fork:
    sourceChainId: pacific-1
    sourceHeight: 200000000
    exportJob:
      image: ghcr.io/sei-protocol/seid:v5.9.0
      snapshot:
        targetHeight: 200000000
```

---

## 8. File Inventory

### Controller repo (sei-k8s-controller)

| File | Action |
|------|--------|
| `api/v1alpha1/seinodegroup_types.go` | Add ForkConfig, ForkStateExport, ForkExportJob, GenesisMutation types; add Fork field; add ConditionForkNeeded |
| `internal/controller/nodegroup/nodes.go` | Add detectForkNeeded; call from reconcileSeiNodes |
| `internal/planner/planner.go` | Add needsForkPlan; update ForGroup dispatch; guard needsGenesisPlan |
| `internal/planner/fork.go` | **New**: forkGroupPlanner with BuildPlan |
| `internal/task/fork.go` | **New**: AssembleForkGenesisParams, mutation params, Phase 2 task params |
| `internal/task/fork_job.go` | **New**: Phase 2 controller-side task executions |
| `internal/task/task.go` | Add 4 cases to Deserialize switch |

### Seictl repo

| File | Action |
|------|--------|
| `sidecar/engine/types.go` | Add 2 TaskType constants |
| `sidecar/tasks/export_state.go` | **New**: StateExporter with TypedHandler |
| `sidecar/tasks/assemble_fork_genesis.go` | **New**: ForkGenesisAssembler with TypedHandler |
| `sidecar/client/tasks.go` | Add 2 constants, 2 task builders |
| `serve.go` | Add 2 handler registrations |

### Tests

| File | Action |
|------|--------|
| `seictl/sidecar/tasks/export_state_test.go` | **New**: 5 tests |
| `seictl/sidecar/tasks/assemble_fork_genesis_test.go` | **New**: 6 tests |
| `seictl/sidecar/tasks/genesis_mutations_test.go` | **New**: 11 tests |
| `seictl/sidecar/client/tasks_test.go` | Add 5 tests |

---

## 9. One-Way Doors

These require explicit approval before implementation:

1. **`Fork` field name on GenesisCeremonyConfig** — CRD field name. Changing after deployment requires migration.
2. **GenesisMutation type names** (`SetParam`, `FundAccount`) — become part of the CRD schema and serialized in status.
3. **`assemble-fork-genesis` task type string** — persisted in SQLite, referenced by controller.
4. **`export-state` task type string** — persisted in SQLite, referenced by controller.

All were discussed and approved during the design session.

---

## 10. Invariants

1. Fork and standard genesis are mutually exclusive. `needsGenesisPlan` returns false when `Fork != nil`.
2. Fork only runs on first generation (`ConditionForkNeeded` cleared after `GenesisCeremonyComplete`).
3. Per-node init plans are identical for fork and standard genesis.
4. Phase 2 export Job follows the bootstrap job lifecycle pattern exactly.
5. The `assemble-fork-genesis` sidecar task uses raw JSON manipulation for validator stripping (avoids importing wasmvm/duckdb deps) but uses proto codec for account manipulation (required for auth Any types).
