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
    s3:
      uri: s3://sei-genesis/pacific-1/genesis.json
      region: us-east-2
  snapshot:
    s3:
      uri: s3://sei-snapshots/pacific-1
      region: us-east-2
    trustPeriod: "9999h0m0s"
  peers:
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
| Snapshot | `spec.snapshot` set | `snapshot-restore` > `configure-genesis` > `config-apply` > `discover-peers` > `config-validate` > `mark-ready` |
| Replayer | `spec.replayer` set | `snapshot-restore` > `configure-genesis` > `config-apply` > `discover-peers` > `config-validate` > `mark-ready` |
| Genesis PVC | `spec.genesis.pvc` set | `config-apply` > `config-validate` > `mark-ready` |
| Genesis S3 | `spec.genesis.s3` set | `configure-genesis` > `config-apply` > `config-validate` > `mark-ready` |

Tasks like `discover-peers` and `configure-genesis` are inserted dynamically based on the spec.

**Runtime operations** (after bootstrap):
- **Snapshot generation** — when `spec.snapshotGeneration` is configured, the sidecar patches `app.toml` for archival pruning and the controller periodically submits `snapshot-upload` tasks to push completed snapshots to S3.
- **Result export** — for replayer nodes with `spec.replayer.resultExport` set, the controller schedules a recurring `result-export` task after the init plan completes.

### Shadow Replayer

A replayer node replays blocks from a snapshot using a different execution engine (e.g. for shadow validation). The controller discovers peers via EC2 tags and infers the snapshot S3 bucket from the chain ID when no explicit URI is given.

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNode
metadata:
  name: pacific-1-shadow-replayer
spec:
  chainId: pacific-1
  image: ghcr.io/bdchatham/sei-shadow:skip-apphash
  sidecar:
    image: ghcr.io/sei-protocol/seictl@sha256:...
  entrypoint:
    command: ["seid"]
    args: ["start", "--home", "/sei", "--skip-app-hash-validation"]
  genesis:
    s3:
      uri: s3://sei-testnet-genesis-config/pacific-1/genesis.json
      region: us-east-2
  storage:
    retainOnDelete: true
  replayer:
    peers:
      - ec2Tags:
          region: eu-central-1
          tags:
            ChainIdentifier: pacific-1
            Component: state-syncer
    snapshot:
      s3:
        region: eu-central-1
      trustPeriod: "9999h0m0s"
    resultExport: {}
```

## Platform Configuration

The controller reads infrastructure-level settings from environment variables, falling back to sensible defaults. This allows per-environment tuning via the Deployment manifest or kustomize overlays without rebuilding the image.

| Env var | Default | Description |
|---------|---------|-------------|
| `SEI_NODEPOOL_NAME` | `sei-node` | Karpenter NodePool for pod scheduling |
| `SEI_TOLERATION_KEY` | `sei.io/workload` | Taint key to tolerate |
| `SEI_TOLERATION_VALUE` | `sei-node` | Taint value to tolerate |
| `SEI_SERVICE_ACCOUNT` | `seid-node` | ServiceAccount for node pods |
| `SEI_STORAGE_CLASS_PERF` | `gp3-10k-750` | StorageClass for full/validator/archive nodes |
| `SEI_STORAGE_CLASS_DEFAULT` | `gp3` | StorageClass for other modes |
| `SEI_STORAGE_SIZE_DEFAULT` | `1000Gi` | PVC size for full/validator nodes |
| `SEI_STORAGE_SIZE_ARCHIVE` | `2000Gi` | PVC size for archive nodes |
| `SEI_RESOURCE_CPU_ARCHIVE` | `8` | CPU request for archive nodes |
| `SEI_RESOURCE_MEM_ARCHIVE` | `48Gi` | Memory request for archive nodes |
| `SEI_RESOURCE_CPU_DEFAULT` | `4` | CPU request for full/validator nodes |
| `SEI_RESOURCE_MEM_DEFAULT` | `32Gi` | Memory request for full/validator nodes |

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

The `config/` directory follows the standard [Kubebuilder](https://book.kubebuilder.io) layout:

```
config/
├── crd/              # Generated CRD manifests (SeiNode, SeiNodePool)
├── rbac/             # ServiceAccount, ClusterRole, bindings, leader election
├── manager/          # Deployment and metrics Service
├── network-policy/   # Metrics traffic NetworkPolicy
└── default/          # Top-level kustomization (namePrefix: sei-k8s-)
```

`config/default` is the canonical kustomize base. Platform repos reference it as a remote base and apply environment-specific overlays (namespace, image overrides, resource patches):

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: sei-k8s-controller-system
resources:
  - github.com/sei-protocol/sei-k8s-controller/config/default?ref=main
  - namespace.yaml
```

CRD manifests are also mirrored into `manifests/` for convenience.
