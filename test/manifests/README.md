# manifests

Shared, importable manifest renderers for the benchmark/chaos harness. The
integration suite (`test/integration`) and external tools (`seictl chaos
render`, `seictl bench render`) both render from these packages so that the
manifests an engineer commits to a GitOps workspace are byte-for-byte what the
nightly suite runs.

- `faults` — the Chaos-Mesh fault catalog. `faults.Catalog` lists the active
  scenarios (kind, one-shot vs duration-bearing); `Fault.Render(Params)` emits
  the CR. Selector contract: `sei.io/nodedeployment=<chainID>` picks the
  network's pods, `sei.io/node=<chainID>-0` picks validator-0 where a fault
  needs a single victim. Every resource is named `<fault>-<runID>` (or a
  fault-specific prefix ending in `-<runID>`) and labelled
  `sei.io/harness-run=<runID>`.
- `bench` — the `seiload` benchmark Job. `bench.Render(Params)` emits the Job
  that mounts a profile ConfigMap and runs seiload for `DurationMinutes`
  with a deadline of `DurationMinutes + DeadlineSlackMinutes`.

Deferred scenarios (`dns-chaos`, `disk-io-latency`) are documented in
`test/integration/chaos_deferred_test.go`, not in the catalog.

## Extended chaos scenarios

**None of these scenarios has run in a cluster yet.** Each is reasoned from the
controller and `sei-tendermint` source, and the first harbor run has to confirm
the open items listed under "Not yet confirmed" below before any of it is trusted.

`faults.ExtendedCatalog` holds faults that are **not** in the nightly suite:
ones that need an assertion the f=1 `Catalog` contract cannot give (a chain that
stops and resumes), or that probe recovery harder than one kill. It is separate
on purpose: the nightly suite and `seictl` iterate `Catalog` and assume every
entry leaves the chain live, so a `QuorumLoss` fault there would turn the
nightly red by design (`TestCatalog_HoldsNoQuorumLossFault` guards this).
`faults.Lookup` does not find extended faults.

They run through `TestChaosExtended` (`test/integration/chaos_extended_test.go`),
which is not named `TestNightly*`, so the nightly's `^TestNightly` pattern never
selects it. Run it by name, as a manual Job (see `clusters/harbor/nightly/README.md`
in the platform repo, "Running a suite manually"), overriding `-test.run` to
`^TestChaosExtended$` and running with **`-test.timeout 0`**: each scenario
budgets 40 minutes, and the default 10-minute test timeout would panic mid-fault.

Required environment: `SEI_CHAIN_ID` (the base chain id) and `SEID_IMAGE_CHAOS`
(the seid image, as for the nightly chaos suite). Optional: `SEI_NAMESPACE`,
`CHAOS_DURATION`, `CHAOS_EXTENDED_SCENARIOS`, `CHAOS_STORAGE_PROFILE`,
`CHAOS_REQUIRE_CLEAN_SHUTDOWN`, `CHAOS_SYNC_DEPTH`, and `CHAOS_LOAD_MINUTES` (which
then requires `SEILOAD_IMAGE`), described below.

