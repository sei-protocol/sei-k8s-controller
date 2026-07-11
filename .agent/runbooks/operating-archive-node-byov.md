# Operating an Archive Node with Bring-Your-Own-Volume

**Audience:** operators bringing up a Sei archive node from a pre-populated EBS volume (the "BYOV" / `dataVolume.import` path).
**Scope:** required volume contents, required CRD spec, controller validation surface, archive-specific gotchas, and the cutover sequence for swapping the underlying EBS.
**Not in scope:** *why* the import feature exists or how it is implemented inside the controller — see [`design-seinode-import-volume.md`](design-seinode-import-volume.md) and [`design-seinode-import-volume-lld.md`](design-seinode-import-volume-lld.md).

The archive use case has one mechanical difference from the validator/full-node BYOV path: the underlying EBS holds **multi-terabyte chain history (~18 TiB at current pacific-1)**, populated out-of-band on a separate EC2 instance, then handed to the controller. Everything else — the CRD, the validation, the sidecar tasks — is the same shape as any other imported volume.

---

## 1. Success criteria

An archive SeiNode is healthy when:

| Resource | State |
|---|---|
| `PersistentVolume <name>` | `STATUS=Bound`, `CLAIM=<ns>/<name>`, `RECLAIM POLICY=Retain` |
| `PersistentVolumeClaim <name>` | `STATUS=Bound`, `VOLUME=<pv-name>`, `CAPACITY` >= the node mode's required size |
| `SeiNode.status.conditions[ImportPVCReady]` | `Status=True`, `Reason=PVCValidated` |
| `Pod` init container `seid-init` log | `data directory already initialized, skipping seid init` |
| seid main container | catching up; height climbing past the seeded height within minutes of pod start |

If `ImportPVCReady=False` with `Reason=PVCInvalid`, the plan is in a terminal failure — the operator must fix the volume/PVC and the SeiNode must be re-created. `Reason=PVCNotReady` is transient and resolves on retry.

---

## 2. Volume filesystem layout

The PV is mounted into the pod at `/home/nonroot/.sei` (see `internal/platform/platform.go` — `DataDir = HomeDir + "/.sei"` with `HomeDir = "/home/nonroot"`). The data dir is a child of the container's `$HOME`, so an ad-hoc bare `seid` (e.g. `kubectl exec ... seid status`) resolves it with no `--home` flag; the controller-built start command still passes `--home /home/nonroot/.sei` explicitly. The init container's "already initialized" gate is a single-file existence check:

```bash
# from internal/noderesource/noderesource.go
if [ -f /home/nonroot/.sei/config/genesis.json ]; then
    echo "data directory already initialized, skipping seid init"
else
    seid init <chain-id> --chain-id <chain-id> --home /home/nonroot/.sei --overwrite   # DESTROYS DATA
fi && mkdir -p /home/nonroot/.sei/tmp
```

That gate is everything. **Without `/home/nonroot/.sei/config/genesis.json` on the volume, the init container clobbers the chain data on first boot.**

The required tree at the root of the formatted EBS volume:

```
<volume-root>/
├── config/
│   └── genesis.json         REQUIRED — controller's "already initialized" gate.
│                            Any valid genesis for the target chain works; the seictl
│                            sidecar's configure-genesis task may overwrite it on first
│                            boot with the chain's canonical content.
├── data/                    REQUIRED — chain state, bulk of the volume.
│   ├── application.db/         (cosmos-sdk app metadata, small)
│   ├── blockstore.db/          (CometBFT raw block bodies — typically 8-12 TiB on pacific-1)
│   ├── pebbledb/               (SeiDB SC state-store — 4-6 TiB on pacific-1)
│   ├── receipt.db/             (EVM receipts — 1-2 TiB on pacific-1)
│   ├── state.db/               (CometBFT consensus state)
│   ├── tx_index.db/            (tx-hash → height index)
│   ├── snapshots/              (state-sync snapshots)
│   └── priv_validator_state.json
└── wasm/                    REQUIRED if any wasm contracts have been instantiated.
    └── wasm/state/wasm/        (compiled .wasm modules)
```

