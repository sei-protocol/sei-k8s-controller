# Agent Runbooks

On-demand procedural and reference context for AI coding agents working on this repo, sister repos (e.g. `sei-protocol/platform`), or related Sei infrastructure.

Distinct from:

- **`CLAUDE.md`** (repo root) — *always-loaded* project conventions and architecture for Claude Code sessions
- **`AGENTS.md`** (if/when added) — same idea, cross-tool format
- **`docs/`** — human-oriented design documents, LLDs, RFCs, post-mortems

This directory is **on-demand**: an agent loads a runbook only when the operator points it here ("check `.agent/runbooks/<file>` for context") or when it's solving a problem the runbook covers. Keeping these out of `CLAUDE.md` saves context-window tokens during routine work and keeps high-volume reference material from crowding always-loaded instructions.

## Convention

- **Flat structure** — one file per procedure or reference. No subdirectories per role; if a runbook is role-specific, the role gets called out in the file's header.
- **Self-contained** — each runbook should make sense without prior session context. Operator and agent both come in cold.
- **Title + one-line summary up top** — so an agent fetching the directory listing can pick the relevant file.
- **File names are descriptive** — `<verb>-<topic>-<modifier>.md` reads well in a list (`operating-archive-node-byov.md`, `cutover-pacific-1-archive-volume.md`, `bootstrap-fork-genesis-pod.md`).

## Loading from outside this repo

Any agent in any repo can be told:

> *"For procedural context on Sei infrastructure operations, list `.agent/runbooks/` in `sei-protocol/sei-k8s-controller` (URL: https://github.com/sei-protocol/sei-k8s-controller/tree/main/.agent/runbooks/) and load whichever runbook matches the task."*

Agents fetch this `README.md` to discover what's available, then `WebFetch` the specific file.

## Index

| File | Summary | When to load |
|---|---|---|
| [`operating-archive-node-byov.md`](operating-archive-node-byov.md) | Required volume contents, PV/PVC spec, SeiNode/SeiNetwork spec, controller validation surface, and EBS-swap cutover sequence for archive nodes using the bring-your-own-volume (`dataVolume.import`) path. | Bringing up an archive node from a pre-populated EBS; swapping the underlying volume of an existing archive PV; debugging `ImportPVCReady=False`; receipt-store pruning concerns. |
| [`migrating-validator-to-byo-secrets.md`](migrating-validator-to-byo-secrets.md) | Cutting a live validator from a legacy host onto the platform carrying its consensus identity via Secrets (`signingKey`/`nodeKey`): what migrates, SeiNetwork spec, controller validation surface, the stop-before-start double-sign discipline + layered equivocation defenses, cutover/rollback sequence, and dry-run gotchas. | Migrating an existing validator (e.g. arctic-1 node-19) off EC2 onto K8s; any cutover where a consensus key changes hosts; understanding the `replicas:1` CEL guard or the double-sign alerts. |
| [`validating-flatkv-memiavl-parity-via-sharded-replay.md`](validating-flatkv-memiavl-parity-via-sharded-replay.md) | flatKV↔memIAVL storage-engine parity validation by differential historical replay: the flatKV+memIAVL replay-pair topology (same binary, same snapshot, blocks from a shared archive), the correctness gates (compare pair-not-archive; verify migration complete so flatKV reads are genuine; `historical_replay` build for pre-v6.5 txs), the seictl shadow comparator, result aggregation + Notion report, and the fan-out to 50+ shards. | Standing up a flatKV-vs-memIAVL correctness validation on harbor; driving a sharded replay campaign; debugging why a replay node is stuck or a comparison reads all-indeterminate/vacuous; understanding `migrate_evm` vs `memiavl_only`/`evm_migrated`/`flatkv_only` read routing. |
| [`benchmarking-evm-gas-vs-execution-time.md`](benchmarking-evm-gas-vs-execution-time.md) | Testing whether Sei's EVM gas schedule tracks real compute, for both (1) replaying a real historical block against restored state and (2) executing arbitrary bytecode against a specific historical state: why hardware parity with prod validators is automatic (`NodepoolForMode`, no engineer config needed), the replayer-node mechanism for functionality 1 and its real gap (gas is captured, execution time is not), the unverified `eth_call`-state-override dependency for functionality 2 and its private-writable-fork fallback, the confounds that invalidate a naive comparison (governance-mutable `SSTORE` gas, precompiles, OCC re-execution/determinism), the differential-loop bytecode-crafting technique for per-opcode isolation, the privileged-attach gate for eBPF on harbor's `eng-<alias>` namespaces (PSS posture, scoped `kubectl debug --target=`), and concrete probe design against real `seid`/go-ethereum symbols (why per-opcode uprobing doesn't work, why `offcputime`/`profile` beat raw uprobe timing). | Designing a gas-vs-compute benchmark; replaying a historical block for timing rather than storage-parity; deciding whether an opcode's gas cost is a fair compute proxy before trusting a result; attaching any eBPF probe to a harbor pod; resolving `seid`'s real symbol names before writing a probe. |

## Adding a new runbook

1. Create `<topic>.md` in this directory. Lead with title + one-paragraph scope statement (audience, what's in scope, what's not — link out to design docs in `docs/` for *why*).
2. Add a row to the **Index** table above with file, summary, and when-to-load triggers.
3. Open a PR. Reviewers check that the runbook is self-contained and that the index entry is accurate.

## What does *not* belong here

- *Why* design decisions were made — that's `docs/design-*.md` (LLDs, ADRs, RFCs)
- Always-loaded project conventions — that's `CLAUDE.md` / `AGENTS.md`
- Invokable slash-command procedures — that's `.claude/skills/<name>/SKILL.md`

If a doc grows beyond procedural reference into a multi-section design rationale, move it to `docs/` and leave a one-line pointer here.
