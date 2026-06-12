# Known Issues: Node Startup Alarms

Recurring alerts observed during SeiNode and SeiNodeDeployment deployments. These are expected during the bootstrap lifecycle but represent areas for improvement.

## 1. SeiNodeFailed: Shadow Replayer on Dev

**Alert:** `SeiNodeFailed` for `pacific-1-shadow-replayer`
**Environment:** dev
**Severity:** critical (alert), expected during iteration

**What happens:** The shadow replayer fails during bootstrap, typically at `discover-peers` or `configure-state-sync`. Each failed deployment requires deleting and recreating the SeiNode. _(Historical: `discover-peers` was a sidecar bootstrap task when this incident occurred; peering is now controller-owned via the config-apply `persistent_peers` override and is no longer a distinct task.)_

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

## General Notes

- **Failed nodes are terminal:** Once a SeiNode enters `PhaseFailed`, no further reconciliation occurs. The operator must `kubectl delete seinode <name>` and let the group controller recreate it (or recreate manually for standalone nodes).
- **PVCs persist across node recreation:** Deleting a SeiNode does not delete its PVC (by default). A new node with the same name reuses the existing PVC, which may contain stale sidecar SQLite state. The sidecar's `rehydrateStaleTasks` handles stale `running` tasks, and the cloud-API `Submit` model handles stale `failed` tasks.
- **Controller image matters:** The `/bin/sh` fix (PR #53), predicate fix (PR #58), and plan ID changes (PR #50) all require the controller image to be updated. Check `config/manager/manager.yaml` matches the latest ECR build.
