# xreview ledger — TestChainUpgrade (WS-I Step 5)

Class: component (integration suite consuming the SDK + harness)
Tier: T2

Target: `test/integration/upgrade_test.go`
Artifact: branch `feat/test-chain-upgrade`

## Round 1

State: RESOLVED
OpenFindings: 0
Convergence: independent (4 blinded reviewers)
Blinded: yes
Dissenter: sei-network-specialist (DISSENT → resolved)

Slate: sei-network-specialist (dissenter, upgrade mechanics), systems-engineer (concurrency/poll/error), kubernetes-specialist (re-apply/targeting), idiomatic-reviewer.

### Boundary table

| Boundary | Status | Evidence | Raised by | Resolution |
|---|---|---|---|---|
| Halt detection (step 5) | **MISMATCH → FIXED** | Scenario uses a fixed wait, NOT a poll — validators halt together and stop serving RPC exactly when the predicate is true (major-upgrade.yaml:380-392); aggregate VIP drops NotReady backends at halt → black hole. My pollHeightAtLeast(aggregate, upgradeHeight-1) hangs/flakes. | dissenter | Poll aggregate only to a PRE-halt height (upgradeHeight-haltMargin, endpoint still alive), then a bounded settle for the halt; bump after. |
| Proposal-resolve JSON shape | **MISMATCH → FIXED** | Scenario jq matches content.plan.name OR messages[].content.plan.name (legacy+v1, :177-200); my struct only decodes content → hangs on gov v1. | dissenter | govProposal decodes both arms; matcher checks both. |
| Recovery gate soundness | **MISMATCH → FIXED** | waitValidatorsReady = PodReady ≠ rejoined-consensus-on-new-binary (brief-Ready-then-CrashLoop slips through). | k8s, dissenter | Replaced with RunTask(AwaitNodesAtHeight{upgradeHeight+postDelta}) per validator (port-faithful to scenario step 9, semantic, targets by NodeRef — no pod-label dependence) + a definitive /cosmos/upgrade applied-plan assertion. |
| False-pass on fast blocks | **MISSING → FIXED** | No assertion the upgrade actually executed; fast blocks → chain passes upgradeHeight while voting → no plan → no halt → WaitHeightAdvances greens a never-upgraded chain. | dissenter | Added waitUpgradeApplied (/cosmos/upgrade/v1beta1/applied_plan/{name} height>0) — proves the handler ran. |
| Enveloped-only /status decode | **MISMATCH → FIXED** | pollHeight re-models /status wrapped-only; the Sei fork sometimes answers unwrapped (SDK latestHeight handles both) → spins forever on such a node. | idiom | Promote SDK latestHeight → exported sei.LatestHeight (dual-shape); suite consumes it. |
| Diagnosability of timeouts | **MISMATCH → FIXED** | Poll helpers swallow last-seen height/status into bare deadline errors — below the WaitHeightAdvances bar; suite is unattended-nightly. | systems | pollREST threads a last-seen string into the deadline error; height polls use LatestHeight's value. |
| Task GC label | **MISSING → FIXED** | Task CRs carry no sei.io/harness-run label → leak on abnormal exit. | k8s | Added SDK TaskSpec.Labels (mirrors NetworkSpec.Labels); suite stamps runLabelKey. |
| Vote error fan-in | flag → FIXED | Only first-in-slice error surfaced. | systems, idiom | errors.Join across all validators. |
| NetworkSpec drift (provision vs bump) | flag → FIXED | Two hand-duplicated literals; a future field added to one strips it on the other via ForceOwnership. | systems, k8s | Single networkSpec builder; bump mutates only Image. |
| Image bump full-spec SSA re-apply | COMPATIBLE | k8s read the controller: it never writes the parent spec (finalizer + status only); genesis re-stamp idempotent (ceremony latched, nodes.go:91); no replica churn; no SSA conflict. Verified equivalent to `patch spec.image`. | k8s (refutes dissenter concern) | Kept; builder shared. |
| Concurrency / leaks / SIGTERM | COMPATIBLE | Race-free vote fan-out, body closed, ctx nesting + NotifyContext match sibling suites. | systems | — |
| Validator naming / namespace co-location | COMPATIBLE | <chain>-<ordinal> 0-based matches controller labels.go; task/target/pods co-located. | k8s | — |

### Idiom addendum (RATIFY)
Reads native (env+spec idiom, helpers, comment register, build-tag). Divergence-with-consequence = the /status + poll duplication of the SDK (resolved by exporting LatestHeight; gov-REST polling stays harness-local as gov-query orchestration, not readiness — matches the scope rule).

### Deferred (not blocking)
- Parent 60m can fire mid-child-step → misattributed error (systems): generous envelope; un-defer on first spurious occurrence.
- min_deposit / deposit-period hang (dissenter): params match the proven scenario (20000000usei clears min_deposit); the last-seen diagnostic surfaces a stuck deposit-period proposal if it ever regresses.
