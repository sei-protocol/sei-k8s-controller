# Production Deployment Analysis: SeiNode CRD vs sei-infra Snapshotter

## Current State: sei-infra Snapshotter (EC2/Terraform)

The existing snapshotter is entirely EC2-based with no Kubernetes involvement:

- **3 instances** (`m7i.8xlarge`) in `eu-central-1` running `seid v6.3.0`
- **32 TiB EBS** storage (RAID0 across 5 disks)
- **Weekly AMI snapshots** (Mondays 16:00 UTC via cron + `aws ec2 create-image`)
- **S3 bucket**: `pacific-1-snapshots/state-sync/` for Tendermint snapshots
- **DynamoDB** metadata table: `pacific-1_snapshot_metadata`
- **ALB** with Route53 DNS (`rpc.sei-archive.pacific-1.seinetwork.io`)
- **WAF** rate limiting (300 req/5min per IP)
- **IAM role**: `pacific-1-snapshot-iam-role` with S3 + DynamoDB permissions

### Bootstrap Flow

1. `ec2_init.sh` downloads sei-infra from S3, runs `generate_configs.py`
2. `mount_ebs_volumes.sh` sets up RAID0 or JBOD
3. `post_init.sh` installs seid binary, configures, starts systemd service
4. `init_configure.sh` runs `seid init`, fetches peers, sets pruning to `nothing`, enables SeiDB + OCC
5. Crontab schedules `snapshot.sh` for weekly AMI creation

### Snapshot Generation

| Setting | Value |
|---------|-------|
| Cron | `0 16 * * 1` (Mondays 16:00 UTC) |
| Mechanism | `snapshot.sh` → `aws ec2 create-image` (no-reboot) |
| Retention | AMIs older than 30 days removed |
| Metadata | DynamoDB with `last_update`, `height`, `imageName` |

### State-Syncer (Snapshot Source)

- 3 instances in `eu-central-1`
- Snapshot interval: 4000 blocks
- Script: `create_snapshot.sh` (halt-height loop, `seid tendermint snapshot`, keep 5 recent)
- Peers: `primaryEndpoint: https://sei-rpc.polkachu.com`

---

## SeiNode CRD: Production Snapshotter

### Example Manifest (Archive Mode)

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNode
metadata:
  name: pacific-1-snapshotter
  namespace: sei-nodes
spec:
  chainId: pacific-1
  image: "ghcr.io/sei-protocol/sei:v6.3.0"
  sidecar:
    image: ghcr.io/sei-protocol/seictl@sha256:78acbf33cc62c41f65766eef10698af2656b3a169eef3be19f707af6f6f51d62
    resources:
      requests:
        cpu: "500m"
        memory: "256Mi"
  entrypoint:
    command: ["seid"]
    args: ["start", "--home", "/sei"]
  storage:
    retainOnDelete: true
  archive:
    peers:
      - ec2Tags:
          region: eu-central-1
          tags:
            ChainIdentifier: pacific-1
            Component: state-syncer
    snapshotGeneration:
      keepRecent: 5
      destination:
        s3:
          bucket: pacific-1-snapshots
          prefix: state-sync/
          region: eu-central-1
