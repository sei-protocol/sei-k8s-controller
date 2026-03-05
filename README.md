# sei-k8s-controller

A Kubernetes operator for managing the full lifecycle of [Sei](https://sei.io) blockchain infrastructure. It defines two CRDs — `SeiNodePool` and `SeiNode` — under the `sei.io/v1alpha1` API group.

## Overview

`SeiNodePool` orchestrates multi-node genesis networks from scratch: it runs a genesis ceremony, distributes validator configs to per-node volumes, then creates individual `SeiNode` resources. `SeiNode` manages a single Sei node — its PVC, StatefulSet, headless Service, and sidecar-driven bootstrap.

### Key design decisions

- **One StatefulSet per node** — each `SeiNode` gets its own single-replica StatefulSet rather than pooling nodes into a single StatefulSet. The pool abstraction exists only for genesis orchestration.
- **EFS for genesis sharing** — `SeiNodePool` uses a `ReadWriteMany` EFS volume so the genesis ceremony Job and per-node prep Jobs can share data concurrently. Individual node data PVCs use gp3 (`ReadWriteOnce`).
- **Dedicated node scheduling** — pods require `karpenter.sh/nodepool=sei-node` and tolerate `sei.io/workload=sei-node:NoSchedule`, keeping blockchain workloads off general-purpose nodes.
- **Sidecar architecture** — every node runs a [seictl](https://github.com/sei-protocol/seictl) sidecar as a restartable init container (`restartPolicy: Always`) that drives bootstrap tasks before seid starts and handles runtime operations afterward. The default sidecar image is `ghcr.io/sei-protocol/seictl:latest`.
- **InitPlan model** — bootstrap is driven by a `TaskPlan` stored in `status.initPlan`. The controller builds a task sequence based on the node's spec, submits tasks to the sidecar one at a time, and advances through the plan. A single task failure marks the entire plan as Failed with no retries.

## CRDs

### SeiNodePool

Orchestrates a fresh genesis network.

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodePool
metadata:
  name: devnet
spec:
  chainId: arctic-1
  nodeConfiguration:
    nodeCount: 4
    image: sei-protocol/seid:v5.0.0
    entrypoint:
      command: ["seid"]
      args: ["start"]
  storage:
    retainOnDelete: false
```

**Reconciliation phases:**
1. Creates per-node data PVCs (gp3) and a shared genesis PVC (EFS)
2. Runs a genesis Job that initializes validators, patches chain params, and produces `genesis.json`
3. Runs per-node prep Jobs that copy node-specific data and configure peers via Kubernetes DNS
4. Creates `SeiNode` children with pre-populated PVCs
5. Applies a `NetworkPolicy` restricting inbound to intra-pool traffic

### SeiNode

Manages a single Sei node.

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNode
metadata:
  name: mainnet-0
spec:
  chainId: pacific-1
  image: sei-protocol/seid:v5.0.0
  genesis:
    chainId: pacific-1
    s3:
      uri: s3://sei-genesis/pacific-1/genesis.json
  snapshot:
    bucket:
      uri: s3://sei-snapshots/pacific-1
    region: us-east-2
  peers:
    sources:
      - ec2Tags:
          region: us-east-2
          tags:
            Network: pacific-1
  snapshotGeneration:
    interval: 2000
    keepRecent: 5
    destination:
      s3:
        bucket: sei-snapshots
        prefix: state-sync/
        region: us-east-2
```

The `sidecar` field is optional — when omitted the controller uses the default seictl image (`ghcr.io/sei-protocol/seictl:latest`). To override:

```yaml
  sidecar:
    image: ghcr.io/sei-protocol/seictl:v1.2.3
    port: 7777
```

**Bootstrap modes** (determined by spec):

| Mode | Condition | Task sequence |
|------|-----------|---------------|
| Snapshot | `spec.snapshot` set | `snapshot-restore` > `config-patch` > `mark-ready` |
| Genesis PVC | `spec.genesis.pvc` set | `config-patch` > `mark-ready` |
| Peer sync | Default | `config-patch` > `mark-ready` |

Additional tasks are inserted dynamically before `config-patch`: `discover-peers` (when `spec.peers` is set), `configure-genesis` (when `spec.genesis.s3` is set), and `configure-state-sync` (for Tendermint state sync).

**Runtime operations** (after bootstrap):
- **Snapshot generation** — when `spec.snapshotGeneration` is configured, the sidecar patches `app.toml` for archival pruning and the controller periodically submits `snapshot-upload` tasks to push completed snapshots to S3.

## Development

```bash
make build       # Build the manager binary
make test        # Run unit tests
make lint        # Run golangci-lint
make manifests   # Regenerate CRD and RBAC manifests
make generate    # Regenerate DeepCopy implementations
```

## Deployment

The controller image is built from a multi-stage Dockerfile (Go build + `distroless/static:nonroot` runtime) and pushed to ECR by the `ecr.yml` GitHub Actions workflow on every push to `main`.

CRD manifests are generated into `config/crd/bases/` and consumed by the platform deployment repo, which deploys the controller via [Flux CD](https://fluxcd.io).
