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
| [`operating-archive-node-byov.md`](operating-archive-node-byov.md) | Required volume contents, PV/PVC spec, SeiNode/SeiNodeDeployment spec, controller validation surface, and EBS-swap cutover sequence for archive nodes using the bring-your-own-volume (`dataVolume.import`) path. | Bringing up an archive node from a pre-populated EBS; swapping the underlying volume of an existing archive PV; debugging `ImportPVCReady=False`; receipt-store pruning concerns. |
| [`migrating-validator-to-byo-secrets.md`](migrating-validator-to-byo-secrets.md) | Cutting a live validator from a legacy host onto the platform carrying its consensus identity via Secrets (`signingKey`/`nodeKey`): what migrates, SND spec, controller validation surface, the stop-before-start double-sign discipline + layered equivocation defenses, cutover/rollback sequence, and dry-run gotchas. | Migrating an existing validator (e.g. arctic-1 node-19) off EC2 onto K8s; any cutover where a consensus key changes hosts; understanding the `replicas:1` CEL guard or the double-sign alerts. |

## Adding a new runbook

1. Create `<topic>.md` in this directory. Lead with title + one-paragraph scope statement (audience, what's in scope, what's not — link out to design docs in `docs/` for *why*).
2. Add a row to the **Index** table above with file, summary, and when-to-load triggers.
3. Open a PR. Reviewers check that the runbook is self-contained and that the index entry is accurate.

## What does *not* belong here

- *Why* design decisions were made — that's `docs/design-*.md` (LLDs, ADRs, RFCs)
- Always-loaded project conventions — that's `CLAUDE.md` / `AGENTS.md`
- Invokable slash-command procedures — that's `.claude/skills/<name>/SKILL.md`

If a doc grows beyond procedural reference into a multi-section design rationale, move it to `docs/` and leave a one-line pointer here.