```

### Generated Kubernetes Resources

| Resource | Details |
|----------|---------|
| StatefulSet | 1 replica, `seid` + `seictl` sidecar |
| Service | Headless (`ClusterIP: None`), `PublishNotReadyAddresses: true` |
| PVC | `data-{nodeName}`, StorageClass `gp3-10k-750`, 2000Gi for archive |
| Init containers | `seid-init` (chain init), `sei-sidecar` (restartable) |

### PlatformConfig (Controller Environment Variables)

| Env Var | Default | Purpose |
|---------|---------|---------|
| `SEI_NODEPOOL_NAME` | `sei-node` | Karpenter NodePool for scheduling |
| `SEI_TOLERATION_KEY` | `sei.io/workload` | Taint key to tolerate |
| `SEI_TOLERATION_VALUE` | `sei-node` | Taint value |
| `SEI_SERVICE_ACCOUNT` | `seid-node` | ServiceAccount for node pods |
| `SEI_STORAGE_CLASS_PERF` | `gp3-10k-750` | StorageClass for full/validator/archive |
| `SEI_STORAGE_CLASS_DEFAULT` | `gp3` | StorageClass for other modes |
| `SEI_STORAGE_SIZE_DEFAULT` | `1000Gi` | PVC size for full/validator |
| `SEI_STORAGE_SIZE_ARCHIVE` | `2000Gi` | PVC size for archive |
| `SEI_RESOURCE_CPU_ARCHIVE` | `8` | CPU request for archive |
| `SEI_RESOURCE_MEM_ARCHIVE` | `48Gi` | Memory request for archive |
| `SEI_RESOURCE_CPU_DEFAULT` | `4` | CPU request for full/validator |
| `SEI_RESOURCE_MEM_DEFAULT` | `32Gi` | Memory request for full/validator |
| `SEI_SNAPSHOT_REGION` | `eu-central-1` | Default S3 region for snapshots |

### Resource Sizing by Mode

| Mode | StorageClass | Size | CPU | Memory |
|------|-------------|------|-----|--------|
| archive | gp3-10k-750 | 2000Gi | 8 | 48Gi |
| full, validator | gp3-10k-750 | 1000Gi | 4 | 32Gi |
| replayer | gp3 | 1000Gi | 4 | 32Gi |

### Hardcoded Values

| Setting | Value |
|---------|-------|
| Data dir | `/sei` |
| Default sidecar image | `ghcr.io/sei-protocol/seictl@sha256:...` |
| Default sidecar port | 7777 |
| Snapshot upload cron | `0 0 * * *` (daily midnight) |
| Snapshot interval | 2000 blocks (in config-apply) |

---

## Gap Analysis: sei-infra vs SeiNode CRD

| Aspect | sei-infra (EC2) | SeiNode CRD | Gap? |
|--------|-----------------|-------------|------|
| Compute | m7i.8xlarge (32 vCPU, 128GB) | 8 CPU, 48Gi | Tunable via PlatformConfig env vars |
| Storage | 32 TiB EBS RAID0 | 2 TiB PVC (gp3-10k-750) | Need larger PVC for full archive |
| S3 bucket | `pacific-1-snapshots` | Configurable per-node | No gap |
| ALB/DNS | ALB + Route53 + WAF | Not managed by controller | External concern (Ingress/Gateway API) |
| DynamoDB metadata | Snapshot metadata tracking | Not implemented | Gap -- could add to seictl |
| AMI snapshots | EC2 AMI creation | N/A (Tendermint snapshots to S3) | Different approach, arguably better |
| IAM | Instance profile | ServiceAccount + IRSA | Via PlatformConfig `SEI_SERVICE_ACCOUNT` |
| Monitoring | Prometheus EC2 SD + alerts | Need ServiceMonitor/PodMonitor | External concern |
| Snapshot verification | Dedicated verifier EC2 | Not implemented | Gap |
| Multi-instance | 3 EC2 instances | 1 replica StatefulSet | Could create multiple SeiNodes |

## What's Needed for Production

### Already handled by the controller
- Node lifecycle (bootstrap, init, running)
- Snapshot generation and S3 upload
- Peer discovery via EC2 tags
- Genesis configuration (embedded or S3)
- Config generation (pruning, state-sync intervals, etc.)
- PVC provisioning and cleanup
- Tolerations, affinity, service account assignment

### Needs external setup
- **Controller in prod Flux kustomization** (currently dev only)
- **Prod PlatformConfig tuning** (storage sizes, resource limits for prod workloads)
- **IRSA ServiceAccount** with S3 permissions in the node namespace
- **Networking** (ALB/Ingress, DNS, WAF -- separate from controller)
- **Monitoring** (ServiceMonitor for Prometheus, alerts ported from sei-infra)
- **Storage sizing** -- `SEI_STORAGE_SIZE_ARCHIVE` may need to be much larger for full archive

### Future enhancements
- Snapshot verification (automated post-upload check)
- DynamoDB metadata tracking for snapshot catalog
- Multi-replica support (if needed for HA/load distribution)
