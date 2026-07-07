# Benchmarking EVM Gas vs. Real Execution Time on Sei

**Audience:** engineers (and their agents) on the gas-repricing project investigating whether Sei's EVM gas schedule tracks real compute cost. Covers both target functionalities: (1) replaying a real historical block against restored state and measuring gas + execution time per tx, and (2) executing arbitrary bytecode against a specific historical state and measuring the same.
**Scope:** standing up a replayer node (functionality 1) and an archive-backed environment (functionality 2) on harbor with confirmed prod-matching hardware, the privileged-attach gate for eBPF instrumentation, concrete probe design against real `seid`/go-ethereum symbols, and the confounds that will produce a false result if ignored. **No sei-chain source changes anywhere in this procedure** — everything here is external client traffic + kernel/process-level observation of a binary you don't touch.
**Not in scope:** *why* Sei's gas schedule has the specific values it does (a governance/design question — capture a resulting change proposal as a `/design` doc, not here). Running any of this against a shared/long-lived/production chain, or against the canonical shared archive node in a way that mutates its state.

> **Read this whole section before running anything.** Four things make the naive version of this experiment **silently wrong**: (1) at least one opcode's gas cost (`SSTORE`) is a **governance-mutable Sei chain param that already diverges from upstream go-ethereum** — using it as your "does gas match compute" baseline tests a value that was hand-tuned away from compute cost *by design*; (2) per-opcode timing via a uprobe **is not achievable** on this binary — the interpreter's opcode dispatch is an indirect call with no per-opcode symbol; (3) attaching any eBPF probe to a `seid` pod on harbor requires a **privileged kernel capability grant on a shared EKS cluster** — nothing in harbor's current namespace configuration technically blocks this, but that is an unhardened gap, not a green light; (4) **arbitrary-bytecode-against-historical-state (functionality 2) has an unverified RPC dependency** — confirm it before you build a campaign on top of an assumption. §1, §4, §5, and §6 exist to keep you out of these holes.

---

## 0. Substitutions used throughout

| Placeholder | Meaning |
|---|---|
| `<alias>` | your harbor engineer alias; namespace is `eng-<alias>`. |
| `<Y>` | the historical height whose post-block state you're restoring/executing against. |
| `<X>` | the historical block (functionality 1 only) whose transactions you're re-executing and timing; `X > Y`. |
| `<replay-node>` | the `SeiNode` in `replayer` mode restoring to `<Y>` and replaying forward (§3). |
| `<archive-node>` | the archive-mode `SeiNode` or existing shared archive infra used for functionality 2 (§4). |
| `<opcode>` | the single EVM opcode under test for a given functionality-2 run (§5). |
| `<seid-pid>` | the PID of the `seid` process inside the target pod — resolve fresh each pod restart. |
| `<sei-chain-checkout>` | your local `sei-chain` clone, used to resolve real symbol names before writing any probe (§7). |

---

## 1. Mental model — what this can and cannot prove (read first)

The question: for a given opcode, does the gas charged scale with the real wall-clock cost of executing it? The honest answer requires knowing which parts of the gas schedule were **derived from measured compute** vs. **set by other means** (governance, upstream inheritance, a flat precompile fee).

**What's a fair target vs. a confound:**

