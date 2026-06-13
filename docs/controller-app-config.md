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

# --- infra (authoritative; read once at startup) ---

scheduling:
  nodepoolName: sei-node
  nodepoolArchive: sei-archive
  tolerationKey: sei.io/workload
  serviceAccount: seid-node

storage:                  # no sizePerf — perf is a storage-class tier only
  classPerf: gp3-10k-750
  classDefault: gp3
  classArchive: gp3-archive
  sizeDefault: 2000Gi
  sizeArchive: 40Ti

resources:
  cpuArchive: "48"
  memArchive: 448Gi
  cpuDefault: "4"
  memDefault: 32Gi

snapshot:
  bucket: sei-snapshots
  region: us-east-2

resultExport:
  bucket: sei-shadow-results
  region: us-east-2
  prefix: shadow-results/

genesis:
  bucket: sei-k8s-genesis
  region: us-east-2

images:
  sidecar: ghcr.io/sei-protocol/seictl@sha256:...
  kubeRBACProxy: quay.io/brancz/kube-rbac-proxy:v0.19.1
  cosmosExporter: ghcr.io/sei-protocol/sei-cosmos-exporter@sha256:...
```

A present-but-unparseable file is a hard startup error. A required infra field
unset in the file fails `Config.Validate` at startup, naming the file key.