Files **not** included on the volume: `config/app.toml`, `config/config.toml`, `config/node_key.json`, `config/priv_validator_key.json`. The seictl sidecar's `configure-genesis` and `config-apply` tasks (re)render those from the `SeiNode` spec on first boot. Including stale copies is harmless but pointless — they get overwritten.

### Filesystem requirements

- xfs is the supported fsType (matches `csi.fsType: xfs` on the PV).
- Mount path inside pod: `/home/nonroot/.sei` (the container's `$HOME/.sei`) — populate `config/`, `data/`, `wasm/` directly at the EBS volume root; do **not** nest a further `.sei/` subdirectory.
- Permissions: seid runs **nonroot** (UID/GID 65532), not root. A `root:root` volume from the populating instance is fine — the pod sets `fsGroup=65532` with `fsGroupChangePolicy: OnRootMismatch`, so on first mount the kubelet recursively `chown`s the root-owned tree to GID 65532 and adds group-write (`chmod g+rwX`), giving the nonroot seid process write access. On a multi-TB archive that first-mount walk is slow, but — unlike §6.4's SELinux relabel, which recurs on every pod move under RWO — it is genuinely one-time: later mounts find GID 65532 and skip it.

---

## 3. Required PV / PVC manifest

Static binding pattern (no dynamic provisioning, no storage class):

```yaml
---
apiVersion: v1
kind: PersistentVolume
metadata:
  name: <pv-name>                            # convention: chain-role-ordinal, e.g. pacific-1-archive-0
  labels:
    sei.io/chain: <chain-id>
    sei.io/role: archive
    sei.io/ordinal: "0"
spec:
  capacity:
    storage: <size>                          # match the EBS volume size, e.g. 40Ti
  volumeMode: Filesystem
  accessModes:
    - ReadWriteOncePod                       # preferred for archives (see §6.4) — activates
                                             # SELinuxMountReadWriteOncePod, skips the ~20-min
                                             # recursive setxattr walk on multi-TB volumes.
                                             # ReadWriteOnce is also accepted by the validator.
  persistentVolumeReclaimPolicy: Retain      # required — preserves EBS across PVC lifecycle
  storageClassName: ""                       # required empty for static binding
  csi:
    driver: ebs.csi.aws.com
    volumeHandle: vol-xxxxxxxxxxxxxxxxx      # the populated EBS volume id
    fsType: xfs
  nodeAffinity:                              # required — restricts pod to AZ where EBS lives
    required:
      nodeSelectorTerms:
        - matchExpressions:
            - key: topology.ebs.csi.aws.com/zone
              operator: In
              values:
                - <az>                       # e.g. eu-central-1b
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: <pvc-name>                           # convention: same as PV name
  namespace: <chain-namespace>               # same namespace as the SeiNode
  labels:
    sei.io/chain: <chain-id>
    sei.io/role: archive
    sei.io/ordinal: "0"
spec:
  accessModes:
    - ReadWriteOncePod                       # must match the PV's access mode
  resources:
    requests:
      storage: <size>                        # match PV capacity exactly
  storageClassName: ""
  volumeName: <pv-name>                      # explicit binding to the PV above
```

**Hard requirements** (the controller's `validateImport` enforces these — see §5):

- PVC must be in the **same namespace** as the SeiNode.
- PVC must be **`Bound`** at the time the SeiNode reconciles.
- PVC must have **`ReadWriteOnce` or `ReadWriteOncePod`** in `accessModes`. RWOP is preferred for new archives — see §6.4.
- PVC's `status.capacity.storage` must be **>= the node mode's required size** (`noderesource.DefaultStorageForMode`).
- PV's `Spec.Capacity.storage` must **exactly match** the PVC's reported capacity.
- PV must not be in phase `Failed`; PVC must not be in phase `Lost`.
- PVC must not have a `deletionTimestamp`.

---

## 4. Required SeiNode spec

An archive is always a **single, standalone SeiNode**. `dataVolume.import.pvcName` is the only new field — it names a PVC in the SeiNode's namespace; everything else about how the volume is sourced is the operator's responsibility. (A SeiNetwork is *not* an archive path — see "What the spec deliberately does not contain" below.)

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNode
metadata:
  name: archive-0
  namespace: <chain-namespace>
spec:
  chainId: <chain-id>
  image: ghcr.io/sei-protocol/sei:<tag>

  dataVolume:
    import:
      pvcName: <pvc-name>                    # the PVC from §3, same namespace

  archive: {}                                # selects archive mode (see §6)
```

### What the spec deliberately does not contain

- `entrypoint` / `command` / `args` — there is no such field on the SeiNode CRD. The controller builds the seid command itself (`seid start --home /home/nonroot/.sei`, in `internal/noderesource/noderesource.go`), so the container args and `--home` are not operator-settable and must not be pinned in the spec.
- `dataVolume.size`, `dataVolume.storageClassName` — there is no such field. Storage spec is on the PV/PVC.
- `dataVolume.import.namespace` — the PVC must be in the SeiNode's namespace, no cross-namespace imports.
- A snapshot-restore field — archive mode doesn't take snapshots from S3; it ingests history from the imported volume and continues from there. (`SnapshotSource()` in `seinode_types.go` returns `nil` for archive nodes.)
- A **SeiNetwork** — a SeiNetwork bootstraps a *genesis* validator pool (it mints new genesis state) and structurally has no `template`/`archive` field, so it cannot run an archive node, which joins an existing chain. Archive BYOV is always a standalone SeiNode. (`api/v1alpha1/seinetwork_types.go`)

### Immutability

`spec.dataVolume.import.pvcName` is immutable on a SeiNode (CEL: `self == oldSelf`). To re-point at a different PVC, delete and recreate the SeiNode.

---

## 5. Controller validation surface

The `ensure-data-pvc` task takes the import path when `spec.dataVolume.import.pvcName` is set. It validates the PVC every reconcile and writes the `ImportPVCReady` condition.

| Condition outcome | Reason | Meaning | Operator action |
|---|---|---|---|
| `True` | `PVCValidated` | All checks pass | none — task moves on |
| `False` | `PVCNotReady` | Transient: PVC not Bound, no `status.capacity` yet, PV transient state | wait — controller retries |
| `False` | `PVCInvalid` | Terminal: PVC missing, wrong access mode, capacity mismatch, PV failed, etc. | fix the PVC/PV and re-create the SeiNode |

The validation is in `internal/task/ensure_pvc.go::validateImport`. Read the function before chasing edge cases — it is small.

When `ImportPVCReady=True`, the controller never deletes the imported PVC. SeiNode finalizer's `deleteNodeDataPVC` is bypassed for imported PVCs. EBS lifecycle is **fully** the operator's responsibility.

---

## 6. Archive-mode-specific concerns

### Receipt-store pruning (current upstream gotcha)

`sei-chain` defaults the EVM receipt-store to `keep-recent=100000` — meaning the seictl sidecar's `config-apply` task, which renders `app.toml` from `sei-config` templates, must emit a `[receipt-store]` section that overrides this for archive mode. Otherwise seid prunes historical receipts on first boot, defeating the point of an archive.

What the rendered `app.toml` must contain:

```toml
[receipt-store]
keep-recent = 0
prune-interval-seconds = 0
```

Operators staging a volume out-of-band on EC2 typically already set this in the source seid's `app.toml`. That source `app.toml` is **not** what the K8s pod uses — the seictl sidecar regenerates `app.toml` from `sei-config` templates on every pod boot. The override has to be in `sei-config`'s archive-mode template, not on the volume.

### State-sync vs. import vs. blocksync

Archive mode does not use state-sync (`SnapshotSource() == nil`). The pod boots against the imported volume's existing state, then resumes blocksync from `latest_block_height` to chain tip. Quote: "if you seeded the volume at height H, the pod takes (tip - H) / blocks_per_second to catch up."

### Storage sizing

The required size comes from `noderesource.DefaultStorageForMode(NodeMode(node), platform)`. For archive mode that is significantly larger than full-node mode — pacific-1 currently lands around 18-20 TiB and grows; size the PV/PVC at 30-40 TiB to leave headroom. Underprovisioning manifests as `Reason=PVCInvalid` with "capacity X is less than required Y."

### Single-AZ failure mode

EBS RWO is bound to one AZ. If the K8s node holding the pod fails, the pod can only reschedule onto another node in the same AZ (`nodeAffinity` on the PV enforces this). Loss of an entire AZ means the archive is offline until AZ recovers. Acceptable for archive nodes, which are not in any consensus path.

### 6.4 SELinux mount labeling — use `ReadWriteOncePod`

On SELinux-enforcing nodes (Bottlerocket on EKS), the kubelet applies per-pod MCS labels to the data volume at mount time. With the default `ReadWriteOnce` access mode, this is a **recursive setxattr walk over every inode** — on a multi-TB archive (40 TiB / 8.2M files on pacific-1), it takes ~20 minutes per pod recreation and runs every chain upgrade, image bump, or node move.

The fix: declare the PV/PVC with `accessModes: [ReadWriteOncePod]` instead. RWOP activates `SELinuxMountReadWriteOncePod` (GA since K8s 1.27, default-on), which applies the SELinux context as a per-mount option in milliseconds rather than walking inodes.

Why not `seLinuxChangePolicy: MountOption` on the pod? That requires the upstream `SELinuxMount` feature gate at the API server, which is default-off in K8s 1.33–1.36 and is not exposed to customers on managed EKS ([containers-roadmap#512](https://github.com/aws/containers-roadmap/issues/512)). RWOP sidesteps that constraint entirely.

**For new archives**: declare RWOP from the start in §3's manifest. archive-1 (pacific-1, eu-central-1a) and archive-2 (eu-central-1c) ship this way. No migration cost.

**For existing archives running RWO** (e.g. pacific-1's `archive-0` was created with RWO): migrate at the next natural pod-recreation event (next chain upgrade, image bump, etc.) — pod is going to recreate anyway, fold the access-mode swap into the same window.

#### Migration procedure (RWO → RWOP, ~5–10 min downtime)

`accessModes` is immutable on a bound PV/PVC, so the migration is a delete-and-recreate sequence. The underlying EBS volume is preserved by `Retain` reclaim policy and never touched.

```bash
# 1. Stop the pod (StatefulSet recreates it; new pod stays Pending until PVC is back)
kubectl -n <chain-namespace> delete pod <archive-pod-name>

# 2. Delete the PVC (PV moves Bound → Released; EBS volume preserved)
kubectl -n <chain-namespace> delete pvc <pvc-name>

# 3. Clear the PV's claimRef so it's bindable again (PV moves Released → Available)
kubectl patch pv <pv-name> --type json \
  -p '[{"op":"remove","path":"/spec/claimRef"}]'

# 4. Patch PV.spec.accessModes — editable now because PV is Available
kubectl patch pv <pv-name> --type json \
  -p '[{"op":"replace","path":"/spec/accessModes","value":["ReadWriteOncePod"]}]'

# 5. Land + apply the PVC manifest update (also RWOP).
#    Under GitOps, merge the platform-repo PR; flux reconciles and recreates the PVC.
#    PV → PVC re-binds via volumeName; StatefulSet's Pending pod proceeds.
flux reconcile kustomization clusters/prod/protocol/<chain-id>
```

What stays the same across the migration:

- **EBS volume** — never touched; `Retain` reclaim + the PV's `volumeHandle` re-references the same AWS volume.
- **Names** — PV name, PVC name, EBS volume id all preserved. SeiNode → PVC → PV → EBS chain re-resolves automatically.
- **SeiNode spec** — no change; references PVC by name.

Verification after the new pod is Running:

```bash
# Confirm RWOP is in effect on the bound objects
kubectl get pv <pv-name> -o jsonpath='{.spec.accessModes}'   # expect ["ReadWriteOncePod"]
kubectl get pvc -n <ns> <pvc-name> -o jsonpath='{.spec.accessModes}'  # expect ["ReadWriteOncePod"]

# Confirm the kernel applied SELinux as a mount option (not via recursive walk)
kubectl debug <pod-name> -c slx-check --target=seid --image=alpine:3.20 --profile=netadmin -- \
  sh -c 'mount | grep "/home/nonroot/.sei "'
# expect output to include  context=system_u:object_r:container_file_t:s0:c<N>,c<M>
```

If the `mount` line shows a `context=...` parameter, per-mount labeling is active and pod-start is fast (no 20-minute walk). If not, double-check the PV/PVC accessModes were both changed.

---

## 7. Cutover: swapping the underlying EBS

`PV.spec.csi.volumeHandle` is **immutable** on a bound PV. To re-point an archive at a freshly populated EBS, the PV must be deleted and recreated.

```text
1. Delete the SeiNode (its pod terminates; the imported PVC is preserved — see §5)
2. Wait for the pod to terminate; CSI auto-detaches the EBS
3. Detach the new EBS from the EC2 instance that populated it
4. Delete the PV (Retain reclaim → both old and new EBS volumes survive)
5. Update the PV manifest's volumeHandle to the new EBS volume id
6. Apply the manifest (PV recreated; PVC re-binds via volumeName)
7. Recreate the SeiNode
8. Watch init container — must log "data directory already initialized"
9. Watch seid — height should climb past the seeded height within minutes
```

If the GitOps tool driving the cluster (flux, argo) has `prune: true` and the kustomization is the source of truth for both PV and PVC, an alternative pattern works:

1. Comment out the `volumes/` entry from the kustomization → GitOps removes PV+PVC
2. Update the volumeHandle in the (now-disabled) PV manifest
3. Un-comment the `volumes/` entry → GitOps recreates PV+PVC with the new handle

Either way, no kubectl-side surgery on the PV is required; the immutability constraint is satisfied via delete-and-recreate.

---

## 8. Common pitfalls

| Symptom | Root cause | Fix |
|---|---|---|
| Pod logs `seid init pacific-1 ...` then chain data is gone | `/home/nonroot/.sei/config/genesis.json` missing on volume | Re-stage volume with the gate file present |
| Pod `Pending`, events show `failed to attach: VolumeBinding ... AZ mismatch` | PV `nodeAffinity` AZ does not match an available node | Ensure EBS, PV `nodeAffinity`, and target node pool are all in the same AZ |
| Pod `Pending`, events show `multi-attach error` | EBS still attached to the EC2 instance that populated it | `aws ec2 detach-volume` from the source instance, wait for `available` |
| `ImportPVCReady=False, Reason=PVCInvalid, msg=capacity X < required Y` | EBS volume undersized for archive mode | Recreate EBS at required size, fpsync, swap |
| `ImportPVCReady=False, Reason=PVCInvalid, msg=PV ... is in phase Failed` | Underlying EBS provisioning or attachment broke | Inspect EBS state in AWS console; recreate PV after fixing |
| `ImportPVCReady=False, Reason=PVCNotReady` and the PVC stays `Pending` | PV `nodeAffinity` matches no schedulable node, or PVC's `volumeName` points at a non-existent PV | Verify PV exists and matches an available AZ |
| seid catches up briefly then receipts disappear from query | sei-config archive-mode template missing `[receipt-store]` override | Land the upstream `sei-config` fix; bounce the pod |
| Init container clobbers a freshly-populated volume on the second boot | Volume populated from a source where `seid init` was rerun and removed `genesis.json` | The gate file must survive the populator's full lifecycle — verify before detaching |

---

## 9. Pre-flight checklist

Before applying the SeiNode manifest:

- [ ] EBS volume created in the target AZ, formatted xfs, populated with the layout in §2.
- [ ] EBS detached from any source EC2 instance (AWS console shows `available`).
- [ ] PV manifest applied with the correct `volumeHandle`, `nodeAffinity` AZ, and capacity.
- [ ] PV and PVC `accessModes` set to `[ReadWriteOncePod]` (preferred — see §6.4) or `[ReadWriteOnce]`.
- [ ] PVC manifest applied with matching `volumeName` and same namespace as the SeiNode.
- [ ] `kubectl get pv,pvc` shows both `Bound`.
- [ ] (If GitOps) the source-of-truth branch is the one driving the cluster — no drift.
- [ ] `sei-config` archive-mode template emits `[receipt-store] keep-recent=0` (otherwise the receipts get pruned regardless of how good the seeded data is).
- [ ] Target node pool has capacity in the matching AZ for an EBS-backed pod.
