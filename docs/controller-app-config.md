# Controller app-config file

The controller reads a single read-only application-config file, pointed at by
`SEI_CONTROLLER_CONFIG` and mounted as a directory (a GitOps-written ConfigMap,
typically `sei-controller-config`). It is decoded into `platform.FileConfig`
(`internal/platform/platform.go`).

Two read paths, by design:

- **Infra sections** (`scheduling`, `storage`, `resources`, `snapshot`,
  `resultExport`, `genesis`, `images`) are resolved **once at startup** by
  `platform.Load`. Editing them in the live ConfigMap propagates to the mount
  but has **no effect until the controller pod restarts**
  (`kubectl rollout restart`) — only `stateSync` hot-reloads.
- **`stateSync`** is re-read **per reconcile** so syncer changes hot-reload
  without a restart (the directory mount swaps atomically).

## Source of truth

The file is **authoritative** for infra config: a required field unset in the
file fails `Config.Validate` at startup (the controller does not boot). There is
no env-var fallback for these fields.

Networking/gateway config (`SEI_GATEWAY_*`, `SEI_P2P_ENDPOINT_DOMAIN`,
`SEI_NLB_TARGET_TYPE`) is **not** in the file — it stays env-sourced pending its
removal from the controller in the GitOps networking move (PLT-451).

## Schema

```yaml
# State-sync canonical syncers, keyed by chain-id. Bare host:port (no scheme).
# Read per-reconcile; >=2 entries per chain or the node fails closed.
stateSync:
  syncers:
    pacific-1:
      - rpc-1.example.net:26657
      - rpc-2.example.net:26657

# --- infra (read once at startup; env-var fallback during PLT-475) ---

scheduling:
  nodepoolName: sei-node          # SEI_NODEPOOL_NAME
  nodepoolArchive: sei-archive    # SEI_NODEPOOL_ARCHIVE
  tolerationKey: sei.io/workload  # SEI_TOLERATION_KEY
  serviceAccount: seid-node       # SEI_SERVICE_ACCOUNT

storage:                          # note: no sizePerf — matches the historical env layout
  classPerf: gp3-10k-750          # SEI_STORAGE_CLASS_PERF
  classDefault: gp3               # SEI_STORAGE_CLASS_DEFAULT
  classArchive: gp3-archive       # SEI_STORAGE_CLASS_ARCHIVE
  sizeDefault: 2000Gi             # SEI_STORAGE_SIZE_DEFAULT
  sizeArchive: 40Ti               # SEI_STORAGE_SIZE_ARCHIVE

resources:
  cpuArchive: "48"                # SEI_RESOURCE_CPU_ARCHIVE
  memArchive: 448Gi               # SEI_RESOURCE_MEM_ARCHIVE
  cpuDefault: "4"                 # SEI_RESOURCE_CPU_DEFAULT
  memDefault: 32Gi                # SEI_RESOURCE_MEM_DEFAULT

snapshot:
  bucket: sei-snapshots           # SEI_SNAPSHOT_BUCKET
  region: us-east-2               # SEI_SNAPSHOT_REGION

resultExport:
  bucket: sei-shadow-results      # SEI_RESULT_EXPORT_BUCKET
  region: us-east-2               # SEI_RESULT_EXPORT_REGION
  prefix: shadow-results/         # SEI_RESULT_EXPORT_PREFIX

genesis:
  bucket: sei-k8s-genesis         # SEI_GENESIS_BUCKET
  region: us-east-2               # SEI_GENESIS_REGION

images:
  sidecar: ghcr.io/sei-protocol/seictl@sha256:...        # SEI_SIDECAR_IMAGE
  kubeRBACProxy: quay.io/brancz/kube-rbac-proxy:v0.19.1  # SEI_KUBE_RBAC_PROXY_IMAGE
  cosmosExporter: ghcr.io/sei-protocol/sei-cosmos-exporter@sha256:...  # SEI_COSMOS_EXPORTER_IMAGE
```

A present-but-unparseable file is a hard startup error — it never silently falls
back to env. Required fields missing from both sources fail `Config.Validate`
with a message naming the file key and the env var.
