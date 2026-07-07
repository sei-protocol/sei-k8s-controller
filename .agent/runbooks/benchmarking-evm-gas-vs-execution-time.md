# Benchmarking EVM Gas vs. Real Execution Time on a Personal Harbor Chain

**Audience:** engineers (and their agents) investigating whether Sei's EVM gas schedule actually tracks real compute cost — "does `gasUsed` correspond to wall-clock execution time, or has it drifted from what the interpreter actually does."
**Scope:** standing up a personal single-validator + RPC-follower chain in your `eng-<alias>` harbor namespace, crafting opcode-isolated bytecode, the privileged-attach gate for eBPF instrumentation, concrete probe design against real `seid`/go-ethereum symbols, the submission/correlation protocol, and the confounds that will produce a false result if ignored. **No sei-chain source changes anywhere in this procedure** — everything here is external client traffic + kernel/process-level observation of a binary you don't touch.
**Not in scope:** *why* Sei's gas schedule has the specific values it does (that's a governance/design question — if this benchmarking effort produces a case for changing a gas param, capture *that* as a `/design` doc, not here). Modifying gas costs, submitting the result on-chain, or running any of this against a shared/long-lived/production chain — this is a throwaway personal chain, full stop. Full onboarding to harbor (see the `/harbor-dev` skill and its `references/preflight.md`).

> **Read this whole section before running anything.** Three independent traps make the naive version of this experiment **silently wrong**: (1) at least one opcode's gas cost (`SSTORE`) is a **governance-mutable Sei chain param that already diverges from upstream go-ethereum** — using it as your "does gas match compute" baseline tests a value that was hand-tuned away from compute cost *by design*, not by accident; (2) per-opcode timing via a uprobe **is not achievable** on this binary — the interpreter's opcode dispatch is an indirect call with no per-opcode symbol, and Go's runtime makes `uretprobe`-based entry/return timing actively hazardous; (3) attaching any eBPF probe to a `seid` pod on harbor requires a **privileged kernel capability grant on a shared EKS cluster** — nothing in harbor's current `eng-<alias>` namespace configuration technically blocks this, but that is an unhardened gap, not a green light. §1, §2, and §4 exist to keep you out of these three holes.

---

## 0. Substitutions used throughout

| Placeholder | Meaning |
|---|---|
| `<alias>` | your harbor engineer alias; namespace is `eng-<alias>`. |
| `<chain-id>` | a fresh chain-id you choose for this experiment (see the `/harbor-dev` naming rule — tie it to a Linear ticket or your alias, never reuse). Also the `SeiNetwork` name. |
| `<rpc-node>` | `<chain-id>-rpc-0` — the single RPC follower (§3). |
| `<ECR>` / `<image-ref>` | the sei-chain image you're benchmarking; resolved per `references/image-resolution.md` in the `/harbor-dev` skill. |
| `<opcode>` | the single EVM opcode under test for a given run (§4). |
| `<seid-pid>` | the PID of the `seid` process inside `<rpc-node>`'s pod — resolve fresh each pod restart. |
| `<sei-chain-checkout>` | your local `sei-chain` clone, used to resolve real symbol names before writing any probe (§6). |

---

## 1. Mental model — what this can and cannot prove (read first)

The question is simple: for a given opcode, does the gas charged scale with the real wall-clock cost of executing it? The honest answer requires knowing, going in, which parts of the gas schedule were **derived from measured compute** and which were **set by other means** (governance, upstream inheritance, a flat precompile fee) — comparing against the latter and reporting "gas doesn't match compute" would be reporting a known, intentional divergence as if it were a finding.

**What's a fair target vs. a confound:**

