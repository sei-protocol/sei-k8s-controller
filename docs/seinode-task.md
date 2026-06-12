# SeiNodeTask

A `SeiNodeTask` is a **one-shot operation against a single `SeiNode`**, driven to a
terminal state by the `nodetask` controller. This is the operator/automation
reference; field-level contracts live in the godoc (`api/v1alpha1/seinodetask_types.go`)
and the design record in the LLD (PR #277). Field definitions are **not** restated
here — only the operational behavior that the types don't tell you.

> Anchors below are a cited contract for the gov-ops skill (PLT-489) — **renaming a
> heading is a breaking change** for its references.

## Kinds

One line per kind; the **behavior note** is the part not obvious from the payload.
`spec.kind` is **immutable**, and **exactly one** payload sub-spec must match it
(admission-rejected otherwise).

| kind | does | behavior note (not in the payload) |
|---|---|---|
| `GovVote` | `MsgVote` on a proposal | chain-idempotent (last-write-wins per voter) — re-apply is safe |
| `GovSoftwareUpgrade` | submits a software-upgrade proposal | content hardcoded to `SoftwareUpgradeProposal`; **not** chain-idempotent |
| `GovParamChange` *(planned, PLT-487)* | submits a `ParameterChangeProposal` | `value` is JSON of the param's type (see [Gotchas](#reconciliation-cadence--gotchas)); **not** idempotent |
| `AwaitCondition` | waits on a local node condition (height) | `action: SIGTERM_SEID` SIGTERMs seid after the condition — the coordinated halt-at-height primitive |
| `AwaitNodesAtHeight` | waits for the target to cross a height | **single-node** (`target.nodeRef`); maps to the sidecar `await-condition(height=H)` and **drops `action`** |
| `UpdateNodeImage` | patches `spec.image`, waits for `currentImage` | **no readiness check** — completes on image observation; green ≠ healthy node |
| `RestartSeid` | restarts seid in place | SIGTERMs seid so it re-reads `config.toml` **without bouncing the sidecar** (no mark-ready reapproval gap); empty payload; completion = local RPC serving again, **not** caught-up/voting — gate height with a following `AwaitNodesAtHeight`. Supersedes `RestartPod` |
| `MarkReady` | re-marks sidecar readiness | fire-and-forget; re-asserts `/v0/healthz=200` to unblock the seid start-gate / proxy probe after a readiness-blind restart or rollout; empty payload; completion = the submit ack (a beat before `/v0/healthz` serves 200) — gate real serving with a following `AwaitCondition`/`AwaitNodesAtHeight` |

## Lifecycle

Phases: `Pending → Running → Complete | Failed`. The task ID is **UUIDv5 of
`(CR UID, spec.kind, 0)`** — so **re-applying the same CR re-joins** the in-flight
task (never re-submits), and **delete+recreate mints a new ID** (a new run). `status.task.id`
is stamped **atomically before any side effect**; terminal CRs are **no-op reconciled**.

- `target.requirePhase` (default `Running`) / `requirePhaseTimeout` (default `5m`) gate
  dispatch; **the timeout is terminal** — a target that never reaches the phase fails the
  task (delete+recreate to retry), it does not wait forever.
- `timeoutSeconds` bounds the run from `status.task.executionStartedAt` (the requirePhase wait is **not** charged). `0` means the per-kind default: unbounded for most kinds, but **`RestartSeid` 10m / `MarkReady` 2m**.
- Status writes use the single optimistic-lock patch model (see `CLAUDE.md`).

## Conditions

`Ready`/`Failed` are a **latch pair** (the documented `kubectl wait` exception):
`Ready=True` only at `Complete`, `Failed=True` only at `Failed`. Treat `reason` as the
stable public API for runbooks/alerting:

- `TargetReady`: `PhaseMet` / `PhaseNotMet`
- `Failed`: `TargetResolveFailed` / `ParamsBuildFailed` / `UnsupportedKind` / `DeserializeFailed` / `TaskTerminalError` / `TaskFailed` / `Timeout`

All condition writes route through `setCondition`, which stamps
`observedGeneration = cr.Generation` — so a condition reliably reflects the spec generation
it was evaluated against.

## Signing topology

The operator keyring is **sidecar-resident** (mounted on `sei-sidecar`, not signable from
the main `seid` container). `keyName` resolves in two layers: the params builder
(`resolveSigningUID`) takes an explicit `keyName` when set; otherwise it derives via
**`ResolveOperatorKeyringUID`** (`api/v1alpha1/validator_types.go`):

1. explicit `keyName` → that key (`resolveSigningUID`);
2. `.validator.operatorKeyring.secret` set, `keyName` empty → **`node_admin`**;
3. `.secret` **unset** → **`validator`** (the gentx genesis-ceremony key).

A gentx-bootstrapped validator that assumes `node_admin` signs with the wrong key.
Out-of-band proposal *submission* needs a sidecar-resident or separately-funded key for
the same reason — `seid` in the main container has no operator key.

## Idempotency

Two layers, distinct:

- **Chain-level:** `GovVote` is last-write-wins (re-apply safe); `MsgSubmitProposal`
  (`GovSoftwareUpgrade`/`GovParamChange`) is **not** idempotent — a duplicate submit is a
  second proposal + second deposit.
- **Controller-level (at-least-once):** the deterministic ID re-joins on reconciler
  restart, and a `submittedAt` stamp guards `Execute` to run **once**. So the submit-proposal
  rehydration risk is a **controller-restart-after-submit** window, not a re-apply window.

## status.outputs

`status.outputs` is **unpopulated for all sidecar-backed kinds** (`GovVote`,
`GovSoftwareUpgrade`, `AwaitCondition`, `AwaitNodesAtHeight`) — **only**
`UpdateNodeImage.appliedImage` is set. The typed output godoc (`GovVoteOutputs.txHash`,
etc.) is forward-compatible shape that is **currently never written** — read the **chain**
for a tx hash or proposal id, not `status`. (This overrides the godoc, which reads as if
outputs are populated.)

## GitOps patterns

Tasks are ordinary manifests: wire them into a cluster's Flux path and `flux reconcile`.
Notes: the runtime `proposalId` of a fresh proposal isn't known until submission, so a
vote manifest's `proposalId` is filled post-submit; after votes complete, the one-shot CRs
sit terminal (prune via a follow-up PR); fan out **per cluster** (a `nodeRef` only resolves
in its own cluster).

## Reconciliation cadence & gotchas

Cadence (the latency budget the voting-window gotcha spends): the controller **Watches**
(not Owns) SeiNode, so a target status change wakes the task; steady poll **15s**,
target-wait **5s**, sidecar HTTP timeouts **30s/15s**.

Gotchas (invariant first; the canonical home — also restated at point of use):

1. **Fees must clear the chain-enforced min-gas-price**, which CheckTx enforces
   independent of the validator's local `app.toml`. A too-low fee is a **silent CheckTx
   code-13 retry-loop**, not a visible rejection — the tally never moves. *(Worked
   instance: arctic-1 enforces `0.02usei/gas`, not the `0.01` in app.toml — size
   `fees ≥ gas × 0.02usei`.)*
2. **A param-change `value` is JSON of the param's registered type** — a scalar (`100`),
   string (`"86400000000000"`), bool, or object — **not** a re-escaped string. A quoted
   string double-encodes and **fails at apply** (deposit consumed). *(Pending PLT-487 for
   the CRD path; applies to the out-of-band `seid tx` path today.)*
3. **Governance `voting_period` vs the cadence above** — arctic-1's 5-minute window is
   tight against merge→reconcile→broadcast; size fees right (gotcha 1) and apply promptly,
   or raise `voting_period` first.

## Example — vote fan-out

Cast a yes-vote from a validator (`keyName` omitted → resolves `node_admin`). `fees` clears
the chain floor (gotcha 1); `proposalId` is filled after the proposal is submitted.

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodeTask
metadata:
  name: govvote-prop42-validator-0
  namespace: arctic-1
spec:
  kind: GovVote
  timeoutSeconds: 600
  target:
    nodeRef:
      name: validator-0-0        # the SeiNode, not the SeiNodeDeployment or the pod
    requirePhase: Running
    requirePhaseTimeout: 30m      # ride through a transient validator restart
  govVote:
    chainId: arctic-1
    proposalId: 42                # filled post-submission
    option: "yes"                 # quote it — bare yes is YAML true
    fees: "8000usei"              # >= gas * chain min-gas-price (0.02usei/gas on arctic-1)
    gas: 300000
```

Generate one per validator from the live list per cluster, wire into that cluster's Flux
path, and `flux reconcile`.
