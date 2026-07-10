# Benchmarking EVM Gas vs. Real Execution Time on Sei

**Audience:** engineers (and their agents) on the gas-repricing project investigating whether Sei's EVM gas schedule tracks real compute cost. Covers both target functionalities: (1) replaying a real historical block against restored state and measuring gas + execution time per tx, and (2) executing arbitrary bytecode against a specific historical state and measuring the same.
**Scope:** standing up a replayer node, choosing the right binary version deliberately (§2), and using `debug_traceTransactionProfile` — a validated, existing RPC method — to get clean per-tx gas + execution-time pairs with a single call, no kernel instrumentation required for either functionality as specified. Also covers a related, separate question that comes up alongside this work: reading whole-block execution time with no new RPC or instrumentation (§8). eBPF (§9) is a fallback for needs beyond what's documented here (e.g. true per-opcode dispatch timing), not the default path. A third avenue, OpenTelemetry tracing, was built and tested against real data and found not fit for this purpose — see Appendix A before re-proposing it.
**Not in scope:** *why* Sei's gas schedule has the specific values it does (a governance/design question — capture a resulting change proposal as a `/design` doc, not here). Running any of this against a shared/long-lived/production chain, or against the canonical shared archive node in a way that mutates its state.

> **Read this whole section before running anything.** Six things make the naive version of this experiment **silently wrong**: (1) at least one opcode's gas cost (`SSTORE`) is a **governance-mutable Sei chain param that already diverges from upstream go-ethereum** — using it as your "does gas match compute" baseline tests a value that was hand-tuned away from compute cost *by design*; (2) **a replayer's binary version must match what actually produced the history you're replaying, or every block after the first will fail app-hash validation** — confirmed empirically this session (§2); (3) **`debug_traceTransactionProfile` and the rest of the `debug_trace*` family are capped to `MaxTraceLookbackBlocks` (default 10,000) blocks behind the node's current tip** — a fast-catching-up replayer ages a target height out of that window in minutes; trace it soon after it's produced (§4); (4) **only `Y=X-1` is supported for Functionality 1 by any tooling that exists today** — there is no code path for pinning a real historical block's transactions to an arbitrarily older base state (§4); (5) **Functionality 2's execution-time output has a real, unclosed gap** — `debug_traceCall` returns gas but no timing field for arbitrary bytecode against arbitrary `Y` (§5); (6) **attaching any eBPF probe to a shared harbor pod requires a privileged kernel capability grant** — nothing technically blocks it today, but that's an unhardened gap, not a green light (§9).

---

## 0. Substitutions used throughout

