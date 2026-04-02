# LLD: Automated Fork Export via Temporary SeiNode

## Overview

Automates the fork export by creating a temporary "exporter" SeiNode that bootstraps from the source chain using the existing bootstrap pipeline (identical to replayer). The SeiNode controller brings it to the target height. The group plan then submits `export-state` to the exporter's sidecar and uploads the result.

No new fields on FullNodeSpec. The exporter is a plain FullNode with BootstrapImage + TargetHeight — the same config replayers use.

## Key Insight

`seid export --height N` reads committed state at exactly height N from the database, regardless of whether seid has progressed beyond N. This means we don't need to halt the node precisely — we just need it to have reached height N. The export command is deterministic.

## Flow

```
reconcileSeiNodes creates:
  - N validator SeiNodes (start init plans, block at configure-genesis)
  - 1 exporter SeiNode (FullNode, bootstraps from source chain snapshot)

SeiNode controller drives exporter through standard bootstrap:
  Pending → Initializing → Running
  (snapshot-restore → config → sync to height → production StatefulSet)

Group plan (once exporter reaches Running):
  await-exporter-running        ← polls exporter phase == Running
  submit-export-state           ← submits export-state task to exporter's sidecar
  teardown-exporter             ← deletes exporter SeiNode
  assemble-genesis-fork         ← existing: downloads export, strips validators, collect-gentxs
  collect-and-set-peers         ← existing
  await-nodes-running           ← existing

Validators unblock: configure-genesis finds genesis.json → proceed → Running
```

## CRD Changes

### ForkConfig (expanded)

```go
type ForkConfig struct {
    // SourceChainID is the chain ID of the network being forked.
    SourceChainID string `json:"sourceChainId"`

    // SourceImage is the seid container image compatible with the source
    // chain at ExportHeight.
    SourceImage string `json:"sourceImage"`

    // ExportHeight is the block height at which to export state.
    ExportHeight int64 `json:"exportHeight"`
}
```

No changes to FullNodeSpec, SnapshotSource, or any existing types.

## Exporter SeiNode

Created by `reconcileSeiNodes` as `{group}-exporter`. A plain FullNode:

```go
Spec: SeiNodeSpec{
    ChainID: fork.SourceChainID,
    Image:   fork.SourceImage,
    FullNode: &FullNodeSpec{
        Snapshot: &SnapshotSource{
            S3:             &S3SnapshotSource{TargetHeight: fork.ExportHeight},
            BootstrapImage: fork.SourceImage,
        },
    },
}
```

This is the same bootstrap config pattern that replayers use. The SeiNode controller handles it through the standard `fullNodePlanner` → `buildBootstrapPlan` → bootstrap Job → StatefulSet.

Labels: `sei.io/nodegroup: {group}`, `sei.io/role: exporter`
Excluded from `IncumbentNodes`.

## Group Plan Tasks

### `submit-export-state` (new controller-side task)

Submits an `export-state` task to the exporter's sidecar via HTTP API:

```go
type SubmitExportStateParams struct {
    ExporterName  string `json:"exporterName"`
    Namespace     string `json:"namespace"`
    ExportHeight  int64  `json:"exportHeight"`
    SourceChainID string `json:"sourceChainId"`
}
```

The task:
1. Builds a sidecar client for the exporter node
2. Submits `export-state` with params: `{height: N, chainId: sourceChainId, s3Bucket: genesisBucket, s3Key: {sourceChainId}/exported-state.json, s3Region: genesisRegion}`
3. Polls the sidecar for task completion
4. Returns Complete when the sidecar task completes

This is the same submit-and-poll pattern used by other sidecar tasks in the plan executor.

### `await-exporter-running` (new controller-side task)

Polls exporter SeiNode phase:

```go
type AwaitExporterRunningParams struct {
    ExporterName string `json:"exporterName"`
    Namespace    string `json:"namespace"`
}
```

Returns Complete when `Phase == Running`, Failed when `Phase == Failed`.

### `teardown-exporter` (new controller-side task)

Deletes the exporter SeiNode:

```go
type TeardownExporterParams struct {
    ExporterName string `json:"exporterName"`
    Namespace    string `json:"namespace"`
}
```

Polls until the SeiNode is gone (ownerReferences cascade the StatefulSet, PVC, etc.).

## Full Group Plan Sequence

When `ForkGenesisCeremonyNeeded`:

```
await-exporter-running      (controller: poll exporter phase)
submit-export-state         (controller: submit to exporter sidecar, poll completion)
teardown-exporter           (controller: delete exporter SeiNode)
assemble-genesis-fork       (sidecar: download export, strip validators, collect-gentxs)
collect-and-set-peers       (existing)
await-nodes-running         (existing)
```

When export already exists in S3 (checked by `needsForkExporter`):

```
assemble-genesis-fork       (sidecar: download export, strip validators, collect-gentxs)
collect-and-set-peers       (existing)
await-nodes-running         (existing)
```

## Edge Case: Export Already Exists

`needsForkExporter` checks S3 for `{sourceChainId}/exported-state.json`. If it exists, no exporter SeiNode is created and the group plan skips the first three tasks. This supports:
- Re-reconciling after a failed assembly (export already done)
- Multiple fork groups from the same source chain (export once, fork many)
- Pre-uploaded exports for faster iteration

## Example YAML

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
    fork:
      sourceChainId: pacific-1
      sourceImage: ghcr.io/sei-protocol/seid:v5.9.0
      exportHeight: 200000000
  template:
    spec:
      image: ghcr.io/sei-protocol/seid:v6.0.0
      validator: {}
```

## Files Affected

### Controller
| File | Change |
|------|--------|
| `api/v1alpha1/seinodegroup_types.go` | ForkConfig: add SourceImage, ExportHeight |
| `internal/controller/nodegroup/nodes.go` | ensureForkExporter, needsForkExporter, filter populateIncumbentNodes |
| `internal/planner/group.go` | Prepend export tasks to fork plan when exporter exists |
| `internal/task/fork_export.go` | New: SubmitExportStateParams, AwaitExporterRunningParams, TeardownExporterParams + executions |
| `internal/task/task.go` | Deserialize: 3 new cases |

### Seictl
| File | Change |
|------|--------|
| (export-state task already exists from earlier work) | No changes needed if export-state handler is already registered |

### What's Reused (no changes)
- `fullNodePlanner` — builds the exporter's bootstrap plan as-is
- `buildBootstrapPlan` — standard bootstrap task sequence
- `GenerateBootstrapJob` — standard bootstrap Job
- `SeiNode controller` — drives the exporter's full lifecycle
- `SnapshotSource` / `S3SnapshotSource` / `BootstrapImage` — existing types
