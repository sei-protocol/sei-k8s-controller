# Validating flatKV↔memIAVL Parity via Sharded Historical Replay

**Audience:** operators/agents driving a flatKV-vs-memIAVL storage-engine correctness validation on the harbor cluster, at scale (50+ shards across a chain's block space).
**Scope:** the end-to-end method for one shard and the fan-out to many — the replay-pair topology, the load-bearing correctness prerequisites, standing up the pair, running the seictl shadow comparator, reading/aggregating results, and generating a Notion report. Leans on the `/harbor-dev` skill for the render→PR→Flux mechanics.
**Not in scope:** *why* flatKV / the migrate_evm migration exists or how the storage engines are implemented — see `docs/` and the sei-db source. The **seidb logical-content digest** track (the separate committed-root validation) is a companion effort, not this.

This runbook exists because the naïve version of this validation is **silently wrong** in three independent ways. Read §1 and §2 before touching anything — they are the difference between a real result and a green run that measured nothing.

## 0. Substitutions used throughout

Resolve these once; they recur in node names, manifests, peer strings, and the results prefix. The ones with an external source (you cannot invent them) are marked.

| Placeholder | Meaning / where its value comes from |
|---|---|
| `<alias>` | your harbor engineer alias; the namespace is `eng-<alias>` (this session: `fromtherain`). |
| `<shard>` | a stable per-shard id you choose (e.g. `s079m`), reused in both node names, the kustomization entries, and the results prefix. |
| `<tag>` | the sei-chain git ref/tag the image was built from (§3). |
| `<ECR>` | `189176372795.dkr.ecr.us-east-2.amazonaws.com`. |
| `<shard-start-height>` | the snapshot height chosen in §6 step 1. |
| `<archive-node-id>@<archive-p2p-dns>` | **(external)** the full-history archive's CometBFT node-id and its p2p DNS — read from the archive's `/status` (`.node_info.id`) and its headless Service DNS (§5). |
| `<shard-node-id>@<shard-p2p-dns>` | **(external)** a shard node's own node-id + p2p DNS — only needed for the archive-dials-shard direction (§5). |
| `<flatkv-node>` / `<memiavl-node>` | `pacific1-flatkv-replayer-<shard>` / `pacific1-memiavl-replayer-<shard>` (§4). |
| `<behind-node>` / `<ahead-node>` | resolved **at comparator-submit time** from live heights (§7) — not fixed. |
| `<archive>` | the full-history archive's SeiNode name (its pod is `<archive>-0`); the block source (§5). |
| `<you>` | the `X-Remote-User` identity the sidecar records for audit — use your `<alias>`. |
| `<task-uuid>` | runtime output: the `id` returned by the `result-export` POST (§7). |
| `<results-bucket>` / `<snapshot-bucket>` | the S3 results bucket and the state-sync snapshot bucket (from the sidecar's platform env). |

---

## 1. Mental model (read this first)

The goal is to prove the **flatKV** storage engine produces byte-identical **execution results** to **memIAVL** for the same real chain history. The method is differential replay:

> For each shard of the chain's block space, run **two** replay nodes on the **same seid binary** and the **same bootstrap snapshot** — one `write_mode=migrate_evm` (flatKV), one `write_mode=memiavl_only` (memIAVL). Both fetch the identical block stream from a shared full-history **archive** over p2p and re-execute it. Compare the **two replay nodes against each other**.

> **What this method does and does not validate (read before believing a green run).** The genuine flatKV signal is **L1 — per-tx execution results** (`block_results`: code/gas/log/events), produced at DeliverTx time *when flatKV was live in the execution (SC) path*. flatKV's committed **root** diverges by design (schedule-dependent) and is the separate **seidb logical-digest** track. **L2 (`eth_getStorageAt`/balance/code/nonce at a historical height) does NOT exercise flatKV** on these nodes: SeiDB serves *all historical IAVL reads from the State Store (SS/pebbledb), not the SC layer where `write_mode` chooses flatKV vs memIAVL* (`sei-cosmos/storev2/rootmulti/store.go` `CacheMultiStoreWithVersion` — "Serve from SS stores for ALL historical queries" when SS is enabled). SS is identical on both nodes (same execution history), so **a green L2 is a same-history sanity check, guaranteed by construction — never read it as flatKV parity.** To exercise the flatKV *read* path directly you must run with SS disabled and read at **latest** height (SC is only consulted for the latest version when SS is off), or rely on L1 + the seidb-digest track. §8 restates this at the point of consumption.

The three load-bearing facts, each of which is a trap if violated:

1. **Compare the two replay nodes to each other — NEVER to the archive.** The archive serves *stored* results produced by whatever (older) binary was live when those blocks were originally committed; your replay nodes re-execute with a *current* binary. Comparing a replay node to the archive conflates three variables:
   - **the storage engine** — what you want to measure;
   - **version drift** — gas-schedule / execution / decoder changes since those blocks were committed;
   - **re-execution vs stored results.**

   It manufactures false "divergences." **The archive is only the block *source* + a version-drift reference.** Holding the binary and snapshot identical across the flatKV/memIAVL pair is what isolates the storage engine as the *only* difference.

2. **A `migrate_evm` node does not serve EVM reads from flatKV until its migration completes** (§2). It is a boundary-split router, not a pure flatKV store. Until migration completes, EVM reads fall to memIAVL — so the flatKV node and the memIAVL node both read memIAVL, and the comparison is **vacuous** (silently: it looks green). You MUST verify migration is complete (`sei_chain_seidb_migration_version == target`) before trusting any result.

3. **Pre-v6.5 blocks need the `historical_replay` build tag** (§3), or the current binary's strict tx decoder rejects historically-non-canonical tx bodies (`code 2 tx parse error`), silently *skipping* their execution and diverging replayed state from history. **This tag is not yet on sei-chain `main`** — building from `main` makes it a no-op and reintroduces this trap; you must build from the lenient-decoder branch (§3).

If all three hold, a `match=false` from the comparator is a **genuine flatKV divergence** — the signal the effort exists to find.

---

## 2. The correctness gate: is the flatKV node actually reading from flatKV?

`write_mode=migrate_evm` builds a **migration router** for the `evm/` module (`sei-db/state_db/sc/migration/`), not a pure flatKV store. (The boundary split governs the `evm/` module only; **all non-EVM modules always read memIAVL on both nodes** — so this method validates EVM-module parity.)

- **Reads** (`MigrationManager.Read`): an EVM key routes to flatKV only if it is `<=` a lexicographically-advancing **migration boundary cursor**; otherwise it reads memIAVL first, with flatKV as a miss-fallback. `eth_getStorageAt` (EVM StateDB → `"evm"` KVStore → `RouterCommitKVStore.Get` → `MigrationManager.Read`) goes through exactly this.
- **Writes** are single-backend per key (not dual-write): migrated/newly-created keys → flatKV, un-migrated existing keys stay in memIAVL until the cursor reaches them.
- The cursor **auto-advances** `sc-keys-to-migrate-per-block` (app.toml `[state-commit]`, default **1024**) keys/block. When it has swept the entire EVM keyspace, the persisted migration version flips from start (0) to **target (1 for migrate_evm)**, and the node comes up in passthrough: **all EVM reads route to flatKV.**

**Therefore a `migrate_evm` node routes *latest-version* EVM reads to flatKV ONLY once migration is complete.** (Scope caveat, ties to §1: this governs the **latest-version SC** read path. It does **not** make the comparator's L2 meaningful — L2 reads at *historical* heights, which SeiDB serves from SS regardless of `write_mode` or migration state. This gate matters for the SS-off + latest-height flatKV read test, and confirms the node's SC is genuinely flatKV; it is necessary-but-not-sufficient and cannot rescue L2 while SS is enabled.) The decisive check is a metric on seid's telemetry port (`:26660`). The on-node Prometheus exposition prefixes every metric with `sei_chain_` (raw OTel names drop it); use the prefixed names:

```bash
kubectl -n eng-<alias> port-forward pod/<flatkv-node>-0 26660:26660 &
curl -s localhost:26660/metrics | grep sei_chain_seidb_migration_version
#   == target version (1 for migrate_evm)  → migration COMPLETE → EVM reads come from flatKV ✅
#   == start version (0)                    → in progress        → EVM reads mostly memIAVL ❌ (comparison vacuous)
# corroborate:  sei_chain_seidb_migration_keys_migrated_total   (keys swept so far)
```

A `memiavl_only` node emits **no** `sei_chain_seidb_migration_*` metrics and only the `sei_chain_memiavl_*` family (metric scope `seidb_memiavl`, no `seidb_flatkv` scope) — that is the clean baseline (pure memIAVL, no flatKV instantiated).

### Whether migration completes depends on where the shard starts — this shapes the whole fan-out

The EVM keyspace grows over the chain's life. Migration must sweep whatever EVM state exists at the shard's bootstrap height:

- **Shard anchored at EVM genesis** (pacific-1 ≈ block 79.2M, just above EVM enablement): the EVM keyspace is tiny (~17k keys observed). Migration completes almost immediately at the default 1024/block — **flatKV reads for free.** This is the ideal anchor.
- **Shard anchored at a high height** (e.g. 205M+): the EVM keyspace is hundreds of millions of slots. `migrate_evm` will **not** complete over a bounded replay → EVM reads stay mostly memIAVL → **the comparison is vacuous for flatKV.** For these shards you MUST use one of:
  - `write_mode=evm_migrated` — all EVM reads/writes flatKV; **requires the EVM migration already completed** on the bootstrap state. **There is no runtime guard:** point `evm_migrated` at a memIAVL-history store and every EVM read returns not-found *silently* (no error).
  - `write_mode=flatkv_only` — all modules flatKV; **requires a flatKV-seeded PVC** (you cannot convert a memIAVL-history store to `flatkv_only` in place).
  - **Force completion** — raise `sc-keys-to-migrate-per-block` so the cursor sweeps the full keyspace during replay (expensive I/O at high heights).

**Practical fan-out consequence:** only the genesis-anchored shard gets flatKV read coverage for free. Every other shard needs a "make reads genuinely flatKV" step — either a flatKV-native snapshot (seed once, reuse) or a forced/pre-completed migration. Bake this into the shard plan; do not assume `migrate_evm` alone tests flatKV at arbitrary heights.

`write_mode` options relevant to the 0→1 EVM migration (full set incl. `auto` and the intermediate modes is in `sei-db/state_db/sc/types/write_mode.go`):

| write_mode | EVM read source | notes |
|---|---|---|
| `memiavl_only` | memIAVL | the parity baseline — pure memIAVL, no flatKV |
| `migrate_evm` | boundary-split (flatKV ≤ cursor, else memIAVL) → flatKV once **complete** | the migration under test; verify completion |
| `evm_migrated` | flatKV (all EVM) | requires migration already complete; no guard — reads empty silently if not |
| `flatkv_only` | flatKV (all modules) | requires a flatKV-seeded PVC |
| `test_only_dual_write` | memIAVL (writes both) | **prod-forbidden**; never use |

---

## 3. The binary: `mock_chain_validation` + `historical_replay`

Both replay nodes in a pair run the **same** image, built from the target chain binary with **two** build tags:

- **`mock_chain_validation`** — swallows the consensus app-hash divergence that re-execution produces (flatKV's committed root is schedule-dependent; a non-mock binary would halt at the first divergent block). Lets the node replay forward without halting; it keeps data/evidence-integrity halts intact. **Not `mock_balances`** — that build tag corrupts real-tx execution and must never be used for replay.
- **`historical_replay`** — activates the lenient tx decoder (a `NewTxConfigWithoutBodyBloatRejection` encoding-config that skips `rejectBloatedBody`) so pre-v6.5 blocks whose protobuf tx bodies are non-canonical **decode and execute** instead of being rejected `code 2 tx parse error`. Without it the current strict decoder skips those txs, silently diverging replayed state. The default/untagged build keeps the strict decoder on all production paths — lenient is reachable *only* via this build tag.

> **Build dependency — not on `main` yet (read before building).** As of this writing, `historical_replay` and its lenient encoding-config are **not on sei-chain `main`**; they land via the lenient-decoder change (sei-chain **PR #3691**). On `main` the only lenient decoder is `DefaultTxDecoderWithoutBodyBloatRejection`, wired into the `evmrpc` *trace* path — **not** the replay/DeliverTx execution path — so a `main` build ignores the tag and silently reverts to the strict decoder (trap #3). The combined-tag build target and the `GO_BUILD_TAGS` plumbing also ride on that change; `main`'s `ecr.yml` does not produce a `mock_chain_validation-historical_replay-*` image. **Until #3691 merges: build from its branch ref.** Once it merges, build from `main`.

Build the image via the sei-chain `ecr.yml` workflow (`workflow_dispatch`) with `ref` = the PR #3691 branch (**not `main`**, per the dependency note above) and `GO_BUILD_TAGS="mock_chain_validation historical_replay"`. The branch **must live on canonical `sei-protocol/sei-chain`** (the ECR OIDC is scoped there — fork branches fail AWS login; push the PR branch to canonical). Resulting tag shape: `<ECR>/sei/sei-chain:mock_chain_validation-historical_replay-<tag>`. Confirm the built image is genuinely lenient before trusting a run — the §11 `code 2 tx parse error` symptom is the failure signature if it isn't.

---

## 4. Per-shard node topology

A shard = **two `SeiNode`s** in `eng-<alias>`, identical except `write_mode` (and name/labels):

```yaml
# flatKV replay node — engineers/<alias>/pacific1-flatkv-replayer-<shard>/seinode.yaml
apiVersion: sei.io/v1alpha1
kind: SeiNode
metadata:
  name: pacific1-flatkv-replayer-<shard>
  namespace: eng-<alias>
  labels: { sei.io/chain: pacific-1 }
  # sei.io/role is intentionally NOT set here: the controller stamps sei.io/role=replayer
  # on the pod (deriveRole) regardless, so a user value would only create a CR-vs-pod
  # mismatch. (The controller's own StatefulSet selector keys on sei.io/node; the
  # sei.io/seinode podLabel below is our own ad-hoc label for comparator/port-forward selection.)
  annotations: { sei.io/networking-orphaned: "true" }   # signal to external networking tooling; the controller itself creates no external exposure for a SeiNode
spec:
  chainId: pacific-1
  image: <ECR>/sei/sei-chain:mock_chain_validation-historical_replay-<tag>
  # spec.sidecar OMITTED on purpose — the controller wires the cluster-default seictl (§7 depends on that build).
  overrides:
    storage.state_commit.write_mode: migrate_evm     # memiavl_only on the twin
    # archive profile: retain the whole replayed range + lift the EVM trace-lookback cap
    storage.pruning: "nothing"
    chain.min_retain_blocks: "0"
    storage.state_store.keep_recent: "0"
    storage.receipt_store.keep_recent: "0"
    storage.receipt_store.prune_interval_seconds: "0"
    evm.max_trace_lookback_blocks: "-1"
  podLabels: { sei.io/chain: pacific-1, sei.io/seinode: pacific1-flatkv-replayer-<shard> }
  peers:
    - static: { addresses: ["<archive-node-id>@<archive-p2p-dns>:26656"] }   # the shared block source (§5)
  replayer:
    snapshot:
      s3: { targetHeight: <shard-start-height> }   # the seictl sidecar restores the highest snapshot <= this height and syncs to it
      trustPeriod: 200000h0m0s   # >> snapshot age; too-short silently yields an empty import. Renders init-only — a wrong value needs a full SeiNode+PVC delete + re-bootstrap, not an edit.
```

**The memIAVL twin is identical except, exhaustively:** (a) `metadata.name` and `podLabels.sei.io/seinode` → `pacific1-memiavl-replayer-<shard>`; (b) `storage.state_commit.write_mode: memiavl_only`. Nothing else changes.

Why each override matters (all learned the hard way):

- **pruning off + `min_retain_blocks: 0`** — a tip-following node prunes to a ~100k-block window; the comparator then reads pruned heights and everything reads indeterminate. Archival retention is mandatory for a replay-and-compare node.
- **`evm.max_trace_lookback_blocks: -1`** — L2 touched-key resolution runs `debug_traceBlockByNumber`, which is capped by default (a finite cap surfaces as `beyond max lookback of N`); `-1` = unlimited.
- **`spec.sidecar` omitted** — pinning a sidecar image obscures failures and drifts from the cluster default; the controller wires the correct seictl (§7's comparator contract depends on that build). See `/harbor-dev` guardrail 7.
- **`replayer` mode** (not `fullNode`) — the dedicated replay mode; CEL enforces mode-exclusivity (exactly one of fullNode/archive/replayer/validator) and peer presence; the S3-snapshot requirement is enforced by the controller's planner. Config renders init-only (§10).

---

## 5. The shared archive (block source, not baseline)

One **full-history archive** (`earliest_block_height == 1`) serves blocks to *every* shard over p2p. Its role is now cheap — sequential block-store reads for blocksync — not the per-block, per-key RPC/trace load of the old "compare-against-archive" design. A single archive fans out to many shards; the constraints are p2p peer count (`max_num_inbound_peers`, default ~40, tunable) and blocksync bandwidth, both with large headroom because replay is execution-bound (~40 blocks/s/node), not fetch-bound.

**Peer by having each shard dial the archive**, not the archive dial each shard:

- Put the archive as a `static` peer in each shard's `spec.peers` (as in §4), using `<archive-node-id>@<archive-p2p-dns>:26656`. Adding a shard then needs **zero archive-side change**.
- The public prod peers (state-syncers / snapshotters) **prune** and cannot serve deep history — a shard bootstrapped below their retention will sit **stuck** (`"no progress since last advance"`, blockstore height 0) until it peers the full-history archive. This is the #1 "why isn't it replaying" cause.
- If you must peer from the archive side instead (the reverse direction from §4 — here you need the *shard's* node-id/DNS), edit the archive's running `config.toml` persistent-peers and restart seid. The archive's seid container (release image) has a shell:
  ```bash
  kubectl -n eng-<alias> exec <archive>-0 -c seid -- sh -c \
    "sed -i \"/^persistent-peers = /s#'\$#,<shard-node-id>@<shard-p2p-dns>:26656'#\" /.sei/config/config.toml"
  ```
  **Then restart seid only — not the pod.** Submit a `restart-seid` task to the archive sidecar (§7). Deleting the pod re-runs init, which re-renders `config.toml` and drops your edit.

---

## 6. Standing up a shard pair (step-by-step)

Uses the `/harbor-dev` render→PR→Flux flow. Per pair:

1. **Pick the snapshot.** List `s3://<snapshot-bucket>/pacific-1/state-sync/` and choose the snapshot nearest (at or just above) the shard's start height. The seictl sidecar restores the highest snapshot `<= targetHeight`. Anchor the first shard just above EVM genesis (§2).
2. **Render + PR both SeiNodes** into `engineers/<alias>/…` (flatKV + memIAVL twin), add both to the `fromtherain` kustomization `resources`, PR to `harbor-engineering-workspace`.
3. **Merge, then reconcile the *workspace-repo* source** (the `fromtherain` Kustomization's source is the separate `harbor-engineering-workspace` GitRepository, **not** the platform `flux-system` source):
   ```bash
   flux --context harbor reconcile source git harbor-engineering-workspace -n flux-system
   flux --context harbor reconcile kustomization fromtherain -n eng-<alias>
   ```
   **Do not run `flux reconcile --with-source` on the root `flux-system` kustomization** — that re-pulls the platform source and re-rolls every component in the cell.
4. **Verify both bootstrap + replay.** Each restores its snapshot, then blocksyncs forward from the shared archive. Confirm heights climb (`/status` `latest_block_height`). Note `/status` on these nodes returns **unwrapped** JSON (parse `d.get('result', d)`).
5. **GATE — verify the flatKV node's migration is complete** (§2): `sei_chain_seidb_migration_version == target`. If not (high-height shard), reconfigure per §2 before comparing. This gate is the difference between a real and a vacuous result.
6. If the shard sits below prod-peer retention and isn't advancing, ensure the archive static peer is present (§5).

---

## 7. Running the shadow comparator (seictl `result-export`)

**Precondition (do not skip):** the flatKV node's migration must be complete before this task means anything — confirm `sei_chain_seidb_migration_version == target` (§2 / §6 step 5) first. Comparing before completion produces a green run that measured memIAVL against memIAVL. This §7 also requires the cluster-default seictl sidecar to carry the **layered comparator** (migrationMode + L2); if the sidecar image ever rolls to a build without it, the `migrationMode`/`shadowEvmRpc`/`canonicalEvmRpc`/`traceRpc` params degrade to a plain block-results export. (The CRD field `spec.replayer.resultExport.shadowResult` exists but is intentionally unused here — it takes only `canonicalRpc` and halts on first divergence; the manual task gives the L1/L2 + survey control this method needs.)

**The invariant that decides where you submit:** `canonicalRpc` (and `canonicalEvmRpc`) MUST point at the node that already holds every height being compared — otherwise the canonical query errors on a missing height mid-run and the run is wasted. Because both nodes climb independently (~40 blocks/s) and *either* can be ahead (the roles are not fixed — §0), resolve them **at submit time**: read `/status` `latest_block_height` on **both** nodes (unwrapped JSON — `d.get('result', d)`, §6 step 4) immediately before submitting, then submit the `result-export` task to whichever is **currently behind** with `canonical*` set to the **currently ahead** node. If the two are level, wait until they diverge by at least one page (100 blocks) before submitting.

The seictl sidecar runs as a **native sidecar** (`seictl serve`, port 7777) on every SeiNode — long-running, controller-wired, with full platform env.

Reach the sidecar API (it binds `127.0.0.1:7777`, fronted by kube-rbac-proxy; the seid/sidecar containers are distroless — no shell):

```bash
kubectl -n eng-<alias> port-forward pod/<behind-node>-0 7777:7777 &   # <behind-node> = the node you just confirmed is behind; re-check /status if any time has passed

curl -s -X POST -H 'X-Remote-User: <you>' -H 'Content-Type: application/json' \
  http://localhost:7777/v0/tasks -d '{
    "type":"result-export",
    "params":{
      "bucket":"<results-bucket>",
      "region":"eu-central-1",
      "prefix":"shadow-results/flatkv-vs-memiavl-<shard>/",   # fresh per run/shard — never reuse (overwrites pages)
      "canonicalRpc":"http://<ahead-node>.eng-<alias>.svc:26657",
      "migrationMode":true,          # flatKV root differs by design → makes AppHash informational AND lets the run not halt on the expected per-block AppHash divergence
      "continueOnDivergence":true,   # survey mode: record divergent blocks and keep going instead of halting on the first genuine L1/L2 divergence
      "shadowEvmRpc":"http://localhost:8545",                       # L2: local (behind) node EVM RPC
      "canonicalEvmRpc":"http://<ahead-node>.eng-<alias>.svc:8545", # L2: ahead node EVM RPC
      "traceRpc":"http://localhost:8545"                            # touched-key traces (unlimited lookback set in §4)
    }}'
# → {"id":"<task-uuid>"}   ;  GET /v0/tasks/<task-uuid> for status ; /v0/healthz|metrics are auth-bypass
```

The task compares from the node's snapshot height forward and follows as it replays, writing `{start}-{end}.compare.ndjson.gz` pages (100 blocks each). Each record carries `match`, `layer0` (AppHash/LastResultsHash — informational in migration mode), `layer1` (per-tx `code`/`gasUsed`/`log`/`events` — the flatKV verdict, §1), and `layer2` (per-touched-key `storage`/`balance`/`code`/`nonce`; `indeterminate` is a per-layer flag on the L1/L2 result, set when that layer couldn't resolve — a same-history sanity check, not a flatKV signal, §1/§8).

**Expected benign edge:** the first block after the snapshot (`<snapshot+1>`) is L2-indeterminate — building its EVM query context needs the parent block's validators (`<snapshot>`, the pre-snapshot base, absent from the blockstore: `height is not available`). One block; not a divergence.

---

## 8. Reading + aggregating results

`seictl report list` / `seictl report divergence` read the pages from S3. For a whole-run verdict, sync the prefix and aggregate:

```bash
aws s3 sync s3://<results-bucket>/shadow-results/flatkv-vs-memiavl-<shard>/ ./<shard>/ --region eu-central-1
# then per record: count match=true/false; for match=false split by divergenceLayer and by
# layer2.indeterminate (benign) vs determinate (real); cluster real divergences by height/contract/kind.
```

Interpretation, given the gates in §1–§2 hold — **L1 is the verdict; L2 is not** (§1):

- **L1 `match=true` across the range** → flatKV ≡ memIAVL at the **execution-result** level for those blocks. This is the parity result this method delivers.
- **Determinate L1 `match=false`** (a real per-tx `code`/`gasUsed`/`log`/`events` mismatch, not the boundary block) → a **genuine flatKV divergence**: record the height and the tx/receipt fields. This is the finding.
- **L2 (`layer2`) is a same-history sanity check, not a flatKV signal.** On these SS-enabled nodes L2 reads the State Store on both sides (§1), so a green L2 is expected and means nothing about flatKV; a determinate L2 `match=false` here would indicate an SS/replay divergence between the two nodes, not a flatKV read-path bug. Do **not** report L2 parity as flatKV parity.
- **`layer2.indeterminate`** beyond the boundary block → a config miss (pruning / trace-lookback — revisit §4), not a divergence.
- Scope caveat: this method validates **L1 (execution results)**. The flatKV **read path** at historical heights and the **committed root** are out of scope here — the read path needs an SS-off + latest-height setup (§1), and the root is the separate **seidb logical-digest** track.

---

## 9. Generating the Notion report

An AI agent reads the aggregated results and writes the report via its Notion MCP (there is no report-writer in seictl). The report should carry, per shard: the pair (images, snapshot height, `write_mode`s), the migration-complete gate result, blocks compared, the **L1 match rate (the flatKV verdict)**, and any determinate L1 divergences clustered by attribution (contract/kind) with the raw dual-node records for each. **Report L1 as the flatKV result; report L2 explicitly as a same-history sanity check, not flatKV parity** (§1/§8) — otherwise a green L2 gets misread as proof. State that the committed-root / flatKV historical read path are out of scope (the seidb-digest track and an SS-off+latest-height run, respectively), so a green run is never read as a complete migration proof.

---

## 10. Fan-out to 50+ shards

- **Sampling:** ~10% of the block space between EVM genesis and tip = a stride over the available 100k-spaced snapshots. Anchor one shard at EVM genesis (free flatKV coverage); the rest need the §2 high-height treatment.
- **Waves:** the cap is archive p2p peer budget + blocksync bandwidth (both generous), and per-node disk (~2×168G per pair, archival). Run in waves sized to the peer budget; one full-history archive serves all.
- **Per-shard identity:** each pair writes a distinct `shadow-results/…/<shard>/` prefix; a re-run gets a fresh `<shard>` (never reuse — reused prefixes overwrite pages).
- **Teardown:** deleting a replayer `SeiNode` **automatically deletes its controller-created data PVC** (the controller's finalizer deletes it on SeiNode deletion) — no separate PVC-delete step for controller-managed volumes. (Only `spec.dataVolume.import` PVCs are retained — those you clean up yourself; the replay nodes here don't use import.) Re-pointing a shard to a new snapshot: delete and recreate the `SeiNode` (its data PVC is deleted with it, forcing a fresh bootstrap) — config renders init-only, so an in-place edit won't re-bootstrap.

---

## 11. Failure modes (quick reference)

| Symptom | Cause | Fix |
|---|---|---|
| Replay stuck, `"no progress since last advance"`, blockstore height 0 | peers prune below the shard height; no full-history block source | add the archive as a static peer (§5) |
| Comparator L2 all `indeterminate` (`beyond max lookback` / pruned) | tip-following pruning + trace-lookback cap | archival overrides + `evm.max_trace_lookback_blocks: -1` (§4) |
| Comparator shows `code 2 tx parse error` where the archive succeeded | strict decoder rejecting pre-v6.5 non-canonical bodies | use the `historical_replay` build (§3) |
| flatKV vs archive "divergence" (OOG both directions, auth failures) | comparing re-execution vs stored results — version drift, not flatKV | compare the two **replay nodes**, never the archive (§1) |
| L2 parity always green | **historical EVM reads served from SS, not flatKV** — L2 is SS↔SS by construction, vacuous for flatKV | treat L2 as a same-history sanity check; take the flatKV verdict from L1; for the flatKV read path use SS-off + latest-height (§1/§8) |
| L1 parity looks perfect but suspiciously so | migration not complete → execution read un-migrated EVM keys from memIAVL on the flatKV node | verify `sei_chain_seidb_migration_version == target` (§2) |
| `evm_migrated` node: every EVM read not-found, no error | pointed at a memIAVL-history store; no runtime guard | only use `evm_migrated` on a migration-complete state (§2) |
| `seictl serve` standalone pod errors on missing `SEI_GENESIS_BUCKET` etc. | serve validates full platform env | use the controller-wired in-pod sidecar instead of a standalone serve |
| canonical-RPC errors mid-run on missing heights | comparator running on the *ahead* node | run it on the **behind** node, canonical = ahead (§7) |

**Observability floor:** `sei_chain_seidb_migration_version` + `_keys_migrated_total` (flatKV read gate), `/status` heights (both nodes, unwrapped JSON), `sei_chain_memiavl_*` vs `sei_chain_...flatkv` metric scopes (confirm each node's backend), the comparator task status via `GET /v0/tasks/<id>`.
