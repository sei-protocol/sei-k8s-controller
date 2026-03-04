# sei-k8s-controller

A Kubernetes operator for managing the full lifecycle of [Sei](https://sei.io) blockchain infrastructure. It defines two CRDs — `SeiNodePool` and `SeiNode` — under the `sei.io/v1alpha1` API group.

## Overview

`SeiNodePool` orchestrates multi-node genesis networks from scratch: it runs a genesis ceremony, distributes validator configs to per-node volumes, then creates individual `SeiNode` resources. `SeiNode` manages a single Sei node — its PVC, StatefulSet, headless Service, and optional sidecar-driven bootstrap.

### Key design decisions

- **One StatefulSet per node** — each `SeiNode` gets its own single-replica StatefulSet rather than pooling nodes into a single StatefulSet. The pool abstraction exists only for genesis orchestration.
- **EFS for genesis sharing** — `SeiNodePool` uses a `ReadWriteMany` EFS volume so the genesis ceremony Job and per-node prep Jobs can share data concurrently. Individual node data PVCs use gp3 (`ReadWriteOnce`).
- **Dedicated node scheduling** — pods require `karpenter.sh/nodepool=sei-node` and tolerate `sei.io/workload=sei-node:NoSchedule`, keeping blockchain workloads off general-purpose nodes.
- **Sidecar as restartable init container** — when configured, a sidecar runs for the pod's entire lifetime via Kubernetes native sidecar support (`restartPolicy: Always` on an init container), driving bootstrap tasks before seid starts.

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
      - type: ec2Tags
        region: us-east-2
        tags:
          Network: pacific-1
  sidecar:
    image: sei-protocol/sei-sidecar:latest
  scheduledUpgrades:
    - height: 100000
      image: sei-protocol/seid:v6.0.0
```

**Bootstrap modes** (determined by spec):

| Mode | Condition | Task sequence |
|------|-----------|---------------|
| Snapshot | `spec.snapshot` set | `snapshot-restore` > `discover-peers` > `config-patch` > `mark-ready` |
| Genesis PVC | `spec.genesis.pvc` set | `config-patch` > `mark-ready` |
| Peer sync | Default | `discover-peers` > `config-patch` > `mark-ready` |

Additional tasks are inserted dynamically: `configure-genesis` (S3 genesis download), `configure-state-sync` (Tendermint state sync), and `discover-peers` (EC2 tag or static peer discovery).

**Runtime operations** (after bootstrap):
- **Scheduled upgrades** — the controller submits `schedule-upgrade` tasks to the sidecar. When the chain halts at the upgrade height, the controller patches the StatefulSet image and Kubernetes restarts the pod with the new binary.
- **Snapshot generation** — periodic Tendermint snapshots can be uploaded to S3 via `snapshot-upload` tasks.

## Development

```bash
make build       # Build the manager binary
make test        # Run unit tests
make lint        # Run golangci-lint
make manifests   # Regenerate CRD and RBAC manifests
make generate    # Regenerate DeepCopy implementations
```

## Deployment

The controller is deployed via [Flux CD](https://fluxcd.io). The platform repo contains a Flux `Kustomization` that points at `config/overlays/dev/` and overrides the controller image tag.

```
config/
├── crd/           # Generated CRD manifests
├── manager/       # Controller Deployment + Namespace
├── rbac/          # ClusterRole, bindings, ServiceAccount
├── default/       # Base kustomize overlay (crd + rbac + manager)
└── overlays/
    └── dev/       # Dev cluster overlay with resource limits
```

The container image is built from a multi-stage Dockerfile (Go build + `distroless/static:nonroot` runtime) and pushed to ECR by the `ecr.yml` GitHub Actions workflow on every push to `main`.
