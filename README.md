# sei-k8s-controller

A Kubernetes operator for managing the full lifecycle of [Sei](https://sei.io) blockchain infrastructure. It defines two CRDs — `SeiNodeGroup` and `SeiNode` — under the `sei.io/v1alpha1` API group.

## Overview

`SeiNodeGroup` orchestrates fleets of nodes: it manages genesis ceremonies, coordinates deployments, and provisions networking and monitoring. `SeiNode` manages a single Sei node — its PVC, StatefulSet, headless Service, and sidecar-driven bootstrap.

### Key design decisions

- **One StatefulSet per node** — each `SeiNode` gets its own single-replica StatefulSet rather than pooling nodes. Groups exist for fleet coordination.
- **Dedicated node scheduling** — pods require `karpenter.sh/nodepool=sei-node` and tolerate `sei.io/workload=sei-node:NoSchedule`, keeping blockchain workloads off general-purpose nodes.
- **Sidecar architecture** — every node runs a [seictl](https://github.com/sei-protocol/seictl) sidecar as a restartable init container that drives bootstrap tasks before seid starts and handles runtime operations afterward.
- **Plan model** — bootstrap is driven by a `TaskPlan` stored in `status.plan`. The controller builds a task sequence based on the node's mode, submits tasks to the sidecar one at a time, and advances through the plan.
- **Environment-driven genesis** — genesis resolution is handled by the sidecar autonomously. Embedded sei-config is checked first for well-known chains (pacific-1, atlantic-2, arctic-1), then S3 fallback at `{SEI_GENESIS_BUCKET}/{chainID}/genesis.json`.

## CRDs

### SeiNodeGroup

Orchestrates fleets of `SeiNode` resources with optional genesis ceremony support.

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodeGroup
metadata:
  name: devnet
spec:
  replicas: 4
  genesis:
    chainId: my-devnet
    stakingAmount: "10000000usei"
  template:
    spec:
      chainId: my-devnet
      image: sei-protocol/seid:v5.0.0
      validator: {}
      sidecar:
        image: ghcr.io/sei-protocol/seictl:v0.0.26
```

### SeiNode

Manages a single Sei node. Supports full nodes, validators, archivers, and replayers.

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNode
metadata:
  name: mainnet-0
spec:
  chainId: pacific-1
  image: sei-protocol/seid:v5.0.0
  fullNode:
    snapshot:
      s3:
        targetHeight: 100000000
      trustPeriod: "9999h0m0s"
    peers:
      - ec2Tags:
          region: us-east-2
          tags:
            Network: pacific-1
```

**Bootstrap modes** (determined by spec):

| Mode | Condition | Key tasks |
|------|-----------|-----------|
| Full node | `spec.fullNode` set | `configure-genesis` > `snapshot-restore` > `config-apply` > `discover-peers` > `mark-ready` |
| Validator | `spec.validator` set | Same as full node, or genesis ceremony flow for new networks |
| Archive | `spec.archive` set | State sync with archival pruning configuration |
| Replayer | `spec.replayer` set | Snapshot restore with result export for shadow validation |

## Platform Configuration

The controller reads all infrastructure-level settings from environment variables. Every field is required — the controller fails fast at startup if any are missing.

| Env var | Description |
|---------|-------------|
| `SEI_NODEPOOL_NAME` | Karpenter NodePool for pod scheduling |
| `SEI_TOLERATION_KEY` | Taint key to tolerate |
| `SEI_TOLERATION_VALUE` | Taint value to tolerate |
| `SEI_SERVICE_ACCOUNT` | ServiceAccount for node pods |
| `SEI_STORAGE_CLASS_PERF` | StorageClass for full/validator/archive nodes |
| `SEI_STORAGE_CLASS_DEFAULT` | StorageClass for other modes |
| `SEI_STORAGE_SIZE_DEFAULT` | PVC size for full/validator nodes |
| `SEI_STORAGE_SIZE_ARCHIVE` | PVC size for archive nodes |
| `SEI_RESOURCE_CPU_ARCHIVE` | CPU request for archive nodes |
| `SEI_RESOURCE_MEM_ARCHIVE` | Memory request for archive nodes |
| `SEI_RESOURCE_CPU_DEFAULT` | CPU request for full/validator nodes |
| `SEI_RESOURCE_MEM_DEFAULT` | Memory request for full/validator nodes |
| `SEI_SNAPSHOT_REGION` | AWS region for snapshot S3 operations |
| `SEI_RESULT_EXPORT_BUCKET` | S3 bucket for shadow result exports |
| `SEI_RESULT_EXPORT_REGION` | AWS region for result export bucket |
| `SEI_RESULT_EXPORT_PREFIX` | S3 key prefix for result exports |
| `SEI_GENESIS_BUCKET` | S3 bucket for genesis artifacts |
| `SEI_GENESIS_REGION` | AWS region for genesis artifacts bucket |

## Development

```bash
make build                    # Build the manager binary
make test                     # Run unit tests
make lint                     # Run golangci-lint
make manifests generate       # Regenerate CRDs, RBAC, DeepCopy after type changes
```

## Deployment

The controller image is built and pushed to ECR by GitHub Actions on every push to `main`.

The `config/` directory follows the standard [Kubebuilder](https://book.kubebuilder.io) layout:

```
config/
├── crd/              # Generated CRD manifests (SeiNode, SeiNodeGroup)
├── rbac/             # ServiceAccount, ClusterRole, bindings, leader election
├── manager/          # Deployment and metrics Service
├── monitoring/       # PrometheusRule and ServiceMonitor
├── network-policy/   # Metrics traffic NetworkPolicy
└── default/          # Top-level kustomization (namePrefix: sei-k8s-)
```

Platform repos reference `config/default` as a remote kustomize base:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: sei-k8s-controller-system
resources:
  - github.com/sei-protocol/sei-k8s-controller/config/default?ref=main
  - namespace.yaml
```
