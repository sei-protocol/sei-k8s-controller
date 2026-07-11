# xreview: retroactive review of injectDeadline (renamed injectWindow)

Class: mechanical
Tier: T2 (operator-requested full "team" review of code that shipped via an
unauthorized, never-reviewed merge — PR #474 on sei-k8s-controller, part of
this session's rogue-agent incident. Kept on technical merits per operator
direction rather than reverted; this round gives it the real review it never
got.)

Target: `injectDeadline` const + its use in `gateInjected` (`chaos_test.go`),
as introduced by PR #474 and still live in `main` after PR #477 (#477 removed
the unrelated `quarantine` mechanism from the same PR but didn't touch this).

## Round 1

State: RESOLVED
OpenFindings: 0
Convergence: independent, non-unanimous on severity — all three reviewers
agreed the core mechanism (bound fault-injection wait independently of the
40m scenario timeout) is sound and worth keeping; two of three (systems-engineer,
kubernetes-specialist) independently converged on the same overclaim in the
comments and a real numeric/timing problem, from different angles.
Blinded: yes — three reviewers dispatched independently against the same
on-disk artifact, no reviewer saw another's output before committing findings.
Dissenter: systems-engineer (assigned red-team pass — RATIFY-with-findings,
i.e. didn't conclude "revert," found two concrete, evidenced problems)

### Findings and resolution

| # | Finding | Raised by | Verdict | Resolution |
|---|---|---|---|---|
| 1 | `injectDeadline` (5m) exceeds default `CHAOS_DURATION` (3m) — a slow-but-successful injection could return after the fault already self-expired, turning the under-fault liveness check into a false green. Not enforced or documented. | systems-engineer | MISMATCH (latent correctness) | Added `injectWindow >= faultDur` guard at the top of `TestChaosSuite` (chaossuite_test.go) — fails loud immediately rather than silently producing a false green. Renamed the const `injectWindow` and lowered it to 90s (matching sibling const `oneShotObserveWindow`'s value/style), leaving 90s of margin below the 3m default. |
| 2 | The "stuck, not slow" framing is overclaimed: chaos-mesh's own retry backoff plus a cold-starting daemon on a freshly-scaled node can legitimately exceed the window without being stuck. Independently, the actual timeout message doesn't capture chaos-daemon apply errors (only CR-read API errors land in `lastErr`), so on the exact "stuck daemon" case named in the comment, the message is no more diagnostic than before. | systems-engineer + kubernetes-specialist (independent convergence) | MISMATCH (comment overclaim) | Reworded `injectWindow`'s doc comment to state plainly it's "a blast-radius bound, not a stuck-vs-slow classifier." Did not implement the per-record-phase diagnostic both reviewers characterized as a "preferred enhancement, not a blocker" — would require guessing at an unverified chaos-mesh CR schema field name (this module deliberately keeps the chaos-mesh API out of its deps, working unstructured-only), out of scope for this pass. Left as a documented future improvement if real false-positives are observed, matching this repo's existing deferred-scenario idiom. |
| 3 | Inline comment in `gateInjected` duplicates the const's doc comment near-verbatim, breaking this package's established "rationale-on-const, lean-call-site" convention (`oneShotObserveWindow` doesn't re-explain itself at its call site either). Also, one sentence in the duplicate narrated a *different* function (`gateRecovered`) from inside `gateInjected`. | idiomatic-reviewer | idiom-divergence (blocking) | Deleted the inline comment entirely, matching the `oneShotObserveWindow` precedent. Moved the misplaced sentence to `gateRecovered`'s own doc comment, where it belongs, plus folded in kubernetes-specialist's teardown-asymmetry caveat (a stuck-daemon fail-fast abort skips `gateRecovered`'s teardown assertion; `cleanupChain` still tears down the namespace regardless). |
| 4 | The post-`AllInjected` `Get()` call uses the outer `ctx`, not `injectCtx`, even though the comment claims to bound "injection" as a whole — the stated bound is only half-true. | idiomatic-reviewer | idiom-divergence, runtime-consequence flagged | **Investigated, not applied as suggested.** Threading `injectCtx` into this call, as literally suggested, would introduce a real (if narrow) new bug: a fault injecting near the 90s boundary could leave `injectCtx` with near-zero time left, causing this single fast read to spuriously fail via context-deadline-exceeded on an otherwise-successful injection — worse than the status quo. Kept `ctx` deliberately and added a comment explaining why (only the poll loop is time-boxed; the one-shot follow-up read isn't, on purpose). |
| 5 | Fail-fast path (`gateInjected` timeout) skips `gateRecovered`'s teardown assertion — asymmetric with the normal path. | kubernetes-specialist | caveat, "worth one sentence not a code change" | Folded into `gateRecovered`'s doc comment (see #3's resolution). |
| 6 | Naming: `injectDeadline` labeled a `time.Duration` (a timeout, not a deadline in Go's `context` terminology); sibling const uses `-Window` suffix. | idiomatic-reviewer | style, suggest-only | Renamed to `injectWindow`, matching `oneShotObserveWindow`. |

### Deliberately not changed (vetted)
- Full per-record chaos-mesh phase/error diagnostics on timeout (finding #2's
  "preferred enhancement") — both reviewers who raised it called it optional;
  implementing it now would mean guessing at unstructured CR field names never
  verified against a live cluster. Documented as a future improvement instead.
- Context correctness of the `injectCtx`/`cancel()`/`defer` wiring itself —
  confirmed clean by both systems-engineer (probe 1) and kubernetes-specialist
  (probe 4); no change needed.
- `waitFaultCondition`'s `NotFound`-is-transient handling — confirmed
  unaffected by the shorter window (kubernetes-specialist probe 3).

### Verification
`gofmt -l`, `go build -tags integration ./test/integration/...`, `go vet -tags
integration ./test/integration/...` all clean. `grep -rn "injectDeadline"
test/integration/` returns nothing — full rename, no dangling references.

**Verdict: RESOLVED.** The core mechanism is kept (not reverted — the panel's
verdict was "sound idea, real fixes needed," not "unnecessary"), with the
correctness bug fixed via an enforced invariant, the overclaim reworded
honestly, the duplicated/misplaced comment cleaned up, and one reviewer
suggestion (idiomatic-reviewer's ctx-threading fix) investigated and
deliberately *not* applied because it would have introduced a new narrow bug —
recorded here so the reasoning isn't lost to a future reader diffing against
the suggestion.
