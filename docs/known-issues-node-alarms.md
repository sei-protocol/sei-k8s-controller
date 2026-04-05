# Known Issues: Node Startup Alarms

Recurring alerts observed during SeiNode and SeiNodeGroup deployments. These are expected during the bootstrap lifecycle but represent areas for improvement.

## 1. SeiNodeFailed: Fork Genesis Validators Exhaust Retries

**Alert:** `SeiNodeFailed` for `fork-test-0`, `fork-test-1`, `fork-test-2`
**Environment:** prod
**Severity:** critical (alert), expected (behavior)

**What happens:** Fork genesis validator nodes start simultaneously with the exporter. Their `configure-genesis` task retries up to 180 times waiting for `genesis.json` to appear in S3. The exporter takes 20-30+ minutes to bootstrap (snapshot download + restore + state sync + replay to export height), but the validators exhaust retries before the exporter completes.

**Root causes:**
1. **Retry backoff bypass (fixed in PR #58):** Status patches triggered watch events that immediately requeued reconciles, collapsing ~90 minutes of intended backoff into ~6 minutes. Fix: `GenerationChangedPredicate` filters self-inflicted status updates. Requires controller image `d8d337b` or later.
2. **No coordination between group plan and node plans:** Validators start their init plans immediately without waiting for the group plan's `assemble-genesis-fork` task to complete. The `configure-genesis` retry loop is a polling mechanism, not an event-driven wait.

**Future improvements:**
- Consider gating validator plan execution on a group-level condition (e.g., `GenesisCeremonyAssembled`)
- Or increase `genesisConfigureMaxRetries` to account for long exporter bootstrap times
- Or add `sei.io/retry-plan` annotation support so operators can retry failed validators without deleting them

---

## 2. SeiNodeFailed: Shadow Replayer on Dev

**Alert:** `SeiNodeFailed` for `pacific-1-shadow-replayer`
**Environment:** dev
**Severity:** critical (alert), expected during iteration

**What happens:** The shadow replayer fails during bootstrap, typically at `discover-peers` or `configure-state-sync`. Each failed deployment requires deleting and recreating the SeiNode.

**Root causes encountered:**
1. **Pruned peers (resolved):** State-syncer EC2 nodes pruned blocks below the snapshot height (200440000). `configure-state-sync` queries peers for a block hash at the trust height and gets empty responses. Fix: use a snapshot at a height within peers' retention window.
2. **Proposer priority divergence (intermittent):** One snapshotter node (`3.75.235.199`) returns different validator set proposer priorities than the other 5. When CometBFT picks divergent nodes as primary vs witness, state sync fails with "proposer priority hashes do not match."
3. **Seid flat JSON format (fixed in PR #63):** `rpc.Client.Get()` assumed JSON-RPC envelopes but seid returns flat JSON. Peer discovery silently failed, reporting "no reachable peers."

**Current mitigation:**
- Use snapshot at height 200940000 (within state-syncer retention)
- Use `Component: state-syncer` peers (consistent proposer priorities at recent heights)

**Future improvements:**
- Use same host for both primary and witness when `useLocalSnapshot: true` (state sync only needs trust hash verification, not independent witnesses)
- Add peer fallback: try multiple peers for block hash queries instead of failing on first error
- Automate snapshot pipeline to keep recent snapshots available

---

## 3. TaskFailureRateHigh: configure-genesis

**Alert:** `TaskFailureRateHigh` for `configure-genesis`
**Environment:** prod
**Severity:** warning

**What happens:** During fork genesis ceremonies, the `configure-genesis` task on validator nodes fails repeatedly because `genesis.json` hasn't been assembled yet. The task retries with backoff (maxRetries=180) until the group controller completes the assembly step.

**This is expected behavior** — the retry loop is the coordination mechanism between the group plan (which assembles genesis) and the node plans (which consume it). The alert fires because >3 terminal failures occur within 15 minutes.

**Future improvements:**
- Tune alert threshold for fork genesis ceremonies (suppress during known group plan execution)
- Or add a `GenesisCeremonyInProgress` label to the metric so the alert can exclude expected retries

---

## General Notes

- **Failed nodes are terminal:** Once a SeiNode enters `PhaseFailed`, no further reconciliation occurs. The operator must `kubectl delete seinode <name>` and let the group controller recreate it (or recreate manually for standalone nodes).
- **PVCs persist across node recreation:** Deleting a SeiNode does not delete its PVC (by default). A new node with the same name reuses the existing PVC, which may contain stale sidecar SQLite state. The sidecar's `rehydrateStaleTasks` handles stale `running` tasks, and the cloud-API `Submit` model handles stale `failed` tasks.
- **Controller image matters:** The `/bin/sh` fix (PR #53), predicate fix (PR #58), and plan ID changes (PR #50) all require the controller image to be updated. Check `config/manager/manager.yaml` matches the latest ECR build.
