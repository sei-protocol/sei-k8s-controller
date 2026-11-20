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
`CHAOS_DURATION`, `CHAOS_EXTENDED_SCENARIOS`, `CHAOS_STORAGE_PROFILE` and
`CHAOS_REQUIRE_CLEAN_SHUTDOWN`, described below.

| Scenario | Fault(s) | What it checks |
|---|---|---|
| `kill-during-recovery` | `container-kill-validator-0` x3 | validator-0 SIGKILLed again as soon as its container runs, so recovery starts from a store a crash left half-done. **Best effort:** a fast recovery can finish before the next kill lands; the log records whether the pod was already Ready at each kill, so a green run is not proof that mid-recovery kills were covered. |
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

### Promotion

A scenario moves from `ExtendedCatalog` into `Catalog`, and so into the nightly,
only once it is stable in a cluster. The bar is not decided yet: how many
consecutive green runs, and who signs off, are open questions for the team.