| Category | Examples | Why |
|---|---|---|
| **Primary target — interpreter-local, no state backend** | `KECCAK256`, pure arithmetic (`ADD`/`MUL`/`PUSH`/`DUP`/…) | Executes entirely in the go-ethereum interpreter loop with no I/O; the cleanest "compute-bound opcode" available. |
| **Secondary target — real, but control for it** | `SLOAD`/`SSTORE` | **`SSTORE`'s set-gas is a governance-mutable Sei chain param (`SeiSstoreSetGasEIP2200`, `x/evm/types/params.go:43`) that already diverges from the upstream go-ethereum constant** — you are not testing "does gas match compute," you're testing "does gas match a number governance picked." If you test it anyway (worth doing as its own category), you must also control for IAVL-tree-depth / cache-warmth / cold-vs-warm-slot variance (Sei's state backend is memIAVL/flatKV, not an MPT) — separate cold-slot and warm-slot runs, and don't compare across them. |
| **Separate, expected-to-diverge category — do not average into "opcode gas"** | Sei precompiles (staking, distribution, oracle, etc. — see `/evm` skill's precompile kit for the current address table) | Precompile "execution" is a Go-native Cosmos-module call outside the interpreter entirely; its gas is a governance-tunable flat-ish fee structurally decoupled from whatever compute it triggers. Report it, but don't blend it with interpreted-opcode results. |

**Two more confounds that apply to every run, regardless of opcode:**

- **Per-tx fixed overhead** (signature verification, the Cosmos ante handler, the EVM↔Cosmos StateDB bridge, usei↔wei decimal conversion) is real wall-clock time attributable to *no* opcode. It doesn't disappear — it dilutes your measurement unless your loop count is high enough to make it a small fraction of total time.
- **Optimistic concurrent execution (OCC).** Sei's parallel tx execution can serialize and **re-execute** a transaction on hot-slot contention. A re-executed tx's wall-clock time is not a clean single-pass measurement. §5's one-tx-per-block sequential-submission protocol sidesteps this by construction — don't skip it to save time.

**Per-opcode granularity is achieved by construction, not by probing.** The interpreter's opcode dispatch (`(*EVMInterpreter).Run`, `core/vm/interpreter.go:168`) reads the opcode as a loop-local byte and calls it through an indirect function-table lookup (`operation.execute(...)` at interpreter.go:322, table lookup at :255) — there is no stable per-opcode symbol or argument to uprobe. Instead: craft bytecode dominated by one repeated opcode (§4), measure the *transaction's* wall-clock time, divide by iteration count. `per-tx time ÷ opcode count ≈ per-opcode time`. This is the disciplined substitute the `/ebpf` skill's overhead-bound and Go-uprobe-hazard rules point you toward — don't try to fight the interpreter's dispatch shape.

---

## 2. The privileged-attach gate — read before running `kubectl debug`

Any eBPF uprobe, `offcputime`, or CPU `profile` run against `seid` requires a **privileged kernel capability grant** (`CAP_BPF`/`CAP_SYS_ADMIN`/`CAP_PERFMON`) on a pod in **harbor — a shared EKS cluster** running other engineers' chains and the `sei-k8s-controller` itself. This is root-on-node. The `/ebpf` skill's guardrail is unconditional here: **the gate is on the attach mechanism, not on how low-stakes the target is.** A personal, zero-stake, single-validator throwaway chain does not waive it — get explicit human + security sign-off before attaching anything, even a one-off `bpftrace -e` line.

**What's actually true about `eng-<alias>` today (verified against the platform repo, not assumed):**

- `eng-<alias>` namespaces carry **no Pod Security Standard label** (`clusters/harbor/engineers/base/namespace.yaml`) and harbor has **no cluster-wide PSS default** either — so the effective PSS level is Kubernetes' own default, **`privileged`, enforced nowhere**. Nothing in-namespace blocks a privileged pod or a `kubectl debug --profile=sysadmin` ephemeral container today.
- Compare this to **prod**: `clusters/prod/walle/podcomposition-policy.yaml` explicitly blocks ephemeral containers as a `kubectl debug` escape vector, on top of a `restricted` PSS label. Harbor's `eng-<alias>` has no equivalent. **This is an unhardened gap, not a sanctioned path** — it's the kind of thing platform closes the moment someone notices, which would break this runbook retroactively if you built a habit of relying on it silently.
- **Shared-node blast radius is real independent of PSS.** Engineer workloads bin-pack onto harbor's untainted default Karpenter NodePool — a `hostPID` pod sees *every* process on that node, including other engineers' `seid` processes and the Cilium agent, not just your own.

**Do this, in order, before attaching anything:**

1. **Get sign-off first.** Surface the ask (what you're attaching, to what, on which pod, for how long) to the platform/security owner before running anything privileged. Don't proceed on "PSS doesn't block it" alone.
2. **Pin to a dedicated node.** Add a `nodeSelector`/toleration so `<rpc-node>`'s pod (and nothing else) lands on a node you're not sharing with other tenants, for the duration of the experiment. This contains the blast radius even though PSS wouldn't otherwise stop it.
3. **Scope the capability grant to the minimum, not `hostPID`.** Use `kubectl debug --target=seid -it <rpc-node>-0 --image=<bpftrace-image> --profile=sysadmin` — `--target=<container>` joins **only that pod's** PID namespace, not the node's. Where possible, request `CAP_BPF`+`CAP_PERFMON` rather than the broader `sysadmin` profile — a same-pod uprobe/off-CPU/profile target doesn't need full `CAP_SYS_ADMIN` or `hostPID`.
4. **If this becomes recurring or a second engineer needs it**, that's the signal to stop improvising per-run and instead open a platform PR for a proper dedicated profiling namespace with its own restricted-by-default PSS + a narrow, auditable exception — mirror `clusters/harbor/admission/tenant-hostname-policy.yaml`'s shape for the exception and `clusters/dev/sei-omnigent/sandbox-namespace.yaml` for the relaxed-but-still-restricted namespace pattern. `clusters/harbor/chaos-mesh/chaos-mesh.yaml` is harbor's existing precedent for an elevated-privilege, namespace-scoped, opt-in-annotation-gated workload — same shape, different capability. **This is a platform decision, not something to self-approve** — this runbook stops at "here's the gap and the mitigation," not "here's the policy."

---

## 3. Standing up the personal chain

You need **two nodes**, not one: a validator's EVM JSON-RPC surface is disabled (`ModeValidator disables EVM` — see the `/harbor-dev` skill's `references/harbor-cluster.md`), so you can't submit or query EVM transactions against the validator directly, even though it does execute every transaction for consensus. Stand up a single-validator genesis chain plus one RPC follower via the `/harbor-dev` skill's render→PR→Flux flow (`seictl network`/`seictl node` — see its `references/ephemeral-chain-flow.md` and `references/seictl-cli.md` for the full mechanics; only the commands unique to this use case are below):

```sh
# Single-validator genesis chain — --replicas 1 is the load-bearing flag (default is 4)
seictl network apply <chain-id> --preset genesis-chain \
  --chain-id <chain-id> --image <image-ref> --replicas 1 \
  -n eng-<alias> --dry-run

# One RPC follower, peered to the genesis chain — this is your submission + query + instrumentation target
seictl node apply <rpc-node> --preset rpc \
  --chain-id <chain-id> --network <chain-id> \
  -n eng-<alias> --dry-run
```

Render → PR against `harbor-engineering-workspace` → merge → `seictl network watch <chain-id> --until=Ready` → `seictl node watch <rpc-node> --until=Running` — this is the standard `/harbor-dev` flow, not repeated here.

**Instrument `<rpc-node>`, not the validator.** You're already pointing `seid tx evm ... --evm-rpc=<rpc-node's evmJsonRpc endpoint>` at it for submission and `eth_getTransactionReceipt` for `gasUsed` — keeping the probe target and the query target in the same pod means one `kubectl debug --target=seid` session covers everything, with no cross-node clock correlation to worry about. The validator executes the same state transition for consensus, but there's no operational reason to split targets.

---

## 4. Crafting opcode-isolated bytecode

**The differential-loop technique** isolates a single opcode's marginal cost without needing per-opcode instrumentation: write two bytecode variants that are identical except for the opcode under test.

- **Variant A (opcode-under-test):** a loop body of `JUMPDEST`, the target `<opcode>` (with whatever operands it needs pre-pushed), then the loop-decrement/`JUMPI` back to `JUMPDEST`.
- **Variant B (baseline):** identical loop scaffold, but the target opcode replaced with a cheap no-op pair (e.g. `PUSH1 0x00` / `POP`) of known, separately-measured cost.

`(wall-clock(A) − wall-clock(B)) ÷ iteration count ≈ per-opcode marginal wall-clock cost`, and the same subtraction on `gasUsed` gives you the marginal *gas* cost to compare it against. This cancels the loop-overhead opcodes (`PUSH`/`DUP`/`SUB`/`JUMPI`) and the per-tx fixed overhead (§1) from both sides symmetrically.

Deploy and invoke via the `seid` CLI directly against `<rpc-node>` (no sei-chain code involved — these are standard `seid tx evm` subcommands, see `seid tx evm --help` for the full flag set):

```sh
seid tx evm deploy /path/to/<opcode>-loop.bin \
  --from=<key> --chain-id=<chain-id> --evm-rpc=http://<rpc-node>.eng-<alias>.svc:8545 \
  --gas-limit=<large-enough-for-iteration-count> --gas-fee-cap=<cap>

seid tx evm call-contract <deployed-addr> <calldata-hex> \
  --from=<key> --value=0 --chain-id=<chain-id> --evm-rpc=http://<rpc-node>.eng-<alias>.svc:8545
```

Pick a large-but-fixed iteration count up front (large enough that fixed overhead is a small fraction of total gas/time; the EVM code-size cap bounds how much you can unroll instead of loop, so loop rather than unroll for counts beyond a few hundred). Run each variant multiple times and take the distribution, not a single sample — wall-clock has real variance from GC pauses, Pebble compaction, and scheduling noise that gas cost is blind to.

---

## 5. Submission + correlation protocol

**Submit exactly one transaction, wait for its block to finalize and its receipt to confirm, before submitting the next.** This is the single biggest simplification available to you: with sequential, no-overlap submission, a block's execution window contains exactly the transaction(s) you just sent, in an order you know. It also directly sidesteps the OCC re-execution confound from §1 — nothing is contending for the same storage slot across concurrent transactions if there's only ever one in flight.

Log client-side, per transaction: `{submit_ts, tx_hash, block_height, gasUsed (from the receipt)}`. Join this against whatever the probe emits (§6) by time-ordering — you do not need to decode transaction data inside the probe itself.

**One real gotcha:** `StateTransition.Execute` (the function you're timing, §6) runs more than once per submitted transaction — once during `eth_estimateGas`/`CheckTx` simulation, and again at actual `FinalizeBlock` delivery. Expect **2–3 invocations per tx**, not one. Disambiguate by taking the invocation whose timestamp falls inside the block-production window for the block your tx landed in (correlate against the block's timestamp from `/status` or the block header), or simply the *last* invocation observed before the receipt becomes queryable.

---

## 6. Instrumentation — cheapest and least-privileged first

Don't reach for eBPF as step one. Try these in order; stop at the first tier that gives you a usable signal.

**Tier 0 — free, no probe at all.** `gasUsed` from the receipt and `eth_estimateGas` are already there. Before instrumenting anything, record the live gas parameters and software version so the eventual comparison means something: confirm the value of any governance-mutable param you're testing against (e.g. `SeiSstoreSetGasEIP2200`, `x/evm/types/params.go:43`) via whatever query path `seid query evm --help` / `seid query params --help` currently exposes on the image you're running — don't assume a CLI shape that may have changed. Record the resolved image digest alongside it.

**Tier 1 — Cosmos SDK's own pprof, no elevated privileges.** The `rpc` preset's `spec.overrides`/`--set` surface can set `network.rpc.pprof_laddr` in the rendered `config.toml` (see the preset's example override in `/harbor-dev`'s `references/seictl-cli.md`). Port-forward and pull a CPU profile with plain `go tool pprof` — zero Kubernetes privilege needed beyond what you already have in your own namespace. **Verify empirically whether this surfaces EVM-interpreter frames at all** — `net/http/pprof` in this codebase is only explicitly imported by the `ethreplay` subcommand, not the running validator/full-node path, so whether `pprof_laddr` gives you a populated `net/http/pprof` mux at all (via a transitive cosmos-sdk import) needs to be confirmed against the actual running pod, not assumed from either agent's read of `seid`'s own `main.go`. If it's empty or unhelpful, move to Tier 2 — this was a cheap thing to rule out first.

**Tier 2 — eBPF (the gated tier; §2 must be cleared first).**

Resolve real symbols before writing any probe — do not assume upstream go-ethereum symbol names. `sei-chain`'s `go.mod` pulls go-ethereum via a `replace` directive to a Sei fork; **Go's `replace` preserves the original import path in compiled symbol names**, so the binary's symbols are still `github.com/ethereum/go-ethereum/...`, not a `sei-protocol/...` path. Confirm on the actual binary, not from source alone:

```sh
kubectl -n eng-<alias> debug --target=seid -it <rpc-node>-0 --image=<toolbox-with-go> --profile=sysadmin -- \
  go tool nm /proc/<seid-pid>/root/usr/bin/seid | grep -E 'StateTransition\)\.Execute|EVMInterpreter\)\.Run'
```

The two real boundaries (from `<sei-chain-checkout>`, confirm line numbers haven't drifted before citing them to anyone):

- **Per-tx:** `github.com/ethereum/go-ethereum/core.(*StateTransition).Execute` (`core/state_transition.go`) — Sei's `x/evm/keeper/msg_server.go` (`applyEVMMessage`) calls this directly; don't probe the upstream `ApplyMessage` wrapper, Sei bypasses it.
- **Per-opcode dispatch loop (not individually probeable — see §1):** `github.com/ethereum/go-ethereum/core/vm.(*EVMInterpreter).Run`.

**Prefer `offcputime` and a PID-filtered on-CPU `profile` over raw entry/return timing.** This is the actual highest-value signal for your question — whether `Execute`'s wall-clock time is CPU-bound (interpreter work, i.e. what gas is supposed to meter) or blocked on Pebble/state I/O (which gas categorically does not meter) is a more honest answer than a single duration number, and both are map-aggregated (near-zero overhead at the ~1 tx/block rate this protocol produces):

```
# on-CPU attribution, PID-filtered
bpftrace -e 'profile:hz:49 /pid == <seid-pid>/ { @[ustack] = count(); }'

# off-CPU (blocked) time, PID-filtered — needs the offcputime tool, not a bare bpftrace one-liner
offcputime -p <seid-pid> -f 30
```

**Raw per-invocation duration via `uprobe`/`uretprobe` is rehearsal-only, on this throwaway chain, with two hazards you must state plainly if you use it:** (1) Go's stack-copying runtime and `uretprobe`'s return-address rewrite can interact badly — this is a known divergence, not a hypothetical; (2) a goroutine can migrate OS threads mid-call on a blocking I/O read, which breaks a `tid`-keyed entry/return pairing even at concurrency 1. The `/ebpf` skill's "never on a validator/consensus-critical node" guardrail is weaker here specifically *because* this is a zero-stake personal chain where a crash costs you a re-deploy, not a slashing event — but that weakening applies only to the crash-tolerance judgment call, not to the §2 attach-privilege gate, which is unconditional.

```
uprobe:/proc/<seid-pid>/root/usr/bin/seid:"github.com/ethereum/go-ethereum/core.(*StateTransition).Execute"
{ @start[tid] = nsecs; }
uretprobe:/proc/<seid-pid>/root/usr/bin/seid:"github.com/ethereum/go-ethereum/core.(*StateTransition).Execute"
/@start[tid]/
{ @dur = hist(nsecs - @start[tid]); delete(@start[tid]); }
```

---

## 7. Reading results

For each opcode under test, you should have: the differential wall-clock time (§4) at the tier-2 granularity you reached, the differential `gasUsed`, and the live gas parameter value that produced it (§6 tier 0). Report them as a ratio (time-per-gas-unit) per opcode, not a single aggregate — the whole point is to see *which* opcodes diverge, and by how much, not to produce one number for "the EVM."

**Before concluding anything, re-check against §1:** if the opcode you measured is `SSTORE`, you already know the gas side is a governance-set number, not a compute-derived one — a divergence there confirms a known fact, it isn't a finding. A divergence on `KECCAK256` or pure arithmetic is the genuine signal this experiment exists to find. A precompile's numbers belong in their own table, never blended with interpreted-opcode results.

---

## 8. Failure modes (quick reference)

| Symptom | Cause | Fix |
|---|---|---|
| `seid tx evm` calls succeed but you can't read `gasUsed` / can't reach `--evm-rpc` | pointed at the validator, not the RPC follower | Validators don't serve EVM JSON-RPC (§3); target `<rpc-node>`'s `evmJsonRpc` endpoint. |
| Probe never fires / symbol not found | assumed upstream go-ethereum's import path instead of resolving on the actual binary | Re-run the `go tool nm` resolution in §6 against `/proc/<seid-pid>/root/usr/bin/seid` — the `replace` directive keeps the upstream path, but don't take that on faith either. |
| Wall-clock measurements wildly inconsistent run-to-run | overlapping submissions triggering OCC re-execution (§1), or measuring a re-execution as if it were a clean single pass | Confirm strictly sequential one-tx-per-block submission (§5); check for more than one `Execute` invocation per tx and disambiguate by block-production window. |
| `seid` pod crashes or misbehaves after attaching a `uretprobe` | the Go-runtime/`uretprobe` interaction hazard (§6) | Expected risk on this tier — redeploy the personal chain (it's throwaway) and prefer `offcputime`/`profile` for anything beyond a one-off rehearsal. |
| "Gas doesn't match compute" conclusion on `SSTORE` | tested a governance-mutable param as if it were compute-derived (§1) | Reframe as "confirms the known SSTORE gas/upstream divergence," not a new finding; re-run on `KECCAK256`/arithmetic for the real test. |
| Can't get sign-off / told to stop before attaching anything privileged | §2's gate working as intended | This is the platform team's call, not something to route around — escalate the ask, don't self-approve a scoped-down version of the same attach. |

---

## 9. Pre-flight checklist

- [ ] Personal single-validator + RPC-follower chain up via `/harbor-dev` (§3); confirmed you're pointed at `<rpc-node>`, not the validator, for submission/query.
- [ ] Recorded the live gas parameter values + image digest you're testing against (§6 tier 0) — the comparison is meaningless without this.
- [ ] Differential bytecode pair built for the target opcode (§4), with a fixed, high-enough iteration count.
- [ ] Sequential one-tx-per-block submission harness ready, logging `{submit_ts, tx_hash, block_height, gasUsed}` (§5).
- [ ] Tier 0 and Tier 1 (pprof) tried and found insufficient before reaching for eBPF.
- [ ] **Sign-off obtained** for any privileged attach, workload pinned to a dedicated node, capabilities scoped to `--target=seid` rather than `hostPID` (§2) — before running anything in §6 tier 2.
- [ ] Symbols resolved against the actual running binary, not assumed from source reading alone (§6).

---

## References

- Sei-EVM gas/precompile specifics: `/evm` skill, `references/sei-evm-profile.md`, `kit-evm-parity-gas.md`, `kit-sei-precompiles.md`; `x/evm/types/params.go` in `sei-chain` for the governance-mutable param set.
- eBPF method, overhead-bounding, and the Go-uprobe/uretprobe divergences: `/ebpf` skill, `references/pack-perf-methodology.md`.
- Harbor chain spin-up mechanics: `/harbor-dev` skill, `references/ephemeral-chain-flow.md`, `references/seictl-cli.md`, `references/harbor-cluster.md`.
- Harbor `eng-<alias>` Pod Security / admission posture: `sei-protocol/platform` — `clusters/harbor/engineers/base/namespace.yaml`, `clusters/prod/walle/{namespace,podcomposition-policy}.yaml` (the posture to eventually mirror), `clusters/dev/sei-omnigent/sandbox-namespace.yaml`, `clusters/harbor/chaos-mesh/chaos-mesh.yaml`, `clusters/harbor/admission/tenant-hostname-policy.yaml`.
- Execution entry points: `sei-chain` — `x/evm/keeper/msg_server.go` (`applyEVMMessage`, `EVMTransaction`); the vendored go-ethereum fork's `core/state_transition.go` (`StateTransition.Execute`) and `core/vm/interpreter.go` (`EVMInterpreter.Run`) — resolve the fork version via `go.mod`, don't hardcode it here.
