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

`faults.ExtendedCatalog` holds faults that are **not** in the nightly suite:
ones that need an assertion the f=1 `Catalog` contract cannot give (a chain that
stops and resumes), or that probe recovery harder than one kill. It is separate
on purpose: the nightly suite and `seictl` iterate `Catalog` and assume every
entry leaves the chain live, so a `QuorumLoss` fault there would turn the
nightly red by design. `faults.Lookup` does not find extended faults.

They run through `TestChaosExtended` (`test/integration/chaos_extended_test.go`),
which is not named `TestNightly*`, so the nightly's `^TestNightly` pattern never
selects it. Run it by name, as a manual Job (see `clusters/harbor/nightly/README.md`
in the platform repo, "Running a suite manually"), overriding `-test.run` to
`^TestChaosExtended$`. It reads `SEID_IMAGE_CHAOS`, like the nightly chaos suite.

| Scenario | Fault(s) | What it checks |
|---|---|---|
| `kill-during-recovery` | `container-kill-validator-0` x3 | validator-0 SIGKILLed again as soon as its container runs; recovery from a store a crash left half-done |
| `cold-start` | `container-kill-all` | every validator SIGKILLed at once; each recovers alone and the chain must reconcile |
| `quorum-loss-partition` | `network-partition-split` | a 2\|2 split: the chain must stop committing, then resume without forking |
| `network-flap` | `network-partition` x5, 20s on/off | repeated drop and reconnect of one validator |
| `clean-stop` | `pod-clean-stop` | graceful stop (SIGTERM); records seid's exit code |
| `time-skew-forward` | `time-skew-forward` | one validator's clock +1m |
| `time-skew-large` | `time-skew-large` | one validator's clock -5m |

Every scenario ends with one recovery gate, stronger than the nightly's:
validators Ready after a **real restart** (a restart count that rose, or a
replaced pod, so a pod that has not yet noticed its own kill cannot pass), the
follower caught up and advancing, and all validators and the follower agreeing
on the **app hash** at a common height (`test/chaoscheck`). A node that came
back with a corrupted store stays Ready and keeps up but reports a different
hash.

Optional inputs: `CHAOS_EXTENDED_SCENARIOS` (comma-separated names),
`CHAOS_STORAGE_PROFILE` (`memiavl`, the default, or `flatkv`, which runs the
chain on `write_mode: flatkv_only` so the faults hit the engine the nightly never
touches), `CHAOS_REQUIRE_CLEAN_SHUTDOWN=true` (fail `clean-stop` if seid was
SIGKILLed instead of exiting on SIGTERM).

None of these scenarios has run in a cluster yet. Promote one into `Catalog`,
and so into the nightly, only after it has been stable there.