| Category | Examples | Why |
|---|---|---|
| **Primary target — interpreter-local, no state backend** | `KECCAK256`, pure arithmetic (`ADD`/`MUL`/`PUSH`/`DUP`/…) | Executes entirely in the interpreter loop with no I/O — the cleanest compute-bound signal. |
| **Secondary target — real, but control for it** | `SLOAD`/`SSTORE` | **`SSTORE`'s set-gas is a governance-mutable Sei chain param (`SeiSstoreSetGasEIP2200`, `x/evm/types/params.go:43`, code default `20000`, governance-set higher on mainnet — query it live, never hard-code either value)** — a divergence here confirms a known, intentional gap, not a finding. If you test it anyway, separate cold-slot and warm-slot runs; Sei's state backend is memIAVL/flatKV, not an MPT, and cache-warmth dominates. |
| **Separate, expected-to-diverge category — never blend into "opcode gas"** | Sei precompiles (staking, distribution, oracle, etc. — see `/evm` skill's `kit-sei-precompiles.md`) | Precompile execution is a Go-native Cosmos-module call outside the interpreter entirely; gas is a governance-tunable, structurally decoupled fee. |

**Confounds that apply regardless of functionality:**

- **Per-tx fixed overhead** (signature verification, the Cosmos ante handler, the EVM↔Cosmos StateDB bridge) is real wall-clock time attributable to *no* opcode — it dilutes a measurement unless your sample size / loop count makes it a small fraction of the total.
- **Optimistic concurrent execution (OCC) — this breaks naive per-tx correlation for functionality 1.** Sei applies a block's transactions through a **parallel scheduler**, not a serial loop: `DeliverTxBatch` → `tasks.NewScheduler(app.ConcurrencyWorkers(), …)` → `ProcessAll` spawns up to `ConcurrencyWorkers` goroutines (localnode default `concurrency-workers = 4`, `occ-enabled = true`; verified `app/abci.go:243`, `sei-cosmos/tasks/scheduler.go`), and re-executes any tx whose read/write set conflicts. **This same block-delivery path runs during replay** — a replayed historical block is applied by the OCC scheduler, not single-stepped. Consequences you must design around:
  - **Functionality 2** (synthetic bytecode, §5) avoids all of this by construction — one tx per block means one task, no contention, no re-execution (§5).
  - **Functionality 1** (replaying a real multi-tx block, §3) does **not** get clean per-tx timing for free: multiple `StateTransition.Execute` calls run concurrently on different OS threads, and a conflicting tx runs `Execute` more than once. An ordinal-by-call-count bracket (which is what §7's `tid`-keyed uprobe gives you) therefore maps to *neither* a specific tx *nor* a single execution — it silently produces **block-level aggregate timing, not per-tx timing**. See §3's correlation subsection for the fix (disable OCC for the measurement run) and its limits.
  - **Determinism:** even setting correlation aside, OCC re-execution is not a deterministic function of the tx list alone (incarnation counts depend on scheduling), so repeated replays of the same `<X>` can differ. If timing looks bimodal/inconsistent across repeated replays, suspect re-execution before suspecting your probe.

---

## 2. Hardware parity — already automatic, verify it rather than configure it

**You do not need to request specific hardware.** `sei-k8s-controller` picks the Karpenter NodePool per node mode automatically: `internal/platform/platform.go`'s `NodepoolForMode()` — archive-mode nodes get `c.NodepoolArchive`, every other mode (including `replayer`) gets `c.NodepoolName` — and wires the matching toleration + required node affinity onto the pod spec (`internal/task/bootstrap_resources.go:203-227`, mirrored in `internal/noderesource/noderesource.go:454-479`). Both `clusters/harbor/sei-k8s-controller/controller-config.yaml` and `clusters/prod/sei-k8s-controller/controller-config.yaml` (in `sei-protocol/platform`) set the **identical** values: `nodepoolName: sei-node`, `nodepoolArchive: sei-archive`, `tolerationKey: sei.io/workload`. Harbor's `clusters/harbor/default/nodepool-{default,archive}.yaml` carry the **same** `r6i`/`r7i` instance-family, Nitro-hypervisor, on-demand requirements as `clusters/prod/default/nodepool-{default,archive}.yaml`. Net: a replayer or archive `SeiNode` in your `eng-<alias>` namespace lands on the same instance class as its prod counterpart, with zero engineer-side configuration.

**Verify it anyway before trusting a timing result** — a Karpenter requirements drift between clusters would silently invalidate every measurement:

```sh
NODE=$(kubectl -n eng-<alias> get pod <replay-node>-0 -o jsonpath='{.spec.nodeName}')
kubectl get node "$NODE" -o jsonpath='{.metadata.labels.node\.kubernetes\.io/instance-type}{"\n"}{.metadata.labels.karpenter\.sh/nodepool}{"\n"}'
```

Expect an `r6i.*`/`r7i.*` instance type and `nodepool=sei-node` (or `sei-archive` for an archive-mode node). If you see anything else, halt and check for a stale `controller-config.yaml` divergence between harbor and prod before proceeding — don't benchmark on it.

---

## 3. Functionality 1 — replay block `<X>` against state after `<Y>`, measure gas + time

**The state-restore-and-forward-replay mechanism is real, documented tooling** — this is the same machinery behind `validating-flatkv-memiavl-parity-via-sharded-replay.md`, used here for timing instead of storage-parity comparison. You do **not** need that runbook's flatKV-vs-memIAVL differential pair; stand up a single replayer engine.

1. **Render a `replayer`-mode `SeiNode`** targeting `<Y>`: `spec.replayer.snapshot.s3.targetHeight: <Y>` (per `validating-flatkv-memiavl-parity-via-sharded-replay.md:148`) — the seictl sidecar restores the highest snapshot `<= <Y>` and syncs forward from there, peering a shared full-history archive over p2p as its block source. Render via the `/harbor-dev` skill's PR-based flow into `harbor-engineering-workspace`, same as any other `SeiNode`.
2. **Let it replay forward past `<X>`.** There is no documented flag to halt exactly at a target height — the tooling (and its shadow-comparator sibling) is built for continuous mass replay (~40 blocks/s/node, paged in 100-block chunks for the parity use case). You don't need a halt, but note that you **cannot reactively bracket the `<X>` window** — RPC height/logs report `<X>` only *after* it commits (§3a). Instead, attach the probe *continuously across a replay span containing `<X>`* and correlate afterward by height/tx; poll `.status`/RPC height or tail logs only to confirm `<X>` has been applied and to bound the span. Letting the node continue replaying past `<X>` is harmless.
3. **Gas is already there per-tx** — the replayed node has genuinely applied `<X>`'s transactions; read `gasUsed` per tx the same way you would from any node (`seid query evm tx <hash>` / `eth_getTransactionReceipt`), no comparator needed for a single-block read.
4. **Execution time is the actual gap.** Neither this replay mechanism nor its comparator captures timing anywhere — the comparator's three layers cover AppHash/`LastResultsHash`, per-tx `code`/`gasUsed`/`log`/`events`, and post-state storage/balance/code/nonce; there is no timing field in any of them. This is exactly what §7's eBPF instrumentation is for, bracketed around the height-`<X>` transition you identified in step 2.

### 3a. Per-tx correlation inside a multi-tx block — the real constraint (read before trusting any per-tx number)

Bracketing `StateTransition.Execute` with a `tid`-keyed uprobe/uretprobe (§7) gives **block-level aggregate timing, not per-tx timing**, on any block with more than one tx — because the OCC scheduler runs several `Execute` calls concurrently on different threads and re-runs conflicting txs (§1). Two separate problems: (a) *which* invocation is *which* tx — `Execute()` takes **no arguments** (receiver-only; the tx lives in the `st.msg *Message` field), so there is no arg to read a hash from, and ordinal-by-call-count is ambiguous under concurrency + re-execution; (b) a `tid`-keyed entry/return pair can be split across threads by goroutine migration, worse under OCC's higher scheduling pressure.

**Fix — serialize the measurement run by disabling OCC.** Render the replayer with `occ-enabled = false` and `concurrency-workers = 1` (app.toml keys; wired via `baseapp.SetOccEnabled`/`SetConcurrencyWorkers`, verified). The block-delivery path then takes its non-OCC serial branch (`!ctx.IsOCCEnabled()`) — one `Execute` at a time, in tx order, no conflict re-execution — so ordinal-by-call-count *does* map cleanly to the Nth tx in `<X>`. **This does not corrupt the measurement you care about:** gas is charged per-tx independently of the parallel schedule, and a tx's own interpreter/state work is the same instruction stream serial or parallel; you are removing cross-tx cache/scheduler interference, not changing the tx's compute. Confirm the SeiNode/seictl spec actually exposes these app.toml keys before relying on it — **VERIFY**; if it doesn't, this is a `/harbor-dev` spec gap to close, not a workaround to skip.
   - **If OCC cannot be disabled** on the replayer: state plainly that functionality 1 yields **block-level aggregate `Execute` time only** (sum over the block's txs + any re-executions), correlatable to the block's *total* `gasUsed`, not per-tx. Per-tx then requires a compiled CO-RE tool that reads `st.msg` from the receiver (pointer + resolved field offsets) and keys by goroutine id, not tid — fragile, version-pinned to the struct layout, and out of scope for a bpftrace one-liner. Don't claim per-tx numbers you can't attribute.
   - **Determinism bonus:** serial application also removes the OCC re-execution non-determinism (§1), so repeated replays of `<X>` are directly comparable.

**Window detection is post-hoc, not reactive.** You cannot *react* to entering block `<X>`: at ~40 blocks/s a block's application window is ~25 ms, and RPC height / logs only report `<X>` *after* it has committed — by the time you observe it, the `Execute` calls are done. So the probe must be **attached and aggregating continuously across a replay span that contains `<X>`**, then filtered afterward by height/tx. Under the disable-OCC serial path this is clean (aggregate per-tx in order, join to the tx list of `<X>` after); with a fast continuous replay you also want to slow the arrival into `<X>` (small `concurrency-workers`, or throttle the block source) so the window isn't lost in sampling noise.

---

## 4. Functionality 2 — execute arbitrary bytecode against state after `<Y>`, measure gas + time

**This has an unverified dependency — confirm it before building anything on top of it.** Two paths, in preference order:

### Path A (preferred) — read-only historical `eth_call` with a state override

If Sei's EVM JSON-RPC supports `eth_call` with a historical `blockNumber`/`blockTag` **and** a state-override object (to simulate the bytecode as if already deployed, without a real mutating tx), this is the clean answer *for gas and correctness*: point it at an archive node's RPC, at height `<Y>`, no real mutating tx. **`operating-archive-node-byov.md` says nothing about this** — it's pure PV/EBS provisioning — so this capability is not documented anywhere in this repo's runbooks. **Confirm empirically before relying on it:** a trivial smoke test (a state-overridden `eth_call` for a known simple computation at a known historical height, checked against a hand-computed expected result).

**Path A does not solve the *timing* half on the shared archive node.** Execution time here comes from §6/§7 eBPF instrumentation, and you may not attach a privileged probe to the **canonical shared archive node** — that is the §6 gate and the shared-blast-radius rule, and it is *not* your node to instrument. So Path A splits:
- **Gas / correctness:** the state-overridden `eth_call` against the shared archive is fine (read-only, no mutation).
- **Timing:** either (i) accept only a client-side RPC round-trip latency as a coarse, network-contaminated proxy (not eBPF-clean — state this limitation, don't dress it as compute time), or (ii) run the *same* state-overridden `eth_call` against **your own private archive/replay node** (restored to `<Y>` as in Path B step 1) which you *are* cleared to instrument. Option (ii) recovers eBPF-grade timing but forfeits Path A's "no new infrastructure" benefit — at which point Path A and Path B converge on "a private node you own," and the only difference is read-only `eth_call` vs. a real submitted tx. Choose Path A(ii) over Path B when the state-override RPC works, to avoid mutating even your private node.

### Path B (fallback) — private writable fork of state-at-`<Y>`

If Path A's RPC capability doesn't exist or doesn't support the injection you need, use the same state-restore mechanism as §3 but stop treating it as a passive replayer: restore a `SeiNode` to height `<Y>` (`spec.replayer.snapshot.s3.targetHeight: <Y>`, same as §3 step 1), then instead of continuing historical replay, submit your own synthetic signed EVM transactions against it (`seid tx evm deploy` / `call-contract`, §5's bytecode). This gives you real trie depth / cache-warmth from genuine historical state (unlike a from-scratch empty genesis chain) at the cost of standing up and mutating your own private copy — never do this against the canonical shared archive.

Either path, **instrument the same node you're submitting to** — no cross-node clock correlation needed.

---

## 5. Crafting opcode-isolated bytecode (Path B / any synthetic-tx run)

**The differential-loop technique** isolates a single opcode's marginal cost without per-opcode instrumentation: write two bytecode variants identical except for the opcode under test.

- **Variant A (opcode-under-test):** a loop body of `JUMPDEST`, the target `<opcode>` (with operands pre-pushed), then loop-decrement/`JUMPI` back to `JUMPDEST`.
- **Variant B (baseline):** identical scaffold, target opcode replaced with a cheap no-op pair (`PUSH1 0x00`/`POP`) of known cost.

`(wall-clock(A) − wall-clock(B)) ÷ iteration count ≈ per-opcode marginal wall-clock cost`; the same subtraction on `gasUsed` gives the marginal gas cost to compare against it. This cancels loop-overhead opcodes and per-tx fixed overhead from both sides symmetrically.

```sh
seid tx evm deploy /path/to/<opcode>-loop.bin \
  --from=<key> --chain-id=<chain-id> --evm-rpc=<target-node-evm-rpc> \
  --gas-limit=<large-enough-for-iteration-count> --gas-fee-cap=<cap>

seid tx evm call-contract <deployed-addr> <calldata-hex> \
  --from=<key> --value=0 --chain-id=<chain-id> --evm-rpc=<target-node-evm-rpc>
```

Pick a large-but-fixed iteration count up front (large enough that fixed overhead is a small fraction of total gas/time; loop rather than unroll past a few hundred iterations — code-size cap). Run each variant multiple times and take the distribution, not a single sample — wall-clock has real variance from GC pauses, compaction, and scheduling noise that gas is blind to.

**Submission protocol (Path B only):** submit exactly one transaction, wait for its block to finalize and its receipt to confirm, before submitting the next. This sidesteps the OCC re-execution confound (§1) by construction — nothing contends for the same slot if only one tx is ever in flight. `StateTransition.Execute` (§7) runs more than once per submitted tx — once during `eth_estimateGas`/`CheckTx` simulation, again at `FinalizeBlock` delivery — expect 2-3 invocations, and disambiguate by taking the one whose timestamp falls inside the actual block-production window.

---

## 6. The privileged-attach gate — read before running `kubectl debug`

Any eBPF uprobe, `offcputime`, or CPU `profile` run against `seid` requires a **privileged kernel capability grant** (`CAP_BPF`/`CAP_SYS_ADMIN`/`CAP_PERFMON`) on a pod in **harbor — a shared EKS cluster** running other engineers' chains and `sei-k8s-controller` itself. This is root-on-node. The `/ebpf` skill's guardrail is unconditional here: **the gate is on the attach mechanism, not on how low-stakes the target is.** A throwaway replayer/archive-fork node does not waive it.

**What's actually true about `eng-<alias>` today (verified against the platform repo, not assumed):** `eng-<alias>` namespaces carry no Pod Security Standard label (`clusters/harbor/engineers/base/namespace.yaml`), and harbor has no cluster-wide PSS default either (`clusters/harbor/admission/` contains only an unrelated tenant-hostname policy) — the effective PSS level is Kubernetes' own default, `privileged`, enforced nowhere. Compare **prod**'s `walle` namespace: explicit `restricted` PSS + a `ValidatingAdmissionPolicy` blocking ephemeral containers outright. Harbor's `eng-<alias>` has no equivalent. **This is an unhardened gap, not a sanctioned path**, and shared-node blast radius is real independent of PSS — engineer workloads bin-pack onto harbor's Karpenter pools alongside other tenants' processes.

**Do this, in order, before attaching anything:**

1. **Get sign-off first** — surface what you're attaching, to what, on which pod, for how long, to the platform/security owner before running anything privileged.
2. **Pin to a dedicated node** — for the duration of the experiment, don't share the node with other tenants (§2's hardware-parity node is already tainted/dedicated by `sei-node`/`sei-archive`'s own scheduling — confirm nothing else landed there).
3. **Scope the capability grant to the minimum**, not `hostPID`: `kubectl debug --target=seid -it <target-node>-0 --image=<bpftrace-image> --profile=sysadmin` — `--target=<container>` joins only that pod's PID namespace. Prefer `CAP_BPF`+`CAP_PERFMON` over the broader `sysadmin` profile where possible.
4. **If this becomes recurring**, that's the signal to open a platform PR for a dedicated profiling namespace with its own restricted-by-default PSS + a narrow, auditable exception (mirror `clusters/harbor/admission/tenant-hostname-policy.yaml`'s exception shape; `clusters/harbor/chaos-mesh/chaos-mesh.yaml` is harbor's existing precedent for an elevated-privilege, opt-in-gated workload) — **a platform decision, not something to self-approve.**

---

## 7. Instrumentation — cheapest and least-privileged first

Don't reach for eBPF as step one.

**Tier 0 — free, no probe.** `gasUsed` from the receipt and `eth_estimateGas` are already there. Record the live gas parameters (e.g. `SeiSstoreSetGasEIP2200`, `x/evm/types/params.go:43`) and image digest before instrumenting anything — the comparison is meaningless without knowing what you're comparing against.

**Tier 1 — Cosmos SDK's own pprof, no elevated privileges.** The rendered `config.toml`'s `pprof_laddr` gives `go tool pprof` access with zero Kubernetes privilege beyond what you already have. **Verify empirically whether this surfaces EVM-interpreter frames at all** on the running pod before relying on it — don't assume from a source read alone. If empty or unhelpful, move to Tier 2.

**Tier 2 — eBPF (gated; §6 must be cleared first).**

Resolve real symbols before writing any probe — `sei-chain`'s `go.mod` pulls go-ethereum via a `replace` directive, which **preserves the original import path in compiled symbol names**, so the binary's symbols are still `github.com/ethereum/go-ethereum/...`. Confirm on the actual binary:

```sh
kubectl -n eng-<alias> debug --target=seid -it <target-node>-0 --image=<toolbox-with-go> --profile=sysadmin -- \
  go tool nm /proc/<seid-pid>/root/usr/bin/seid | grep -E 'StateTransition\)\.Execute|EVMInterpreter\)\.Run'
```

The two real boundaries (confirm line numbers against `<sei-chain-checkout>` before citing them to anyone):

- **Per-tx bracket:** `github.com/ethereum/go-ethereum/core.(*StateTransition).Execute` (`core/state_transition.go`) — Sei's `x/evm/keeper/msg_server.go` (`applyEVMMessage`) calls this directly, bypassing the upstream `ApplyMessage` wrapper.
- **Per-opcode dispatch (not individually probeable):** `github.com/ethereum/go-ethereum/core/vm.(*EVMInterpreter).Run` — indirect function-table dispatch, no stable per-opcode symbol. This is why §5 constructs isolation via bytecode rather than probing the loop.

**Prefer `offcputime` and a PID-filtered on-CPU `profile` over raw entry/return timing** — the actual highest-value signal is whether `Execute`'s wall-clock time is CPU-bound (interpreter work, what gas is supposed to meter) or blocked on state I/O (which gas categorically does not meter). Both are map-aggregated, so overhead is event-rate-bound not drain-bound — but "near-zero" holds only for **functionality 2's low tx rate**. **On a functionality-1 replayer running ~40 blocks/s with heavy state I/O, the context-switch rate is high, and `offcputime` overhead scales with context-switch rate** (`/ebpf` skill's `pack-perf-methodology.md` §7 caveat) — it is bounded but no longer negligible, and it perturbs the very replay you're timing. Per guardrail 3 (which holds even off-prod, because a perturbing probe corrupts a *measurement*): **A/B the replay throughput (blocks/s) with the probe attached vs. detached before trusting any number**; if attaching the probe moves replay throughput, the timing is contaminated — narrow the window, drop the sampling rate, or serialize (§3a) so the rate is low. Note also that during replay there is **no `CheckTx`/`eth_estimateGas` path** (blocks arrive already-finalized over p2p), so — unlike §5's submission flow — `Execute` fires once per tx *per OCC incarnation*, not 2–3× per tx:

```
# on-CPU attribution, PID-filtered
bpftrace -e 'profile:hz:49 /pid == <seid-pid>/ { @[ustack] = count(); }'

# off-CPU (blocked) time, PID-filtered
offcputime -p <seid-pid> -f 30
```

**Raw per-invocation duration via `uprobe`/`uretprobe` is rehearsal-only**, with two hazards to state plainly if you use it: (1) Go's stack-copying runtime and `uretprobe`'s return-address rewrite can interact badly — a known divergence, not hypothetical; (2) a goroutine can migrate OS threads mid-call on a blocking I/O read, breaking a `tid`-keyed entry/return pairing even at concurrency 1.

```
uprobe:/proc/<seid-pid>/root/usr/bin/seid:"github.com/ethereum/go-ethereum/core.(*StateTransition).Execute"
{ @start[tid] = nsecs; }
uretprobe:/proc/<seid-pid>/root/usr/bin/seid:"github.com/ethereum/go-ethereum/core.(*StateTransition).Execute"
/@start[tid]/
{ @dur = hist(nsecs - @start[tid]); delete(@start[tid]); }
```

---

## 8. Reading results

For each opcode/block under test: the differential (functionality 2) or bracketed (functionality 1) wall-clock time at the tier reached, the `gasUsed`, and the live gas parameter value behind it (Tier 0). Report as a ratio (time-per-gas-unit) per opcode/tx, not a single aggregate.

**Before concluding anything, re-check against §1:** a divergence on `SSTORE` confirms a known governance-set gap, not a finding; the real signal is `KECCAK256`/arithmetic. Precompile numbers belong in their own table, never blended with interpreted-opcode results. For functionality 1, a bimodal or inconsistent time distribution across repeated replays of the same `<X>` should raise the OCC-determinism assumption (§1) before any other explanation.

---

## 9. Failure modes (quick reference)

| Symptom | Cause | Fix |
|---|---|---|
| `kubectl describe node` shows an instance type outside `r6i`/`r7i` | `controller-config.yaml` or NodePool requirements drifted between harbor and prod | Halt — the timing result won't reflect prod hardware. Diff `clusters/{harbor,prod}/sei-k8s-controller/controller-config.yaml` and `clusters/{harbor,prod}/default/nodepool-*.yaml` before proceeding. |
| Can't get a clean read on block `<X>`'s gas/results | Tried to force the replayer to halt exactly at `<X>` instead of reading its result once the height passed | Let replay continue past `<X>`; query `<X>`'s results directly via RPC once the height has been applied (§3). |
| `eth_call` with a state override returns an error or silently ignores the override | Path A's RPC capability (§4) isn't actually supported on this `seid` version | Fall back to Path B — restore a private writable fork of state-at-`<Y>` and submit real signed txs. |
| Probe never fires / symbol not found | Assumed upstream go-ethereum's import path instead of resolving on the actual binary | Re-run the `go tool nm` resolution (§7) against `/proc/<seid-pid>/root/usr/bin/seid`. |
| Wall-clock measurements wildly inconsistent run-to-run | OCC re-execution (§1) — overlapping submissions (functionality 2) or parallel/re-executed scheduling on replay (functionality 1) | Functionality 2: strictly sequential one-tx-per-block. Functionality 1: render the replayer with `occ-enabled = false`, `concurrency-workers = 1` (§3a) for serial, deterministic, per-tx-correlatable application. |
| Functionality-1 per-tx timing doesn't line up with per-tx `gasUsed` | Bracketed a multi-tx block under OCC — got block-level aggregate, not per-tx (§1, §3a) | Disable OCC on the replayer (§3a) so ordinal-by-call-count maps to the Nth tx; if OCC can't be disabled, report block-level aggregate vs. block-total gas only. |
| Attaching the probe changes replay throughput (blocks/s) | High context-switch rate on a 40 blocks/s replayer makes `offcputime`/probe overhead non-negligible (§7, guardrail 3) | A/B throughput probe-on vs. probe-off; narrow the window, lower sampling rate, or serialize (§3a) until throughput is unperturbed. |
| Path A gives gas but no clean execution time | Tried to eBPF-attach the shared archive node (forbidden, §4/§6) | Run the state-overridden `eth_call` against your own private restore node (Path A(ii), §4), or fall back to Path B. |
| `seid` pod crashes after attaching a `uretprobe` | The Go-runtime/`uretprobe` interaction hazard (§7) | Expected risk on a throwaway node — redeploy and prefer `offcputime`/`profile` beyond a one-off rehearsal. |
| "Gas doesn't match compute" conclusion on `SSTORE` | Tested a governance-mutable param as if compute-derived (§1) | Reframe as confirming the known divergence; re-run on `KECCAK256`/arithmetic for the real test. |
| Can't get sign-off / told to stop before attaching anything privileged | §6's gate working as intended | Platform team's call — escalate, don't self-approve a scoped-down version of the same attach. |

---

## 10. Pre-flight checklist

- [ ] Confirmed the target node landed on `r6i`/`r7i` via `sei-node`/`sei-archive` (§2) — don't trust the default without checking.
- [ ] Recorded live gas parameter values + image digest (§7 Tier 0).
- [ ] **Functionality 1:** replayer node restoring to `<Y>` and replaying forward (§3); confirmed per-tx `gasUsed` for `<X>` is readable post-replay.
- [ ] **Functionality 1 (per-tx correlation):** if `<X>` has >1 tx, replayer rendered with `occ-enabled = false` + `concurrency-workers = 1` (§3a) so serial ordinal correlation holds — or the report is explicitly scoped to block-level aggregate timing. Probe attached *continuously across a span containing `<X>`* (window detection is post-hoc, not reactive — §3a), not started reactively on the height transition.
- [ ] **Functionality 2:** confirmed whether Path A's `eth_call` state-override capability actually exists on this `seid` version (§4) before committing to it; if not, Path B's private writable fork is staged instead.
- [ ] (Path B only) Differential bytecode pair built (§5), fixed high-enough iteration count, sequential one-tx-per-block submission harness logging `{submit_ts, tx_hash, block_height, gasUsed}`.
- [ ] Tier 0 and Tier 1 (pprof) tried and found insufficient before reaching for eBPF.
- [ ] **Sign-off obtained** for any privileged attach, capabilities scoped to `--target=seid` rather than `hostPID` (§6) — before running anything in §7 Tier 2.
- [ ] Symbols resolved against the actual running binary, not assumed from source alone (§7).

---

## References

- Sei-EVM gas/precompile specifics: `/evm` skill — `kit-evm-parity-gas.md`, `kit-sei-precompiles.md`; `x/evm/types/params.go` in `sei-chain` for the governance-mutable param set.
- eBPF method, overhead-bounding, Go-uprobe/uretprobe divergences: `/ebpf` skill, `references/pack-perf-methodology.md`.
- Harbor chain spin-up mechanics: `/harbor-dev` skill, `references/ephemeral-chain-flow.md`, `references/seictl-cli.md`, `references/harbor-cluster.md`.
- Replay mechanism (functionality 1): `validating-flatkv-memiavl-parity-via-sharded-replay.md` (this directory) — the `replayer.snapshot.s3.targetHeight` restore, the shadow comparator's layer definitions (gas yes, timing no).
- Archive-node provisioning: `operating-archive-node-byov.md` (this directory) — infra only; does not document RPC query semantics, hence §4's Path A verification requirement.
- Hardware-parity mechanism: `sei-k8s-controller` — `internal/platform/platform.go` (`NodepoolForMode`), `internal/task/bootstrap_resources.go`, `internal/noderesource/noderesource.go`; `sei-protocol/platform` — `clusters/{harbor,prod}/sei-k8s-controller/controller-config.yaml`, `clusters/{harbor,prod}/default/nodepool-{default,archive}.yaml`.
- Harbor `eng-<alias>` Pod Security / admission posture: `sei-protocol/platform` — `clusters/harbor/engineers/base/namespace.yaml`, `clusters/prod/walle/{namespace,podcomposition-policy}.yaml` (the posture to eventually mirror), `clusters/harbor/chaos-mesh/chaos-mesh.yaml`, `clusters/harbor/admission/tenant-hostname-policy.yaml`.
- Execution entry points: `sei-chain` — `x/evm/keeper/msg_server.go` (`applyEVMMessage`); the vendored go-ethereum fork's `core/state_transition.go` (`StateTransition.Execute`) and `core/vm/interpreter.go` (`EVMInterpreter.Run`) — resolve the fork version via `go.mod`, don't hardcode it here.