| Placeholder | Meaning |
|---|---|
| `<alias>` | your harbor engineer alias; namespace is `eng-<alias>`. |
| `<Y>` | the historical height whose post-block state you're restoring/executing against. |
| `<X>` | the historical block (Functionality 1 only) whose transactions you're re-executing and timing; in practice `X = Y+1` — see §4. |
| `<replay-node>` | the `SeiNode` in `replayer` mode restoring to `<Y>` and replaying forward (§3). |
| `<archive-node>` | the archive-mode `SeiNode` or existing shared archive infra used for deep historical `<Y>` reach — **read-only queries only** (§5's Path A). Never a target for §6's `seid tx evm` submission commands, which mutate state — those go only to your own private node. |
| `<image-tag>` | the sei-chain image the replayer runs — chosen deliberately per §2, never left at whatever default a template suggests. |

---

## 1. Mental model — what this can and cannot prove (read first)

The question: for a given opcode/tx, does the gas charged scale with the real wall-clock cost of executing it? The honest answer requires knowing which parts of the gas schedule were **derived from measured compute** vs. **set by other means** (governance, upstream inheritance, a flat precompile fee).

**What's a fair target vs. a confound:**

| Category | Examples | Why |
|---|---|---|
| **Primary target — interpreter-local, no state backend** | `KECCAK256`, pure arithmetic (`ADD`/`MUL`/`PUSH`/`DUP`/…) | Executes entirely in the interpreter loop with no I/O — the cleanest compute-bound signal. |
| **Secondary target — real, but control for it** | `SLOAD`/`SSTORE` | **`SSTORE`'s set-gas is a governance-mutable Sei chain param (`SeiSstoreSetGasEIP2200`, `x/evm/types/params.go:43`, code default `20000`, governance-set higher on mainnet — query it live, never hard-code either value)** — a divergence here confirms a known, intentional gap, not a finding. If you test it anyway, separate cold-slot and warm-slot runs; Sei's state backend is memIAVL/flatKV, not an MPT, and cache-warmth dominates. |
| **Separate, expected-to-diverge category — never blend into "opcode gas"** | Sei precompiles (staking, distribution, oracle, etc. — see `/evm` skill's `kit-sei-precompiles.md`) | Precompile execution is a Go-native Cosmos-module call outside the interpreter entirely; gas is a governance-tunable, structurally decoupled fee. |

**Confounds that apply regardless of functionality:**

- **Per-tx fixed overhead** (signature verification, the Cosmos ante handler, the EVM↔Cosmos StateDB bridge) is real wall-clock time attributable to *no* opcode — it dilutes a measurement unless your sample size / loop count makes it a small fraction of the total.
- **OCC (optimistic concurrent execution) does not break per-tx correlation for §4's RPC primitive.** Sei applies a block's transactions through a parallel scheduler (`app/abci.go`, `sei-cosmos/tasks/scheduler.go`), not a serial loop. A raw uprobe/uretprobe bracket around `StateTransition.Execute` (§9) *would* need OCC disabled to get clean per-tx numbers, since multiple `Execute` calls can run concurrently on different threads. `debug_traceTransactionProfile` sidesteps this entirely: it isolates a single tx's execution by replaying the block *up to* that tx's index and then executing just it (`evmrpc/simulate.go`'s `replayTransactionTillIndex`), independent of how the block was originally scheduled. You do **not** need to disable OCC on the replayer to get clean per-tx numbers via this RPC.
- **Whole-block wall-clock time (log-line `latency_ms` on `"finalized block"`) is the wrong signal — verified empirically, not theoretically.** It's diluted by non-EVM per-block work (oracle vote tally, IBC, begin/end-blockers for every module) and, on a replayer still catching up, contention with block-fetching. A same-session sample: correlating gas against whole-block latency gave Pearson r=0.301; filtering to EVM-active blocks and using the OCC-scheduler's tx-execution-phase latency improved it to r=0.618, still with real outliers. Correlating gas against `debug_traceTransactionProfile`'s `executionNanos` on the same chain gave r=0.9914 on an initial 9-tx sample — **but that invocation used the default StructLogger tracer, which adds opcode-scaled overhead into the timed window; switching to the lighter `callTracer` on a properly diverse sample gave r=0.8093** (§4 step 9 has the full story, including a sampling mistake worth learning from). Use the RPC with `callTracer`, not a log-derived proxy and not the default invocation.

---

## 2. Version pinning — the replayer must match what actually produced the history, or every block after the first fails

**This is the load-bearing gotcha of the whole runbook, and it is not theoretical — it happened this session.** A replayer restores state via an S3 snapshot restore (§4 — not Tendermint's p2p state-sync variant; the `SeiNode` CRD supports both, but a `replayer` requires the `s3` source) and then blocksyncs forward, re-executing each real historical block against its own binary. If that binary's state-transition logic differs *at all* from what actually ran on the live chain — even one commit ahead of the released version — the resulting `AppHash` after the first replayed block will not match the real chain's recorded `AppHash` for the next block, and the node halts with `validateBlock(): wrong Block.Header.AppHash: expected X, got Y: app hash mismatch`, evicting every peer it asks for that block (they all report the same real historical data; the mismatch is your binary, not them).

**Confirm the live chain's actual version before choosing an image:**

```sh
kubectl get seinode <a-live-syncer-or-validator> -n <its-namespace> -o jsonpath='{.spec.image}{"\n"}'
```

Two deliberate choices, pick one per experiment — **do not default to "whatever image tag is lying around"**:

### 2a. Pin to the released version matching the live chain — for a clean, zero-divergence baseline

Use the exact tag the live chain runs (e.g. `ghcr.io/sei-protocol/sei:v6.5.2`). This gives you faithful replay of real history with **zero** validation errors — the right choice for "does the *currently released* gas schedule track *currently released* compute cost," which is almost certainly the baseline question the gas-repricing project needs before proposing any change.

Verified this session: switching from an arbitrary dev-branch commit image to the release tag matching the live chain's actual version took the replayer from a permanent app-hash-mismatch eviction loop at the very first post-snapshot block to zero errors across tens of thousands of blocks.

**"The release tag" and "the exact commit someone else already validated" are not always the same thing — check, don't assume by release family.** Verified this session: a `release/v6.6` backport commit and the `v6.5.2` tag had genuinely diverged (380 commits ahead, 63 behind each other) despite both being "the v6.5-ish safe version" in casual conversation. Whether `debug_traceTransactionProfile` (§4) exists on a given image is commit-specific, not release-family-specific — no released *tag* contains it as of this writing, but a backport/hotfix branch someone else is already running might already have it (confirmed: one such commit had it, more completely than the version on `origin/main`, with an added lookback guard the original commit lacked). Before assuming you need `mock_chain_validation` (§2b) or a custom cherry-pick to get the profiling endpoint, check the *exact* image already in use for the exact height range you care about — `debug_traceTransactionProfile` against it directly is the fastest way to find out, faster than reasoning about it from tags.

### 2b. `mock_chain_validation-<sha>` — for testing bleeding-edge/proposed code against real transaction load

**Only ever the image of your own throwaway replayer in `eng-<alias>` — never merge this tag onto a shared, long-lived, or archive `SeiNode`.** Image selection for any `SeiNode` flows through the `/harbor-dev` skill's PR-based render into `harbor-engineering-workspace`, a repo shared across engineers; nothing stops you from pointing this tag at the wrong manifest, and a shared node running it would silently accept diverged state while still looking healthy. Confirm the target is your own disposable node before merging.

If you need to test code that **isn't released yet** (a proposed repricing change, a migration candidate) against real historical transaction patterns, sei-chain's CI builds a `mock_chain_validation` variant of essentially every commit. It's a Go build tag (`sei-tendermint/types/consensus_policy_mock_chain_validation.go`) that swallows the app-hash/results-hash/validator-set class of consensus errors (`ErrAppHash`, `ErrConsensusHash`, `ErrValidatorsHash`, `ErrUpgradeBeforeTrigger`, etc.) **while still fully verifying peer-delivered transaction data** (`ErrDataHash`/`ErrEvidenceHash` are never swallowed — a lying peer still can't inject fake transactions). It emits `sei_unsafe_validation_skipped_total` (a Prometheus counter labeled `validation_error`) every time it swallows one, so divergence is directly observable, not silent.

**Gas metering itself is untouched by this build tag** — the swallowed errors are all consensus-hash comparisons; `sei-cosmos/store/gaskv/store.go`'s `ConsumeGas` calls (the actual gas accounting) are identical to the default build. The tag only swaps which validation failures halt vs. continue, not how gas is charged — relevant reassurance given this whole runbook is a gas benchmark.

**Find the image:**

```sh
aws ecr describe-images --repository-name sei/sei-chain --region us-east-2 | \
  python3 -c "
import json,sys
d=json.load(sys.stdin)
imgs=[i for i in d['imageDetails'] for t in i.get('imageTags',[]) if 'mock_chain_validation' in t]
imgs.sort(key=lambda i: i['imagePushedAt'], reverse=True)
for i in imgs[:10]: print(i['imagePushedAt'], i.get('imageTags'))
"
```

On-demand builds are tagged `mock_chain_validation-<git-sha>`; nightly builds are `mock_chain_validation-nightly-<date>-<sha7>`. Confirm the tag you pick actually contains the commit you care about (`git merge-base --is-ancestor <commit> <tag-sha>` in your `sei-chain` checkout) before using it.

**Read the divergence rate before trusting anything measured under this mode:**

```sh
kubectl port-forward -n eng-<alias> <replay-node>-0 <local-port>:26660
curl -s localhost:<local-port>/metrics | grep sei_chain_sei_unsafe_validation_skipped_total
```

Verified this session (illustrative, not a permanent number — re-check `origin/main`'s actual distance from the last release yourself): running `mock_chain_validation` built from `origin/main`, hundreds of commits ahead of the last release, against real pacific-1 history produced **thousands of `app_hash` skips within the first few hundred blocks** — i.e. essentially every block's resulting state diverges from real history once you're running meaningfully different code. **This is expected, not a bug, and it compounds**: state at height `Y+N` reflects `N` blocks of the *new* binary's execution repeatedly applied to itself, not what actually existed historically at that height. That's fine and arguably desirable if your question is "does this new code's gas track this new code's own compute cost, exercised by realistic transaction load" — it is the wrong tool if your question requires reproducing exact historical state at a specific `<Y>`. Pick §2a for the latter.

**`sei_unsafe_validation_skipped_total` tells you divergence is happening — expected, by design — but not whether it has corrupted your measurement.** Watch tx success/revert rate alongside it. Once state has compounded far enough from reality, replayed transactions can start executing against genuinely unrealistic state (wrong balances, wrong nonces, wrong contract state) — early reverts and short-circuited paths, not real compute. That would corrupt even the "does new code's gas track new code's own compute" question this mode is otherwise good for, and the skip counter alone won't tell you it happened (it's *supposed* to be large under this mode). If the revert rate climbs, treat later measurements as suspect regardless of what the skip counter says.

---

## 3. Hardware parity — already automatic, verify it rather than configure it

**You do not need to request specific hardware.** `sei-k8s-controller` picks the Karpenter NodePool per node mode automatically: `internal/platform/platform.go`'s `NodepoolForMode()` — archive-mode nodes get `c.NodepoolArchive`, every other mode (including `replayer`) gets `c.NodepoolName` — and wires the matching toleration + required node affinity onto the pod spec (`internal/task/bootstrap_resources.go`, mirrored in `internal/noderesource/noderesource.go`). Harbor's and prod's `sei-k8s-controller/controller-config.yaml` are designed to carry identical `nodepoolName`/`nodepoolArchive`/`tolerationKey` values, and both clusters' NodePool manifests are designed to carry the same `r6i`/`r7i` instance-family, Nitro-hypervisor, on-demand requirements — by design, not independently re-verified here each time. Net: a replayer or archive `SeiNode` in your `eng-<alias>` namespace *should* land on the same instance class as its prod counterpart, with zero engineer-side configuration — confirm it with the step below rather than trusting the design intent, since drift between the two configs is exactly the failure mode that step catches.

**Optionally**, for a throwaway single-tenant experiment node, set the `sei.io/dedicated-node: "true"` annotation (see `sei-k8s-controller`'s dedicated-node feature) — not required for correctness, but removes one source of noisy-neighbor variance from your timing numbers. **Set it in the initial render, not on an already-running replayer.** Verified against the reconcile logic: a Running replayer's plan only rebuilds the pod on image drift or a sidecar-reapproval need (`internal/planner/replay.go`'s `buildRunningPlan`); an annotation-only change matches neither, so nothing replaces the pod, and with the StatefulSet's `OnDelete` strategy nothing else will either. The SeiNode object updates but the pod keeps running on its original, possibly-shared node indefinitely — a false sense of isolation that directly undermines step 7's noise-reduction discipline below. If you need to add it after the fact, delete the pod yourself to force a rebuild (§2's image-swap steps already establish this pattern).

**If a dedicated pod never schedules, nothing on the SeiNode object tells you.** This is documented behavior, not an oversight to work around: "a pod that can never be scheduled (no available single-tenant node) stays Pending with no SeiNode condition reflecting it" (`internal/noderesource/noderesource.go`). Check the pod's own `PodScheduled` condition and events directly if `.spec.nodeName` (the verify step below) comes back empty.

**Verify hardware parity anyway before trusting a timing result:**

```sh
NODE=$(kubectl -n eng-<alias> get pod <replay-node>-0 -o jsonpath='{.spec.nodeName}')
kubectl get node "$NODE" -o jsonpath='{.metadata.labels.node\.kubernetes\.io/instance-type}{"\n"}{.metadata.labels.karpenter\.sh/nodepool}{"\n"}'
```

Expect an `r6i.*`/`r7i.*` instance type and `nodepool=sei-node` (or `sei-archive` for an archive-mode node). If you see anything else, halt and check for a stale `controller-config.yaml` divergence between harbor and prod before proceeding — don't benchmark on it.

---

## 4. Functionality 1 — replay block `<X>` against state after `<Y>`, measure gas + time

**Scope note, confirmed by reading the actual RPC/replayer code, not by inference: only `Y=X-1` is supported today.** Every path that replays a real historical block's transactions — this replayer, `debug_traceBlockByNumber`, `debug_traceTransactionProfile` — hardcodes the base state to exactly one block before the target (`evmrpc/simulate.go`'s `initializeBlock`: `prevBlockHeight := block.Number().Int64() - 1`). There is no hook anywhere for pinning execution to an arbitrarily older `Y`. If your actual need is the general case (e.g. isolating state-size effects by holding transactions fixed while varying the base height), that is new engineering, not a config change — flag it explicitly rather than silently reporting `Y=X-1` numbers as if they answered a different question.

### Steps

1. **Choose and pin the image deliberately (§2)** before rendering anything.
2. **Render a `replayer`-mode `SeiNode`** targeting `<Y>`: `spec.replayer.snapshot.s3.targetHeight: <Y>` **and `spec.peers`** — the CRD's admission rule rejects a replayer with no `peers` set (`api/v1alpha1/seinode_types.go`'s CEL rule: "peers is required when replayer mode is set"), so the render is incomplete without it. Point `peers` at a real historical block source (a shared full-history archive's p2p address, or a `label`/`selector` peer within your namespace) — the seictl sidecar restores the highest snapshot `<= <Y>` and syncs forward from there (verified directly against seictl's own source, not just the sei-k8s-controller CRD: `sidecar/tasks/snapshot_restore.go` — "TargetHeight, when set, selects the highest available snapshot <= that height. When zero, the latest snapshot (from latest.txt) is used," enforced in `resolveKeyForHeight`'s height-capped search). **The `SeiNode` CRD's own doc comment on `TargetHeight` (`api/v1alpha1/common_types.go`) reads differently and only describes the zero-value/"restore latest, then sync forward" case — don't trust it over seictl's actual selection logic if the two seem to disagree.** Render via the `/harbor-dev` skill's PR-based flow into `harbor-engineering-workspace`, same as any other `SeiNode`.
3. **Let it replay forward past `<X>`.** There is no documented flag to halt exactly at a target height. Poll `.status`/RPC height or tail logs to confirm `<X>` has been applied; letting the node continue replaying past it is harmless.
4. **A replayer's EVM-RPC is reachable but not advertised — port-forward directly, don't look for it in `.status`.** The controller treats `replayer` mode as an RPC-less restore workload (`internal/controller/node/endpoints.go`'s `servesEVM()` returns `false` for it), so `.status.endpoint` is absent for a replayer regardless of whether the underlying `seid` process actually serves EVM traffic. It does — this session's measurements ran against it — but you must reach it via `kubectl port-forward <replay-node>-0 8545:8545`, not by reading it off the SeiNode's status. If a future controller change acts on the "RPC-less" intent by actually disabling EVM on replayers, this whole runbook's measurement path breaks; verify 8545 responds (`eth_blockNumber`) before building anything on top of it.
5. **Get the tx hash(es) for `<X>`:** `eth_getBlockByNumber(<X>, true)` on the replayer's own EVM-RPC (port 8545) returns full transaction objects including `hash` and the block-level `gasUsed`.
6. **For each tx, call the RPC — but not with the default invocation.** No-config `debug_traceTransactionProfile` attaches a full opcode-level `StructLogger` (stack + storage capture per step) *inside* the `executionNanos` timing bracket (confirmed by reading `evmrpc/block_trace_profiled.go`'s `config==nil` path and the bracket around `core.ApplyTransactionWithEVM`). That overhead scales with opcode count — i.e. with gas — which artificially inflates any gas-vs-time correlation you measure. **Pass `{"tracer":"callTracer"}`** instead — confirmed empirically this session: the same tx measured 65.1ms with the default StructLogger vs. 34.7ms with `callTracer`, nearly 2x contamination. `callTracer` has no per-opcode hook at all (`OnTxStart`/`OnTxEnd`/`OnEnter`/`OnExit`/`OnLog` only, no `OnOpcode`) — its overhead scales with **call-frame and log count**, not opcode count, so for §1's primary target (interpreter-local arithmetic, `KECCAK256` — no nested calls, no logs) its residual is genuinely close to zero. Contamination concentrates on call-heavy, log-heavy, or state-heavy txs — the secondary/precompile categories §1 already treats separately. **Do not reach for `noopTracer` as an even-lighter baseline — it's actually heavier per-opcode than `callTracer`** (it does register `OnOpcode`); diffing against it would overstate `callTracer`'s contamination, not bound it. If you want to bound `callTracer`'s own nested-call-frame overhead specifically, diff it against `{"tracer":"callTracer","tracerConfig":{"onlyTopCall":true}}` (short-circuits nested `OnEnter`/`OnExit`) — same differencing technique that exposed the StructLogger gap, one level finer, no source change needed. The true zero-overhead floor (no tracer hooks at all, `executionNanos` still populated) needs a small sei-chain change — see §5 — but `callTracer` is the best available today without one.

   ```sh
   curl -s -X POST -H "Content-Type: application/json" \
     --data '{"jsonrpc":"2.0","method":"debug_traceTransactionProfile","params":["<tx-hash>", {"tracer":"callTracer"}],"id":1}' \
     localhost:8545
   ```

   The response's `result.trace.gas` (or `result.trace.gasUsed` depending on tracer) is the real gas used. `result.profile.phases.executionNanos` is the per-tx EVM execution time — **use this field, not `result.profile.totalNanos`**, which also includes `replayHistoricalTxsNanos` (replaying the block up to this tx's index — real cost, but not *this tx's* cost) and `traceResultNanos` (tracing-instrumentation overhead itself). **`executionNanos` itself still includes state-backend I/O time**, and for a SLOAD/SSTORE-heavy tx this means cache-warmth (memIAVL/flatKV page-cache state, itself run-to-run variable) is baked into what you're calling "execution time."

   **Do not subtract `result.profile.store`'s totals from `executionNanos` to isolate interpreter-only compute — that arithmetic is broken, caught in this session's own peer review before it shipped.** `executionNanos` brackets only the target tx (the window around `core.ApplyTransactionWithEVM`), but the store trace dump accumulates over the *entire* statedb lifetime — including every preceding tx's execution during `replayHistoricalTxsNanos`'s `ReplayTransactionTillIndex` pass, on the same tracer instance. For a tx at index k in its block, the store total includes k other transactions' state I/O, not just this one's; subtracting it from a single tx's `executionNanos` will silently go negative for anything past the first tx or two in a block. It's also incomplete even ignoring the windowing bug: the read-side lookup total (`historicalDbLookupNanos`) doesn't include `Set`/`Delete` (write) latency, which lives in a separate stats bucket — so it wouldn't capture SSTORE cost even if correctly scoped. Cleanly separating interpreter compute from state I/O per tx needs a sei-chain change to reset or snapshot the store tracer at the execution boundary — flag that as follow-on engineering rather than approximating it with this subtraction. Until then, report `executionNanos` as one number (compute + this tx's state I/O, not cleanly separable today) and treat `result.profile.store`'s counts as informative about which modules/keys were touched, not as a subtractable timing quantity.

7. **Take min-of-N, not a single sample, per tx.** Wall-clock timing (`time.Since`) absorbs GC pauses, goroutine preemption, and OS thread migration — noise that only ever *adds* time, never subtracts it. Across several repeated profile calls on the same tx, the **minimum** is the closest estimate of true compute cost, and the spread across repeats tells you the noise magnitude directly. Force a GC before each run if your tooling allows it (or note that you didn't). This costs nothing and needs no elevated privilege — do it before reaching for anything in §9. Run on a dedicated, quiesced node (§3) with no concurrent RPC traffic during timing; that removes more noise than any instrumentation would merely reveal.

8. **Do this promptly.** `debug_trace*` is capped to `MaxTraceLookbackBlocks` behind the node's current tip (default 10,000, `evmrpc/config/config.go`, configurable via `evm.max_trace_lookback_blocks`). A replayer still catching up can process 20-30 blocks/sec — 10,000 blocks ages out in under 10 minutes. If you collect a batch of tx hashes and then profile them later, some will fail with `"block number ... is beyond max lookback of 10000"`. Verified this session: exactly this happened mid-collection. Either profile each tx immediately after finding it, or wait until the node has fully caught up to live tip (stable height) before doing a bulk collection pass.

9. **Sample size, a real mistake to avoid, and what the correlation number can and can't tell you.** A single tx pair doesn't tell you much, and **neither does a sample with no variance in gas** — collecting 25 txs from a narrow, bot-dominated block window this session produced near-identical gas (458,696 for 24 of 25 txs) and a meaningless r=0.11, not because the correlation is weak but because a nearly-constant independent variable can't test it at all. Deliberately collect a real range of gas magnitudes/contracts before computing anything.

   **The following numbers are this session's illustrations of the primitive working, not a general finding about Sei's gas schedule — treat them as sizing the tool, not the research conclusion, and re-derive your own:** on a properly diverse sample (gas ranging 0–1.3M, n=25, single sample per tx — **not** the min-of-N discipline from step 7, so treat this as a lower bound on achievable precision), default-StructLogger gave Pearson r=0.9914 between gas and `executionNanos`; switching to `callTracer` on an equally diverse sample gave r=0.8093 — still a strong, real correlation, but honestly lower once tracer contamination is reduced.

   **Pearson r on a small, high-leverage sample is a smoke test that the tool produces signal, not a measure of pricing fidelity — don't let it become the headline finding.** A couple of extreme high-gas points and one `gas=0` point (an edge case worth checking directly — real EVM txs carry ≥21k intrinsic gas, so confirm what that tx actually is before including it) can dominate r on n=25; the question the gas-repricing project actually cares about is whether time-per-gas is *constant across opcode/contract mixes*, which is a residual-structure question, not an aggregate-association one. Report r² and the spread of individual (gas, time) points around a fit line, not just r; Spearman's rank correlation is more robust to the leverage/heteroscedasticity here than Pearson. Treat the differential-bytecode-loop (§6) as the real instrument for pricing-fidelity questions — the aggregate correlation in this step only establishes that the primitive measures something real.

   **A same-gas cluster (~130k-133k, n=8) showed a genuine ~2x spread in measured `callTracer` time (29ms-59ms) — do not dismiss this as noise addressable by step 7's min-of-N.** Min-of-N reduces measurement noise *across repeated calls of the same tx*; it says nothing about variance *across 8 different transactions* that happen to charge similar gas. Same gas does not mean same work — two txs charging ~130k can run entirely different opcode/state-access mixes, and a real spread here is arguably the exact phenomenon the gas-repricing project is hunting (equal gas, unequal real compute), not something to average away. **The correct check:** repeat each of the 8 txs individually with min-of-N. If the spread collapses within a single tx's repeats, it was measurement noise (most likely cache-warmth, since each target tx executes against post-replay state whose warmth depends on what its own block's preceding txs touched). If the spread persists across the 8 min-of-N values, that's a real, reportable gas-vs-compute divergence at fixed gas — treat it as a finding, not a fluke, and don't pre-judge it as noise before checking.

   Always re-check against §1's confounds (a divergence concentrated on `SSTORE`-heavy txs confirms the known governance gap, not a finding).

10. **Subtract intrinsic gas before correlating — a flat protocol charge is otherwise mixed into your independent variable.** `gasUsed` includes the intrinsic-gas floor (21000 base plus per-byte calldata costs, more for contract creation or an access list), deducted before the interpreter loop ever runs. It moves `gasUsed` without moving `executionNanos` at all, which is exactly why every minimum-gas transfer in a real sample clusters near zero on both axes regardless of what it actually touched. This is a client-side fix, not a sei-chain change: Sei calls the standard, unmodified go-ethereum `core.IntrinsicGas(data, accessList, authList, isContractCreation, true, true, true)` identically in three places (`app/app.go:2016`, `app/ante/evm_checktx.go:107`, `x/evm/ante/basic.go:51`), with the trailing fork-activation flags hardcoded `true`, not conditional on block height, so one formula covers all of Sei's history. Everything it needs — `input`, `to`, the access list — is already in `result.trace` or a plain `eth_getTransactionByHash`. Compute `executionGas = gasUsed - IntrinsicGas(...)` and correlate that against `executionNanos` instead of raw `gasUsed`.

11. **The bigger limitation no measurement technique here closes: `Y=X-1` blocks the question with the most repricing leverage for state opcodes.** For `SLOAD`/`SSTORE`, the decision-relevant variable is how access cost scales with state size / tree depth as the chain grows — which requires holding bytecode fixed while sweeping the base height, i.e. the general `Y<X` case this runbook's scope note (top of §4) already says no tooling supports. Interpreter-local opcodes (`KECCAK256`, arithmetic) you can measure well today with what's here; state-opcode cost-vs-state-size you structurally cannot. Name this explicitly rather than letting a `Y=X-1` measurement imply it answers the state-scaling question — it doesn't.

---

## 5. Functionality 2 — execute arbitrary bytecode against state after `<Y>`, measure gas + time

Two paths. Prefer Path B given the current state of the RPC surface — it fully reuses §4's validated primitive and closes the timing gap that Path A cannot.

### Path A — read-only `eth_call`/`debug_traceCall` (gas works, timing does not)

`eth_call` and `debug_traceCall` both take a fully independent `blockNrOrHash` parameter — no `X` concept at all, so arbitrary bytecode/calldata against an arbitrary historical `Y` works structurally for the *call* itself (`evmrpc/simulate.go`, `evmrpc/tracers.go`). But:

- Plain `eth_call` returns **only return-data** — no gas, no time. It is not subject to the lookback cap below, so it reaches genuinely arbitrary depth — at the cost of giving you neither number this runbook cares about.
- `debug_traceCall` with `{"tracer":"callTracer"}` returns **real gas** in the JSON result — but **no execution-time field exists anywhere in this path**, and **it is capped by the same `MaxTraceLookbackBlocks` as the rest of `debug_trace*`** (`evmrpc/tracers.go`'s `TraceCall` calls the same lookback guard). So Path A's gas-bearing option is bounded to recent history, not "arbitrary `<Y>`" — only the timing-blind, gas-blind plain `eth_call` reaches deep history. The `phaseDurations`/`ExecutionNanos` mechanism that powers §4's `debug_traceTransactionProfile` is simply not wired into `TraceCall` at all (confirmed by reading the call sites — one place populates `phaseDurations`, `TraceCall` isn't it).
- **This is a real, unclosed gap**, not a workaround-able limitation. Closing it is a small, well-scoped extension (thread the existing, already-working profiling mechanism into `TraceCall`) — but it's a sei-chain source change to a shared repo, out of scope for this runbook to make unilaterally. If Path A's read-only simulation (no real tx submitted) is a hard requirement, flag this gap to the team rather than fabricating a timing number.

### Path B (preferred) — submit a real signed tx, then reuse §4's primitive

Restore a `SeiNode` to height `<Y>` exactly as in §4 step 2, then instead of continuing passive historical replay, submit your own synthetic signed EVM transaction against it (deploy + call, §6's differential-bytecode technique). Once it's mined, it's an ordinary historical tx — **`debug_traceTransactionProfile` on its hash gives you gas + `executionNanos` in one call**, identical to §4, with none of Path A's timing gap.

**This mutates state — target only your own private restored node, never the canonical shared archive or any other engineer's `SeiNode`.** §6's `seid tx evm deploy`/`call-contract` commands submit real signed transactions; point `--evm-rpc` at your own node's address, confirmed before every run, not at shared archive infra. This costs standing up and mutating your own private node but fully closes both halves of Functionality 2's output contract with already-validated tooling.

### Historical `<Y>` reach — pruning limits, verified live

Your replayer/dedicated node prunes; the canonical shared archive doesn't. Checked directly this session: a replayer running Karpenter-default config had `pruning-keep-recent=86400`, `pruning-keep-every=500` (dense access to the last 86,400 blocks it has processed, sparse beyond that), while `p1-archive-canonical` ran `pruning=nothing` (full history to genesis). **For a genuinely old `<Y>`, point your tooling at the archive node's RPC, not a fresh replayer** — a replayer only has state back to wherever its own snapshot restore started forward from. Failure mode if you ask for a pruned height is loud, not silently wrong: `CacheMultiStoreWithVersion` returns `"failed to load state at height %d; %s (latest height: %d)"` — no risk of silently getting the wrong state.

---

## 6. Crafting opcode-isolated bytecode (Path B / any synthetic-tx run)

**The differential-loop technique** isolates a single opcode's marginal cost without per-opcode instrumentation — still necessary even with §4/§5's primitive, because `debug_traceTransactionProfile`'s timing is per-tx/per-phase, not per-opcode; its `structLogs` give per-opcode *gas*, not per-opcode *time*. Write two bytecode variants identical except for the opcode under test.

- **Variant A (opcode-under-test):** a loop body of `JUMPDEST`, the target `<opcode>` (with operands pre-pushed), then loop-decrement/`JUMPI` back to `JUMPDEST`.
- **Variant B (baseline):** identical scaffold, target opcode replaced with a cheap no-op pair (`PUSH1 0x00`/`POP`) of known cost.

`(executionNanos(A) − executionNanos(B)) ÷ iteration count ≈ per-opcode marginal wall-clock cost`; the same subtraction on `gas` gives the marginal gas cost to compare against it. This cancels loop-overhead opcodes and per-tx fixed overhead from both sides symmetrically.

**`--evm-rpc` below must point at your own private node — never the canonical shared archive or a node you don't own.** These commands submit real signed, state-mutating transactions.

```sh
seid tx evm deploy /path/to/<opcode>-loop.bin \
  --from=<key> --chain-id=<chain-id> --evm-rpc=<target-node-evm-rpc> \
  --gas-limit=<large-enough-for-iteration-count> --gas-fee-cap=<cap>

seid tx evm call-contract <deployed-addr> <calldata-hex> \
  --from=<key> --value=0 --chain-id=<chain-id> --evm-rpc=<target-node-evm-rpc>
```

Pick a large-but-fixed iteration count up front (large enough that fixed overhead is a small fraction of total gas/time; loop rather than unroll past a few hundred iterations — code-size cap). Run each variant multiple times and take the distribution, not a single sample — wall-clock has real variance from GC pauses, compaction, and scheduling noise that gas is blind to. Submit one tx at a time, wait for its receipt, before submitting the next — then call `debug_traceTransactionProfile` on each mined hash per §4/§5.

---

## 7. Reading results

For each opcode/tx under test: `gas` (free — already on the receipt, no extra instrumentation), `executionNanos` (§4/§5's primitive), and the live gas parameter value behind it if testing a governance-mutable opcode (§1). Report as a ratio (time-per-gas-unit) per opcode/tx, not a single aggregate.

**Before concluding anything, re-check against §1:** a divergence on `SSTORE` confirms a known governance-set gap, not a finding; the real signal is `KECCAK256`/arithmetic. Precompile numbers belong in their own table, never blended with interpreted-opcode results.

---

## 8. Per-block execution time — a related, separate question

§4/§5 answer per-tx gas vs. compute. A separate, legitimate question came up alongside this work: the full per-block cost in the execution layer after consensus hands off a block — parse, execute every tx, begin/end-blockers, receipt generation, commit. No new RPC or kernel instrumentation is needed for this either.

**seid already logs it.** The `"execution block time"` line (`sei-cosmos/baseapp/abci.go`) carries `height`, `process_proposal_ms`, `finalize_block_ms`, `commit_ms`, and `total_execution_ms` per block, no config change, already running on every node.

**On a replayer specifically, this is clean.** A blocksyncing replayer never runs `ProcessProposal` (a live-consensus phase), so `process_proposal_ms` is ~0 and `total_execution_ms` (finalize plus commit) is the whole pipeline: parse, begin-blocker, execute all txs, end-blocker, receipt construction, result hash, commit.

**Two caveats, both verified against source rather than assumed:**

- **This does not generalize to a live validator.** Sei has an optimistic-processing path (`app/app.go`'s `FinalizeBlocker`) where tx execution can run asynchronously during `ProcessProposal`, overlapping consensus. On a validator, the same `finalize_block_ms` would measure mostly wait time, not real execution cost. Blocksync never triggers that path, so the number is clean only for a replayer, not a general property to carry over to validator data.
- **This is a whole-block aggregate across every message type, not an EVM-isolated number.** Real Sei blocks mix EVM traffic with native Cosmos messages — IBC transfers, staking, gov, bank sends — so `total_execution_ms` on an arbitrary block includes an unknown amount of non-EVM time. Trust it as an EVM-only proxy only for a block confirmed to be all-EVM; otherwise it answers "how long did this block take," not "how long did the EVM take in this block."

**A narrower, EVM-adjacent metric exists too.** `block_process_duration{type="optimistic_concurrency"}` covers just the parallel transaction-execution phase, excluding begin/end-blockers and commit — a more targeted signal for pure gas-vs-compute work, since gas is charged during tx execution, not the blockers.

**And one already-existing, EVM-only signal worth knowing about: `run_msg_latency`** (`sei-cosmos/baseapp/metrics.go`), a histogram labeled by exact message-type URL, recorded at `baseapp.go`'s message-routing loop. `run_msg_latency{type="/seiprotocol.seichain.evm.Msg/EVMTransaction"}` is a clean, low-cardinality (the label is message type, not per-tx), EVM-only per-message-execution-time histogram, zero code change required, and it already excludes ante-handler time (a sibling span/timer, not included in this window).

---

## 9. eBPF — only if §4/§7's primitive genuinely isn't enough

**Don't reach for this by default — it is no longer the primary path for either functionality as specified, and a peer collaboration (systems-engineer) confirmed this independently rather than just validating the existing approach.** `debug_traceTransactionProfile` with `callTracer` already gives real per-tx gas + execution time without any kernel-level instrumentation (r=0.8093 on a diverse sample, §4 step 9). Specifically ruled out this session:

- **Per-block wall-clock / begin-end-blocker overhead:** not an eBPF problem. Block wall-clock is already logged (`latency_ms`); decomposing begin-blocker/deliver-tx/end-blocker is an in-process instrumentation change (~20 lines), not a kernel probe — and differencing an eBPF block-total against a summed-per-tx RPC number doesn't cleanly isolate anything anyway, since the two sides come from different execution paths (parallel/untraced vs. serial/traced).
- **True per-opcode dispatch timing:** `EVMInterpreter.Run` has no per-opcode symbol (below), and even with one, a uprobe's ~20-50ns overhead would dwarf a ~1ns `ADD`. The differential-bytecode-loop (§6) remains the correct technique — no eBPF involved.
- **CPU-time vs. wall-clock noise:** cheaper fixes cover this — min-of-N + forced GC (§4 step 7) exploit that the noise is one-sided; `runtime/metrics` (`/gc/pauses`, `/sched/latencies`) quantifies GC/scheduling contribution with no elevated privilege at all.

**The one kernel-level tool worth considering — and it isn't a uprobe.** `perf stat -p <pid>` for cycles/instructions on the differential-loop's bytecode runs is a hardware counter read, not a Go-symbol attach — none of the stack-copy/uretprobe/thread-migration hazards below apply to it. It's immune to *off-CPU descheduling* jitter in a way wall-clock `time.Since` isn't — but it's process-wide, so it still counts GC worker threads' own cycles; that only cancels out in the `cycles(A) − cycles(B)` differential under an assumption of roughly equal GC load between the two variants, not as an absolute guarantee. With that caveat, `(cycles(A) − cycles(B)) ÷ iterations` is a genuinely independent cross-check on the differential-loop's nanos-per-opcode numbers. Treat this as a **deferred, second-iteration cross-validation** — worth running only if you still see unexplained variance after the min-of-N discipline, or if a repricing recommendation needs to rest on absolute per-opcode precision rather than relative ordering. It still needs `CAP_PERFMON` and the same human sign-off as anything else in this section.

If you do need actual uprobes/`offcputime`/`profile`, the privileged-attach gate and symbol-resolution guidance below still apply.

**The gate:** any eBPF uprobe, `offcputime`, or CPU `profile` run against `seid` requires a **privileged kernel capability grant** (`CAP_BPF`/`CAP_SYS_ADMIN`/`CAP_PERFMON`) on a pod in **harbor — a shared EKS cluster**. This is root-on-node — and root-on-node on EKS means more than co-tenant pod access: it reaches the node's IAM role via IMDS (`169.254.169.254`), kubelet credentials, and any system DaemonSet's tokens/secrets scheduled on that node. Pinning to a dedicated node (§3) removes co-tenant *engineer* pods, not the node's AWS-credential surface. `eng-<alias>` namespaces carry no Pod Security Standard label and harbor has no cluster-wide PSS default — the effective PSS level is Kubernetes' own default, `privileged`, enforced nowhere. **This is an unhardened gap, not a sanctioned path.**

**Do this, in order, before attaching anything:**

1. **Get sign-off first, and require an affirmative response — silence is not consent.** Surface what you're attaching, to what, on which pod, for how long, and to whom (the harbor platform/security owner — confirm the current contact/channel with the team; don't proceed on an assumed default). Wait for an explicit "yes" from a human. An agent driving this runbook must not treat "I posted and got no objection" as approval.
2. Pin to a dedicated node (§3's `sei.io/dedicated-node` annotation) for the duration — this narrows co-tenant blast radius, not the node-credential exposure named above.
3. Scope the capability grant to the minimum — prefer `CAP_BPF`+`CAP_PERFMON` over the broader `sysadmin` profile wherever the tool supports it: `kubectl debug --target=seid -it <target-node>-0 --image=<bpftrace-image> --profile=<minimal-profile-your-tool-supports>` (fall back to `--profile=sysadmin` only if the narrower profile doesn't work for the specific probe you need, and say so explicitly when you do). `--target=<container>` joins only that pod's PID namespace regardless of which profile you use.
4. **Set a hard time bound and tear down explicitly.** State the max duration in the sign-off ask (step 1), and when you're done, delete the debug container/pod and confirm it's gone — don't leave a root-on-node ephemeral container running on shared infrastructure with no revocation step.
5. If this becomes recurring, that's the signal to open a platform PR for a dedicated profiling namespace with its own restricted-by-default PSS + a narrow, auditable exception — a platform decision, not something to self-approve.

Resolve real symbols before writing any probe — `sei-chain`'s `go.mod` pulls go-ethereum via a `replace` directive, which preserves the original import path in compiled symbol names:

```sh
kubectl -n eng-<alias> debug --target=seid -it <target-node>-0 --image=<toolbox-with-go> --profile=<minimal-profile-your-tool-supports> -- \
  go tool nm /proc/<seid-pid>/root/usr/bin/seid | grep -E 'StateTransition\)\.Execute|EVMInterpreter\)\.Run'
```

(symbol resolution via `go tool nm` needs no elevated capability beyond joining the pod's PID namespace — `--profile=sysadmin` should not be needed here at all; reserve the broader profile for the actual probe attach in the step above, and only if the minimal profile doesn't cover it.)

- **Per-tx bracket:** `github.com/ethereum/go-ethereum/core.(*StateTransition).Execute` — Sei's `x/evm/keeper/msg_server.go` (`applyEVMMessage`) calls this directly, bypassing the upstream `ApplyMessage` wrapper.
- **Per-opcode dispatch (not individually probeable):** `github.com/ethereum/go-ethereum/core/vm.(*EVMInterpreter).Run` — indirect function-table dispatch, no stable per-opcode symbol. This is why §6 constructs isolation via bytecode rather than probing the loop.

Prefer `offcputime` and a PID-filtered on-CPU `profile` over raw entry/return timing. On a replayer under heavy state I/O, `offcputime` overhead scales with context-switch rate and is no longer negligible — **A/B the replay throughput (blocks/s) with the probe attached vs. detached before trusting any number**.

Raw per-invocation duration via `uprobe`/`uretprobe` is rehearsal-only: Go's stack-copying runtime and `uretprobe`'s return-address rewrite can interact badly, and a goroutine can migrate OS threads mid-call, breaking a `tid`-keyed entry/return pairing.

---

## 10. Failure modes (quick reference)

| Symptom | Cause | Fix |
|---|---|---|
| Replayer hits `wrong Block.Header.AppHash: expected X, got Y` and evicts every peer it asks | Binary version doesn't match what actually produced the history being replayed (§2) | Pin to the release tag matching the live chain (§2a), or deliberately use `mock_chain_validation` (§2b) if testing unreleased code and you accept compounding divergence from real history. |
| `debug_traceTransactionProfile`/`debug_traceCall` returns `"block number ... is beyond max lookback of 10000"` | Collected tx hashes, then profiled them later; the replayer's rapid catch-up aged the target height out of `MaxTraceLookbackBlocks` (§4 step 8) | Profile immediately after finding each tx, or wait for the node to fully catch up to live tip before a bulk pass. |
| Correlating gas against whole-block `latency_ms` gives a weak/inconsistent result | Whole-block wall time is diluted by non-EVM per-block work and (on a replayer) blocksync contention — verified this session (r=0.301) (§1) | Use `debug_traceTransactionProfile`'s `executionNanos` with `{"tracer":"callTracer"}`, not a log-derived block-level proxy and not the default StructLogger invocation (§4 step 6) — verified r=0.8093 on a diverse same-session sample, not a general constant. |
| Gas-vs-time correlation looks suspiciously perfect (near 0.99) | Default `debug_traceTransactionProfile` invocation attaches a full StructLogger inside the timed window; overhead scales with opcode count/gas (§4 step 6) | Re-run with `{"tracer":"callTracer"}` and compare — verified this session: ~2x overhead on one tx (65.1ms default vs. 34.7ms callTracer). |
| Gas-vs-time correlation looks near zero even though the tool is sound | Sample had little/no variance in gas (e.g. repeated bot/arbitrage calls in a narrow block window) — correlation is meaningless without variance in the independent variable, not evidence the relationship is weak (§4 step 9) | Widen the collection window or deliberately sample across gas magnitudes before computing a correlation. |
| `kubectl describe node` shows an instance type outside `r6i`/`r7i` | `controller-config.yaml` or NodePool requirements drifted between harbor and prod | Halt — the timing result won't reflect prod hardware. Diff `clusters/{harbor,prod}/sei-k8s-controller/controller-config.yaml` and `clusters/{harbor,prod}/default/nodepool-*.yaml` before proceeding. |
| Path A (`debug_traceCall`) gives gas but no execution time | `TraceCall` was never wired to the `phaseDurations` profiling mechanism — a real, unclosed gap, not a config issue (§5) | Use Path B: submit a real signed tx against your own restored-to-`<Y>` node, then `debug_traceTransactionProfile` its mined hash. |
| Trying to replay block `X` against a `Y` that isn't `X-1` | No existing tooling supports the general case (§4) | This is new engineering, not a config change — confirm whether `Y=X-1` actually answers the real question before treating it as a blocker. |
| "Gas doesn't match compute" conclusion on `SSTORE` | Tested a governance-mutable param as if compute-derived (§1) | Reframe as confirming the known divergence; re-run on `KECCAK256`/arithmetic for the real test. |
| Attaching an eBPF probe changes replay throughput (blocks/s) | High context-switch rate on a fast replayer makes `offcputime`/probe overhead non-negligible (§9) | A/B throughput probe-on vs. probe-off; narrow the window or lower sampling rate. Consider whether you need eBPF at all — §4's primitive may already answer the question. |
| Can't get sign-off / told to stop before attaching anything privileged | §9's gate working as intended | Platform team's call — escalate, don't self-approve a scoped-down version of the same attach. |
| Correlating gas against a mixed-traffic block's `total_execution_ms` gives an inconsistent EVM-time proxy | The block isn't all-EVM — native Cosmos messages (IBC, staking, gov) are baked into the same aggregate (§8) | Confirm the sampled block is all-EVM before treating it as an EVM-time proxy, or use `run_msg_latency{type=".../evm.Msg/EVMTransaction"}` instead (§8). |
| Considering OpenTelemetry tracing spans instead of `debug_traceTransactionProfile` for per-tx timing | Looks like it would save a per-tx RPC round-trip | Don't, without reading Appendix A first — built and tested with real data, found an irreducible deterministic node-level confound in both concurrent and serial execution modes. |

---

## 11. Pre-flight checklist

- [ ] Confirmed the target node landed on `r6i`/`r7i` via `sei-node`/`sei-archive` (§3) — don't trust the default without checking.
- [ ] **Chosen §2a (release-pinned) or §2b (`mock_chain_validation`) deliberately**, not defaulted to an arbitrary image tag. If §2b, confirmed `sei_unsafe_validation_skipped_total` is being watched, not ignored.
- [ ] **Functionality 1:** replayer restoring to `<Y>` and replaying forward (§4); confirmed `Y=X-1` actually answers the intended question, or explicitly flagged that it doesn't.
- [ ] **Functionality 1/2 measurement:** calling `debug_traceTransactionProfile` with `{"tracer":"callTracer"}` — **not the default invocation**, which attaches a full StructLogger and inflates `executionNanos` by roughly 2x (§4 step 6). Using `result.profile.phases.executionNanos` (not `totalNanos`, not a log-derived latency proxy) for timing, `result.trace.gas`/`gasUsed` for gas.
- [ ] **Not subtracting `result.profile.store` totals from `executionNanos`** — that arithmetic is broken (different time windows; caught in peer review, §4 step 6). Cleanly separating interpreter compute from state I/O per tx needs a sei-chain change, not a subtraction on today's fields.
- [ ] **Timing runs use min-of-N** (forced GC before each, dedicated/quiesced node, §4 step 7), not a single sample per tx.
- [ ] **Traced promptly** relative to `MaxTraceLookbackBlocks` — not collecting a large batch of tx hashes to profile long after the fact on a still-catching-up node.
- [ ] **Functionality 2:** using Path B (real submitted tx + §4's primitive) unless a genuine read-only-simulation requirement forces Path A — in which case, the execution-time gap in `debug_traceCall` is flagged explicitly, not worked around with a fabricated number.
- [ ] Recorded live gas parameter values (e.g. `SeiSstoreSetGasEIP2200`) + image digest before drawing conclusions.
- [ ] **Sample size covers a real range of gas magnitudes/contracts, and you've checked for variance before computing a correlation** — a narrow, bot-dominated window with near-constant gas will give a meaningless near-zero r regardless of the true relationship (§4 step 9).
- [ ] **Correlating `executionGas = gasUsed - IntrinsicGas(...)` against `executionNanos`**, not raw `gasUsed` (§4 step 10) — the intrinsic-gas floor moves gas without moving time and dilutes the signal, especially at the low-gas end.
- [ ] Re-checked results against §1 (`SSTORE`/precompile confounds) before concluding anything about whether gas tracks compute.
- [ ] **Named explicitly whether the question needs state-opcode cost-vs-state-size scaling** (§4 step 11) — if so, flagged that as an open engineering gap, not something a `Y=X-1` measurement answers.
- [ ] **If reading whole-block execution time (§8), confirmed the sampled block is all-EVM** before treating `total_execution_ms` as an EVM-time proxy, and confirmed whether the node is a replayer or a live validator (the optimistic-processing caveat, §8).
- [ ] Confirmed eBPF uprobes/`offcputime` are actually necessary (§9) before requesting sign-off for a privileged attach — §4-§7 already answer both functionalities as specified without them; `perf stat` cycle counters (§9) are the one kernel-level tool worth considering, and only as a deferred cross-validation.

---

## Appendix A: Invalidated avenues (read before re-proposing these)

One path was investigated in depth for this project, built, run against real data, and found not fit for purpose. Documented here so the next engineer doesn't re-spend the time re-discovering why.

### A.1 OpenTelemetry tracing for per-tx gas-vs-time correlation

Sei ships real OTel tracing (Jaeger exporter, currently disabled by default), with a full span tree — `Block → BeginBlock/DeliverTxBatch(→SchedulerExecuteAll→SchedulerExecute→AnteHandler+RunMsgs)/MidBlock/EndBlock` — carrying `txHash`/`absoluteIndex`/`txIncarnation` on the OCC scheduler spans. Given that `debug_traceTransactionProfile` (§4) needs a per-tx RPC call each, tracing looked like an attractive way to get the same numbers as a byproduct of normal block processing, no extra calls needed.

**Three independent specialist reviews (systems-engineer, observability-platform-engineer, opentelemetry-expert) first found real, structural problems, later confirmed against actual data:** a process-global mutex around every span-start call (`sei-cosmos/utils/tracing/tracer.go`) serializes span creation across OCC's concurrent workers; no trace backend exists anywhere in harbor; the collector endpoint is hardcoded to `localhost:14268`, forcing a sidecar-in-pod deployment shape; the enabling flag is CLI-argument-only, not reachable through the SeiNode CRD's config overrides; gas is not on any span, requiring a secondary height+txHash join regardless.

**Built and tested it anyway, with real data, to settle the efficacy question rather than reason about it abstractly.** Stood up two tracing-enabled replayer nodes in `eng-fromtherain` (a wrapper image backgrounding an in-process Jaeger collector before exec'ing the real seid binary, so AppHash fidelity against pacific-1 was unaffected) — one running default concurrent OCC execution, one with `chain.concurrency_workers=1` forcing strict serial execution — both restored from the identical S3 snapshot, so both replayed the identical real historical chain.

**The result was a real, informative negative:**

| | r(gas, total span time) | r(gas, `RunMsgs` span time) |
|---|---|---|
| Concurrent OCC (n=190 EVM txs) | 0.736 | 0.686 |
| Strictly serial (same 190 txs) | 0.690 | 0.574 |
| Already-validated `debug_traceTransactionProfile` (§4) | — | 0.809 |

Serializing execution did not close the gap to the validated method; it made the clean-signal correlation worse. Both modes showed a recurring outlier: certain transactions' `AnteHandler` span durations inflated 100-155x relative to their `RunMsgs` span, on the same transactions in both datasets (confirmed by joining on identical `txHash` across the two nodes). Because strict serial execution has zero cross-goroutine overlap (confirmed directly from span start-times — back-to-back, ~4µs gaps), this ruled out OCC/mutex contention as the cause. Comparing the same outlier transactions across the two independent nodes (separate pods, separate Jaeger processes, separate GC timing) showed the inflation lands on the *same transactions*, at matching magnitudes, on both (Jaccard 0.92) — which rules out random per-process contention too, including the in-process Jaeger collector stealing CPU from `seid`. The remaining explanation both datasets are consistent with: a deterministic, per-height node-level stall — most likely GC mark-assist or a memIAVL commit/snapshot-flush cycle — that inflates whatever span happens to be open at that moment, landing more often on longer (higher-gas) transactions and inflating the correlation on exactly the end that makes it look better.

**Verdict: not fit for absolute per-tx gas-vs-execution-time correlation, in either execution mode.** The already-validated `debug_traceTransactionProfile` isolates a transaction's re-execution from the live block-delivery pipeline entirely — no concurrent scheduler, no GC/commit cycle landing mid-measurement — which is the structural reason it measures closer to true compute cost. OTel tracing remains genuinely useful for a narrower, different question: structural/relative phase attribution within a block or transaction (what fraction of the time went to ante-handling vs. message execution vs. begin/end-blockers), not for this correlation.

---

## References

- Sei-EVM gas/precompile specifics: `/evm` skill — `kit-evm-parity-gas.md`, `kit-sei-precompiles.md`; `x/evm/types/params.go` in `sei-chain` for the governance-mutable param set.
- Version-pinning / `mock_chain_validation`: `sei-chain` — `sei-tendermint/types/consensus_policy.go`, `consensus_policy_mock_chain_validation.go`, `consensus_policy_default.go`, `policy_metrics.go`; `.github/workflows/ecr.yml`, `.github/workflows/nightly-ecr.yml` for the image-tag scheme.
- Measurement primitive: `sei-chain` — `evmrpc/trace_profile.go` (`debug_traceTransactionProfile`, `profiledTraceTx`, `phaseDurations`; the store trace dump spans the whole statedb lifetime including replayed preceding txs, not just the target tx — this is why `result.profile.store` can't be subtracted from `executionNanos`), `evmrpc/simulate.go` (`initializeBlock`, `replayTransactionTillIndex`, `StateAtBlock`, plain `Call`), `evmrpc/tracers.go` (`TraceCall`), `evmrpc/block_trace_profiled.go` (`debug_traceBlockByNumber/Hash` — profiling exists but is passed `nil` here, not surfaced; also the `config==nil` → default `StructLogger` path and the `executionNanos` bracket around `core.ApplyTransactionWithEVM` — this is why the default invocation over-times relative to `callTracer`), `evmrpc/config/config.go` (`MaxTraceLookbackBlocks`), `sei-cosmos/types/context.go` (`WithIsTracing` — where the store tracer gets minted once for the whole trace), `sei-cosmos/types/tracer.go` (`StoreTraceDump` — read/write stats tracked separately).
- Intrinsic gas (§4 step 10): go-ethereum fork's `core/state_transition.go` (`IntrinsicGas`); Sei's call sites — `app/app.go:2016`, `app/ante/evm_checktx.go:107`, `x/evm/ante/basic.go:51` — all identical, all with fork-activation flags hardcoded `true`.
- Per-block execution time (§8): `sei-cosmos/baseapp/abci.go` (the `"execution block time"` log line), `sei-cosmos/baseapp/metrics.go` (`run_msg_latency` and the rest of the OTel metric set), `app/app.go` (`FinalizeBlocker`'s optimistic-processing path), `x/evm/keeper/msg_server.go` (`EVMTransaction` — confirms the exact message-type URL to filter `run_msg_latency` by).
- OTel tracing investigation, invalidated (Appendix A): `sei-cosmos/utils/tracing/tracer.go` (the span-start mutex, the hardcoded collector endpoint), `sei-cosmos/tasks/scheduler.go` (the OCC scheduler spans), `sei-cosmos/server/start.go` (the CLI-only `--tracing` flag).
- Snapshot restore selection: `seictl` — `sidecar/tasks/snapshot_restore.go` (`resolveKeyForHeight` — the authoritative source for `TargetHeight`'s "highest snapshot <= height" semantics; the `SeiNode` CRD's own doc comment in `sei-k8s-controller`'s `api/v1alpha1/common_types.go` only describes the zero-value case and reads differently — trust seictl's source over it).
- Harbor chain spin-up mechanics: `/harbor-dev` skill, `references/ephemeral-chain-flow.md`, `references/seictl-cli.md`, `references/harbor-cluster.md`.
- Hardware-parity mechanism: `sei-k8s-controller` — `internal/platform/platform.go` (`NodepoolForMode`), `internal/task/bootstrap_resources.go`, `internal/noderesource/noderesource.go` (also home of the `sei.io/dedicated-node` annotation and the documented no-SeiNode-condition-on-stuck-Pending behavior), `internal/planner/replay.go` (`buildRunningPlan` — why an annotation-only change on a Running replayer doesn't trigger a pod rebuild), `internal/planner/planner.go` (`imageDrifted` — image-only, not annotation-aware); `sei-protocol/platform` — `clusters/{harbor,prod}/sei-k8s-controller/controller-config.yaml`, `clusters/{harbor,prod}/default/nodepool-{default,archive}.yaml`.
- Harbor `eng-<alias>` Pod Security / admission posture: `sei-protocol/platform` — `clusters/harbor/engineers/base/namespace.yaml`, `clusters/prod/walle/{namespace,podcomposition-policy}.yaml` (the posture to eventually mirror), `clusters/harbor/chaos-mesh/chaos-mesh.yaml`, `clusters/harbor/admission/tenant-hostname-policy.yaml`.
- Execution entry points (§9 only): `sei-chain` — `x/evm/keeper/msg_server.go` (`applyEVMMessage`); the vendored go-ethereum fork's `core/state_transition.go` (`StateTransition.Execute`) and `core/vm/interpreter.go` (`EVMInterpreter.Run`) — resolve the fork version via `go.mod`, don't hardcode it here.
