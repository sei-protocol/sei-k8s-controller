# harness

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