| Scenario | Fault(s) | What it checks |
|---|---|---|
| `kill-during-recovery` | `container-kill-validator-0` x3 | validator-0 SIGKILLed again as soon as its container runs, so recovery starts from a store a crash left half-done. **Best effort:** a fast recovery can finish before the next kill lands; the log records whether the pod was already Ready at each kill, so a green run is not proof that mid-recovery kills were covered. |
| `restart-during-blocksync` | `container-kill-node` | a follower added to a chain already `CHAOS_SYNC_DEPTH` blocks deep is SIGKILLed while it reports `catching_up`. The kill is injected a couple of seconds after the follower is observed syncing, so afterwards the scenario compares the follower's first height after the restart with the chain tip at the kill: at or above the tip means it had already finished and the scenario fails (raise the depth). It must also not resume below the height it had when observed (less a block or two), catch up, and agree with every other node. |
| `restart-during-statesync` | `container-kill-node` | a follower bootstrapping from a snapshot is SIGKILLed while it state syncs. The phase is detected from seid's **log** (`starting state sync` without `Finished state sync`), because the RPC listener binds after state sync and `/status` is refused for the whole phase; this needs seid logging at info level. Afterwards the killed container's own previous log must show it started state sync and never finished, which is the proof the kill landed in the phase. It must then start over (a snapshot restore is not resumable), still end up from a snapshot (earliest height above 1), and agree with every other node. |
| `cold-start` | `container-kill-all` | every validator SIGKILLed at once; each recovers alone and the chain must reconcile |
| `quorum-loss-partition` | `network-partition-split` | a 2\|2 split of the validators, RPC follower on one side: the chain must stop committing (observed on validator-0), then resume without forking |
| `network-flap` | `network-partition` (from the nightly `Catalog`, not `ExtendedCatalog`) x5, 20s on/off | repeated drop and reconnect of one validator |
| `clean-stop` | `pod-clean-stop` | graceful stop (SIGTERM, 30s grace period: the controller sets none, so Kubernetes' default applies); records seid's exit code |
| `time-skew-forward` | `time-skew-forward` | one validator's clock +1m |
| `time-skew-large` | `time-skew-large` | one validator's clock -5m. With one of four validators skewed, exactly three honest validators remain, which is quorum with no margin, so expect occasional red runs from unrelated transient loss. |

Every scenario ends with one recovery gate, stronger than the nightly's:
- every validator Ready after a **real restart** (a restart count that rose, or
  a replaced pod), so a pod that has not yet noticed its own kill cannot pass;
- the RPC follower caught up and advancing;
- every validator and the follower reporting the **same committed state hash at
  several heights above the height the chain had reached just before the fault**
  (`test/chaoscheck`). The hash is each node's own, read from `/abci_info`, so
  it catches a node that recovered into different state, and a fork. It does
  not catch a node that recovered into corrupt state and then stalled: that
  node rejects the next block, and the Ready and caught-up checks catch it.

The suite assumes CometBFT consensus, which is what it provisions: the
follower's caught-up check reads `/status` `catching_up`, which is not
meaningful under Autobahn (it stays `true` with no consensus reactor).

Optional inputs:
- `CHAOS_LOAD_MINUTES`: when above 0, every scenario runs under seiload for up to
  that long. It needs `SEILOAD_IMAGE`, and takes `SEILOAD_PROFILE` (default
  `nightly_evm_transfer`) and `SEILOAD_COMMIT_ID` as the benchmark suite does. The
  load starts before the scenario captures its pre-fault height; transactions must
  be landing in blocks **before** the fault (else the load is not real and the run
  fails), the load runs through the fault and the recovery gate, and transactions
  must land **again** after recovery. seiload's own thresholds (run length, revert
  ratio) are not applied, since a fault makes sends fail by design. The chain gets
  two followers, one taking the sends and the other, the observer, serving receipts.
  The chaos image is the `mock_balances` build, so no funded key is needed.
  **Set it above the scenario's own length, 30 minutes or more:** the Job runs this
  long and nothing extends it. A Job that has ended makes the post-recovery check
  fail, with a message that says the load ended, not that the chain stopped taking
  traffic. Scenario budgets are 40 minutes, 60 for state sync and 75 for block sync.
- `CHAOS_SYNC_DEPTH`: how many blocks the chain has before `restart-during-blocksync`
  adds its follower (default 600). The deeper, the wider the block-sync window. The
  scenario fails, and says to raise this, if the follower finished syncing before
  the kill landed.
- `CHAOS_EXTENDED_SCENARIOS`: comma-separated scenario names.
- `CHAOS_STORAGE_PROFILE`: `memiavl` (the default), or `flatkv`, which runs the
  chain on `write_mode: flatkv_only`. **Unconfirmed:** the chaos image is built
  for memiavl, so the first `flatkv` run must show that it boots at all.
- `CHAOS_REQUIRE_CLEAN_SHUTDOWN=true`: fail `clean-stop` if seid was SIGKILLed
  instead of exiting on SIGTERM. **Expected to fail today:** seid is not known
  to flush storage on SIGTERM, so leave it off until that lands.

### Not yet confirmed (first harbor run)

- `pod-kill` with a grace period against the controller's `OnDelete`
  StatefulSet: that the pod is recreated under the same name with a new UID.
- That each validator's per-node RPC endpoint (`GetNode(<chain>-<i>)`) serves
  `/abci_info` in the shape `chaoscheck` parses.
- That the 2|2 split with the follower on one side really stops the chain.
- That Chaos Mesh keeps the per-target `injectedCount` on a fault CR after it
  recovers (`network-flap` relies on it).
- That the chaos image boots with `flatkv_only`.
- That `/status` reports `catching_up` true with a rising height while a node block
  syncs (`restart-during-blocksync` gates on it).
- That the chaos chain's seid logs `starting state sync` at the level it runs at
  (`restart-during-statesync` gates on the log; if the line never appears the
  scenario fails and says so), and that kubelet keeps the killed container's log so
  the scenario can read it.
- That a follower pod carries `sei.io/node=<its name>`, so `container-kill-node`
  reaches it, and that a snapshot-restored follower reports an earliest height
  above 1.
- That the seiload Job's profile (`SEILOAD_PROFILE`, default `nightly_evm_transfer`)
  lands transactions on the `mock_balances` chaos image without a funded key.
- That a kill can actually be landed in each sync phase on a chain of this size; the
  two sync scenarios check afterwards that it did and fail when it did not, rather
  than pass.

### Not covered

There is no WAL-replay scenario. A node replays at most one block on start, in
milliseconds, and the RPC listener binds after every other service, so "container
running, RPC refused" spans the whole of startup and cannot isolate replay. Replay
is exercised only indirectly, by `kill-during-recovery` under `CHAOS_LOAD_MINUTES`,
which kills a node mid-block and again as it restarts.

### Promotion

A scenario moves from `ExtendedCatalog` into `Catalog`, and so into the nightly,
only once it is stable in a cluster. The bar is not decided yet: how many
consecutive green runs, and who signs off, are open questions for the team.
