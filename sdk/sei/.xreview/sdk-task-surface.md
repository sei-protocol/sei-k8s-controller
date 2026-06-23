# xreview ledger — SDK SeiNodeTask surface (WS-G)

Class: component (public SDK surface over the SeiNodeTask CRD)
Tier: T2

Target: `sdk/sei/task.go`, `sdk/sei/provider.go`, `sdk/sei/provider/k8s/{render,handle,k8s}.go`, stubs + tests
Artifact: branch `feat/sdk-task-surface` (diff /tmp/wsg-task-surface.diff)

## Round 1

State: RESOLVED
OpenFindings: 0
Convergence: independent (4 blinded reviewers)
Blinded: yes
Dissenter: sei-network-specialist (DISSENT → resolved)

Slate: kubernetes-specialist (CRD-contract), idiomatic-reviewer (Go idiom), systems-engineer (poll/error contract), sei-network-specialist (dissenter, upgrade semantics).

### Boundary table

| Boundary | Provider | Consumer | Status | Evidence | Raised by |
|---|---|---|---|---|---|
| GovSoftwareUpgrade proposal-ID handoff | nodetask controller | harness (GovVote input) | **MISMATCH → FIXED** | `controller.go:360-369` populateOutputs only handles UpdateNodeImage; gov/await outputs never written (chain-as-medium by design). SDK advertised `ProposalID` as "the GovVote input" → always 0 → GovVote.proposalId Minimum=1 admission reject (`seinodetask_types.go:366`). | dissenter (lead), k8s, systems |
| UpdateNodeImage RequirePhase on halted node | nodetask controller | harness (step 4) | **MISMATCH → FIXED** | Gate is `==` exact-match defaulting Running (`controller.go:195-199`); a node halted at upgrade height still reports Running (phase sticky). SDK doc+test told callers to relax to Pending → terminal timeout. | dissenter |
| Payload field mapping (4 kinds) | CRD | renderTask | COMPATIBLE | All fields field-for-field congruent (render.go). | k8s |
| SSA / status subresource | CRD | k8s apply | COMPATIBLE | Main-resource Apply; status subresource separate — no status stomp. | k8s |
| WaitComplete poll loop | — | harness | **MISMATCH → FIXED** | Only NotFound tolerated; a transient Get error aborts a multi-minute wait. Tolerate retryable (ServerTimeout/TooManyRequests/InternalError). | systems |
| Complete + nil outputs | controller | WaitComplete | MISMATCH → MITIGATED | `(nil,nil)` nil-deref hazard; largely mooted by removing the unpopulated gov output types. | systems, k8s |
| Resubmit / idempotency | CRD + controller | RunTask | MISSING → DOCUMENTED | same-name re-apply is a no-op (no double-submit); delete+recreate resubmits the gov-tx. Doc'd on RunTask. CEL immutability + on-chain dedup are later coordinated CRD work. | systems |
| GovVote per-validator key derivation | controller | harness | COMPATIBLE | `KeyName:""` derives per-target SeiNode key (`seinodetask_params.go:120,231`); no shared-key assumption. | dissenter |

### Idiom addendum (idiomatic-reviewer — RATIFY)
Clean. No correctness/divergence-with-consequence findings. Endorsed `WaitComplete (*TaskOutputs, error)` as the correct one-shot-terminal shape. Two pure-style notes accepted as-is. Process note: add a provider-side one-output-per-Kind test.

### Resolutions (this PR, no controller change)
1. **ProposalID lie:** removed `GovSoftwareUpgradeOutputs`/`GovVoteOutputs`/`AwaitNodesAtHeightOutputs` from the SDK (all structurally unpopulated); `TaskOutputs` now carries only `UpdateNodeImage` (the sole kind populateOutputs writes). Package + payload docs rewritten to the chain-as-medium reality.
2. **RequirePhase backwards:** UpdateNodeImage doc fixed — a halted node still reports Running, so the default is correct; removed the relax-to-Pending guidance; repurposed the test to verify mechanical RequirePhase override (not the upgrade-failure pattern).
3. **Transient-error tolerance:** WaitComplete keeps polling on retryable Get errors.
4. **Cheap hardening:** validateTaskSpec now validates GovVote.Option enum + rejects 0<Timeout<1s (silent-unbounded trap).
5. **Docs:** RunTask resubmit/idempotency contract; Namespace/Node co-location; AwaitNodesAtHeight H-vs-H-1 + ctx-bound; InitialDeposit≥min_deposit + voting-period<upgradeHeight traps.
6. taskFailureReason: append phase to the no-reason fallback; prefer the Ready condition.

### Deferred (un-defer conditions noted; not blocking)
- CEL payload-immutability guard + on-chain upgrade-proposal dedup — coordinated CRD work; un-defer before driving a non-disposable chain.
- Sidecar typed TaskResult → re-add gov output fields when it lands.
- RestartSeid kind — when a release suite needs a config-only restart.
