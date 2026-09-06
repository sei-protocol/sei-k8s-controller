# Spec: mutable `SeiNetwork.Spec.ConfigOverrides` / `SeiNode.Spec.Overrides` on a running node

Status: draft v2, for review (v1 cross-checked by an independent codex pass against a fresh
clone — corrections and a materially different §3.4 recommendation are folded in; see the
Revision Notes at the bottom)
Repo: `sei-protocol/sei-k8s-controller`
Scope: `internal/planner/*`, `internal/task/*`, `api/v1alpha1/{seinode,seinetwork}_types.go`,
`internal/controller/{node,nodetask,seinetwork}/*`, and (per §3.4) possibly `sidecar/tasks/*` +
the `sei-config` dependency's `ConfigIntent`/`ResolveIncrementalIntent` path.

## 0. Problem statement (verified against the code, cross-reviewed independently)

**The two fields are already API-mutable today.** Neither `SeiNetwork.Spec.ConfigOverrides`
(`seinetwork_types.go:56`) nor `SeiNode.Spec.Overrides` (`seinode_types.go:47`) carries a
`self == oldSelf` CEL immutability rule — `SeiNetworkSpec`'s only immutability rules cover
`genesis`/`replicas`/`dataVolume` (`seinetwork_types.go:27-29`), and the CEL rules that scan
`self.overrides` on `SeiNodeSpec` (`seinode_types.go:24-26`) are content-*restrictions*
(no `chain.freeze_height` / `chain.halt_height` / `chain.halt_time` in overrides), not
immutability. **This is not a CRD/CEL relaxation task** — the API server already admits an edit
to either field post-create.

**The gap is that the controller ignores the edit once a node is `Running`.**

1. `SeiNetwork.Spec.ConfigOverrides` → `SeiNode.Spec.Overrides` propagation already works:
   `generateSeiNode`/`ensureSeiNode` diff and live-update the child's `Overrides` on every
   reconcile (`internal/controller/seinetwork/nodes.go:165,201`), skipped only while paused or
   while a *network*-level plan is active (`nodes.go:25,30` — this guard only creates a temporary
   staleness window; ordinary reconciliation re-runs `ensureSeiNode` afterward and nothing is
   permanently lost).
2. Every mode's Running-node planning path checks only image drift before falling through to
   readiness reapproval — `full.go:53`, `replay.go:56`, `seed.go:56`, `validator.go:125`.
   `node.Spec.Overrides` is not read anywhere in that decision.
3. `Overrides` is folded into a `ConfigIntent` and applied only on the **init/bootstrap** branch
   (e.g. `full.go:35`), which runs only while the node has not yet reached `Running`.

Net effect: an edit to either field on an already-`Running` node has no effect until the node is
re-bootstrapped/replaced. (No existing test directly exercises "edit `Overrides` on an
already-Running node and assert nothing happens" — that's an actual coverage gap, not just an
unverified claim; §8 adds it.)

**Goal:** give a `Running` node's planner a drift check for `Overrides`, symmetric with the
existing image-drift check, whose plan gets the change onto the live node and restarts `seid` —
without requiring a full pod replace when only config drifted.

## 1. Non-goals

- No CRD/CEL change is required to make the fields *mutable* — they already are.
- No change to `SeiNetwork`'s propagation path — it already does the right thing.
- Not a replacement for the image-drift update plan or the heavy migration workflow
  (`internal/planner/workflow.go`) — both untouched; this adds a third, narrower trigger.
- **Not a decision, yet, on whether the delivery mechanism is `TaskConfigPatch` or incremental
  `TaskConfigApply`** — §3.4 below is now the central open architecture question this spec exists
  to pose, not a settled implementation detail. Either choice keeps the *bootstrap* branch
  (whole-config `TaskConfigApply`, non-incremental) exactly as it is today.

## 2. Design overview

Add an `overridesDrifted(node)` check to every mode's Running-node planning path, alongside the
existing image-drift checks, tracked against a new `SeiNode.Status.CurrentOverrides` field
(mirroring `Status.CurrentImage`). Two plan shapes, chosen by which drift fired:

- **Overrides drifted, image did not** (the new, lighter path): patch the live config, validate
  it, restart `seid` in place via the existing `RestartSeid` mechanism, then record what was
  applied. No `ApplyStatefulSet`/`ApplyService`/`ReplacePod`/`MarkReady` — the pod spec is
  unchanged, and per `SeiNodeTaskKindRestartSeid`'s documented semantics
  (`seinodetask_types.go:49`, `sidecar/tasks/restart_seid.go:103`), the sidecar process survives
  an in-place `seid` restart, so its readiness flag never drops and there's no
  mark-not-ready/mark-ready dance to run.
- **Image (also) drifted**: reuse the existing heavy path (`full.go:56`:
  `ApplyStatefulSet → ApplyService → ConfigPatch → ConfigValidate → ReplacePod → ObserveImage →
  MarkReady`, pinned by `node_update_test.go:98`) unchanged in shape, with its config-delivery
  step extended to also carry the overrides change (§3.4) and an `ObserveOverrides`
  status-update added alongside `ObserveImage`. `ReplacePod` already restarts `seid`, so no
  separate restart step is needed on this path.
- **Validator/seed nodes**: both plan shapes must preserve the mode-specific pre-flight gates
  those modes already prepend — validator's signing-key/node-key/operator-keyring validation
  (`validator.go:128`) and seed's node-key validation (`seed.go:59`). These are not optional
  scaffolding; they run before either plan shape's config step today and must keep doing so.

## 3. Detailed design

### 3.1 New status field — `SeiNode.Status.CurrentOverrides`

Add `CurrentOverrides map[string]string`, additive/optional on `SeiNodeStatus`, mirroring
`Status.CurrentImage`. Populated by a new `ObserveOverrides` status-mutation, in both places
overrides get applied to a node:

- The new running-node update plan (§2), on success.
- **Also the existing bootstrap `ConfigApply → ConfigValidate → MarkReady` progression**
  (`planner.go:65`, `bootstrap.go:159`). This is a correction from v1: if only the running-node
  update plan populates `CurrentOverrides`, every node that bootstraps with a non-empty
  `Spec.Overrides` reaches `Running` with `Status.CurrentOverrides` still unset/empty, so
  `overridesDrifted` fires a spurious update plan on the very next reconcile. The bootstrap path
  must seed `CurrentOverrides` from the same merged-overrides value it just applied
  (`mergeOverrides(mergeOverrides(commonOverrides(node), controllerOverrides(node)),
  node.Spec.Overrides)`), not leave it for the running-node path to discover as "drift."

Flag this field for human sign-off per the CRD-contract-is-a-one-way-door guardrail (additive,
but a new piece of the status contract).

### 3.2 Drift detection — `overridesDrifted`

```go
func overridesDrifted(node *seiv1alpha1.SeiNode) bool {
    return !maps.Equal(node.Spec.Overrides, node.Status.CurrentOverrides)
}
```

Added to every mode's Running-node trigger, alongside `imageDrifted`/`sidecarImageDrifted`
(`full.go:53` and the equivalent lines in `archive.go`, `replay.go:56`, `seed.go:56`,
`validator.go:125`). This is the first Running-node update trigger keyed off a non-image spec
field — there's no existing "spec-field diff → plan" pattern to copy beyond the image-drift
shape; treat this as the template going forward.

**Removal semantics (new in v2 — flagged by cross-review, unresolved, needs a decision):**
`maps.Equal` correctly detects a key's removal from `Spec.Overrides` as drift, but what should
then be *applied* is not obvious: is a removed override key expected to (a) revert to whatever
`commonOverrides`/`controllerOverrides` would set for it, (b) revert to seid's built-in default
(neither controller- nor user-specified), or (c) simply stay at whatever value is currently on
disk (i.e., removal is a no-op for already-applied keys, only suppresses future application)?
The two candidate delivery mechanisms in §3.4 handle this differently and neither has an
out-of-the-box answer:
- Plain `TaskConfigPatch`/`tomlpatch.Merge` only *deletes* a key when the patch explicitly
  supplies `nil` for it (`sidecarapi/tomlpatch/merge.go:12`) — a removed `Overrides` entry
  produces neither a value nor a `nil` unless the translator explicitly diffs old vs. new
  `Overrides` and emits `nil` for dropped keys, and even then "delete the TOML key" isn't the
  same as "restore it to the controller/seid default."
- Incremental `ConfigIntent`/`ResolveIncrementalIntent` resolves a *complete* desired
  configuration through sei-config's schema each time (not a key-by-key patch), so it may
  already produce the right "reverts to non-user default" behavior for free — this needs
  confirming with a sei-config maintainer/spike before committing to either mechanism, since it
  materially affects which one is simpler to get right.
This must be resolved and written up (with a chosen, testable semantics) before implementation
starts; do not leave it implicit.

### 3.3 Plan assembly

Branch on which drift fired, per §2:
- image (also) drifted → existing heavy list, config-delivery step extended (§3.4),
  `ObserveOverrides` appended alongside `ObserveImage`.
- overrides-only → light list: config-delivery step → validate → `RestartSeid` →
  `ObserveOverrides`.
- Validator/seed: prepend the existing mode-specific key-validation gates (§2) unchanged, on
  both shapes.

`Active: true`, `TargetPhase: PhaseRunning`, empty `FailedPhase` (transient retry), matching the
existing update plans — this is a routine config-drift correction, not a terminal condition.

### 3.4 Config-delivery mechanism — the central open decision (revised in v2)

v1 of this spec assumed the delivery mechanism must be `TaskConfigPatch`, reasoning from the
repo's enforced convention *"init writes whole config via `TaskConfigApply`; Running node patches
only via `TaskConfigPatch`; never both"* (`planner.go:65`, pinned by
`TestPlannerConvention_InitUsesApply_UpdateUsesPatch`, `node_update_test.go:299`). Independent
cross-review found that convention is real and enforced, **but also found a fact v1 got wrong**:
the premise that `TaskConfigApply` is *incapable* of incremental, day-2 use is false. The
sei-config dependency's `ConfigIntent` explicitly supports an `Incremental` mode documented for
exactly this: reading existing on-disk configuration and patching it for day-2 changes
(`sei-config/intent.go:29`), and the sidecar already has a code path for it —
`applyIncremental` (`sidecar/tasks/config_apply.go:29,71`) reads both files, calls
`ResolveIncrementalIntent`, and writes the resolved configuration back through the *same*
schema-aware, **validated** resolution `ApplyOverrides`/the registry use at bootstrap
(`sei-config/io.go:161`, `sei-config/registry.go:46`) — there is even a repository test applying
`"evm.http_port"` incrementally and verifying it on disk (`sidecar/tasks/config_apply_test.go:104`).

This changes the recommendation. Two real candidates, not one:

**Option A — new `TaskConfigPatch` translator (v1's original plan).** Requires building a new
dotted-sei-config-key → (file, TOML-section-path, raw-key) translator; per cross-review, **no
such reusable resolver exists to call into** — sei-config's registry maps dotted keys to unified
Go struct field paths, not legacy file/key coordinates, and the two legacy-file conversions are
hard-coded (`sei-config/io.go:115`), so exposing the needed mapping "would not be a trivial
wrapper around current metadata." Worse: `TaskConfigPatch`'s sidecar handler is a raw,
**untyped, unvalidated** recursive merge onto a TOML file (`sidecar/tasks/config.go:60`) — unlike
incremental apply, nothing schema-validates the translated keys/values before they're written.
Preserves the current convention/test's literal meaning ("update path only ever uses
`TaskConfigPatch`") at the cost of real, non-trivial new code with a materially higher
correctness burden and no schema validation safety net.

**Option B — incremental `TaskConfigApply` (`ConfigIntent{..., Incremental: true}`), recommended
by default.** Reuses the exact resolution + validation pipeline the bootstrap path already
trusts — no new translator, no new unvalidated write path. The cost is that it **requires
deliberately evolving** the "`TaskConfigApply` is init-only" convention and updating
`TestPlannerConvention_InitUsesApply_UpdateUsesPatch` to state the convention it should become:
something like *"a Running node never uses non-incremental (whole-config) `TaskConfigApply`;
incremental `TaskConfigApply` is the sanctioned Running-node path for `Overrides` changes,
`TaskConfigPatch` remains the sanctioned path for controller-derived p2p/typed-migration
patches."* That is a deliberate, named convention change, not an oversight to quietly work
around — call it out explicitly in the PR/review that implements this and update the test's
name/intent alongside the code, not after.

**Recommendation:** default to Option B unless the sei-config/sidecar-team push back on
evolving the convention, given Option A's higher, harder-to-test correctness risk (§ above) and
the removal-semantics ambiguity in §3.2 being more likely to fall out "for free" from the
schema-aware incremental resolver than from a hand-rolled patch tree. Whoever picks this up
should spike both for a day before committing, specifically to answer the §3.2 removal-semantics
question empirically against a real `sei-config` build.

Either option: fold `mergeOverrides`'s existing user-overrides-win precedence
(`mergeOverrides(mergeOverrides(commonOverrides(node), controllerOverrides(node)),
node.Spec.Overrides)`) unchanged, and keep the freeze/halt content-guard keys
(`chain.freeze_height`, `chain.halt_height`, `chain.halt_time`) excluded/denied at the same point
translation happens, mirroring the CEL admission guard so a mid-life edit can't smuggle in what
CEL already forbids at create.

### 3.5 Restart step — `RestartSeid` vs `ReplacePod`

Use the existing `SeiNodeTaskKindRestartSeid` (`api/v1alpha1/seinodetask_types.go:59`, sidecar
wire `TaskRestartSeid`) for the overrides-only light path, reusing the existing
`defaultRestartSeidTimeout` (10m) wiring (`internal/controller/nodetask/controller.go:59,369`)
rather than inventing a new timeout. Per cross-review, `RestartSeid` today is **only ever
constructed from operator-submitted `SeiNodeTask` objects** (`internal/task/seinodetask_params.go:207`)
— it is never currently emitted by a node/workflow planner. This spec is the first design to give
a planner standing authority to submit it autonomously; see §7.2.

Do **not** invoke the heavy migration workflow's `mark-not-ready → stop-seid → reset-data →
config-patch → configure-state-sync → mark-ready` sequence (`workflow.go:68-71`) — that path
wipes data and is reserved for typed `ConfigMigration`s; an overrides edit is not a migration.

When image also drifted, `ReplacePod` (already in the heavy plan) restarts `seid` as a side
effect of pod recreation — no additional `RestartSeid` step needed on that path.

### 3.6 New task — `ObserveOverrides`

Mirrors `ObserveImage`: on success, sets `Status.CurrentOverrides = node.Spec.Overrides` (a
value copy via `maps.Clone`, not an aliased reference — same discipline as
`nodes.go:201`'s `maps.Clone(network.Spec.ConfigOverrides)`). Runs in both the running-node
update plan (§3.3) and the bootstrap progression (§3.1's correction). In-memory-only executor
mutation, per the plan-driven-reconciliation model — never a direct cluster write.

### 3.7 `SeiNetwork` level — no change

Propagation already works (§0). No change needed; the plan-in-progress propagation-skip guard
(`nodes.go:30`) only creates a temporary staleness window, confirmed by cross-review — not a
correctness bug.

## 4. CRD / API changes

- No new/changed CEL immutability rule — none exists to relax on either field.
- Existing content-restriction CEL rules (`seinode_types.go:24-26`) stay in force and must keep
  rejecting forbidden keys on a post-create edit — add a regression test confirming this (they
  already run on every update since they aren't `oldSelf`-scoped, so no code change is expected
  here, just coverage).
- New additive `SeiNodeStatus.CurrentOverrides` field; regenerate via `make manifests generate`;
  never hand-edit `zz_generated.deepcopy.go`/`manifests/`. Flag for human sign-off (§3.1).
- **If Option B (§3.4) is chosen:** update
  `TestPlannerConvention_InitUsesApply_UpdateUsesPatch`'s name and assertion to state the revised
  convention explicitly, in the same change that introduces incremental `TaskConfigApply` on the
  Running path — don't leave the test's name lying about what the code now does.
- Documentation: extend the existing `SeiNetworkSpec` doc-comment warning that a `spec.image`
  edit "rolls every genesis validator near-simultaneously, which briefly interrupts consensus" to
  also cover `configOverrides`, since this feature gives that field the same restart blast radius
  it previously didn't have.

## 5. Failure handling / idempotency

- Follow existing `Terminal(err)`/transient classification per task; model `RestartSeid`'s split
  on `nodetask/controller_test.go`'s existing `_SidecarFailLoud_Fails`/timeout cases.
- Deterministic UUIDv5 task IDs apply unchanged — a controller restart mid-plan rejoins
  in-flight config-delivery/`RestartSeid` tasks rather than double-submitting.
- Plan persists to `.status.plan` and requeues immediately before executing (unchanged
  discipline).

## 6. Observability

No new condition type needed — existing phase/`Ready` conditions reflect an active plan the same
way an image-drift plan already does. `Status.CurrentOverrides` gives an operator a
`kubectl`-visible "what's actually live" record, distinct from `Spec.Overrides` ("what's
desired").

## 7. One-way doors / risks flagged for human approval

1. **New `SeiNodeStatus.CurrentOverrides` field** — additive, but a status-contract addition.
2. **First-ever "the controller can auto-restart a live `seid`" trigger from a plain config edit,
   with no operator confirmation step.** `RestartSeid` today is operator-submitted only (§3.5);
   this design gives the reconciler standing authority to submit it autonomously. Confirm this is
   the intended trust model before building — anyone with edit access to a `SeiNetwork`/`SeiNode`
   object can trigger a live fleet-wide restart by editing a config map field.
3. **Concurrent restart blast radius on `SeiNetwork`** — a `configOverrides` edit propagates to
   every child in the same reconcile pass, each independently resolving its own drift and
   restarting on its own schedule; no serialization exists today (same as the existing image-bump
   blast radius, but this is a new trigger for it — the doc-comment update in §4 must say so).
4. **The `TestPlannerConvention_InitUsesApply_UpdateUsesPatch` convention change**, if Option B
   (§3.4) is chosen — this is a deliberate redefinition of an enforced convention, not a bug fix;
   surface it explicitly for review rather than quietly relaxing the test's assertion.

## 8. Test plan

- Add a test proving today's dormancy explicitly (§0's coverage gap): edit `Overrides` on an
  already-`Running` node, assert `ResolvePlan` returns nil pre-change (this spec's baseline).
- Planner drift/plan-shape (`internal/planner/node_update_test.go`, sibling to the existing
  per-mode image-drift cases): add `_OverridesDrift_UpdateProgression` (light plan),
  `_OverridesAndImageDrift_SinglePlan` (heavy plan, merged config step, no duplicate restart), and
  a validator/seed variant asserting the mode-specific pre-flight gates still run first (§2).
  Update/rename `TestPlannerConvention_InitUsesApply_UpdateUsesPatch` if Option B is chosen (§4).
- Config-delivery payload correctness: exhaustive per-key tests against every currently-documented
  `Overrides` key (not just `p2pConfigPatch`'s two), plus explicit removal-semantics cases per
  the §3.2 decision, plus a case confirming the freeze/halt content-guard keys are rejected
  consistently with the CEL admission guard.
- `RestartSeid` planner-submitted wiring: model on `nodetask/controller_test.go`'s existing
  `RestartSeid` cases, adapted for a planner-submitted (not operator-submitted) call path.
- Bootstrap seeding: assert a freshly-bootstrapped node with non-empty `Spec.Overrides` has
  `Status.CurrentOverrides` populated and does **not** immediately trigger a spurious update plan
  on its first Running-phase reconcile (§3.1 fix).
- Propagation interaction: extend `seinetwork/nodes_test.go`'s configOverrides-propagation cases
  with a propagation-during-active-network-plan case (§3.7).

## 9. Open questions (resolve before/at start of implementation)

1. **Option A vs. Option B (§3.4)** — the central decision this spec poses; recommend spiking
   both for a day against real `sei-config` before committing.
2. **Override-removal semantics (§3.2)** — needs an explicit, testable answer, likely informed
   by whichever option wins in (1).
3. Full map vs. content hash for `Status.CurrentOverrides` — recommend the full map for parity
   with `Status.CurrentImage`'s directness, unless override sets are large in practice.
4. Confirm the intended trust model for autonomous `RestartSeid` (§7.2) with whoever owns the
   fleet-operations runbook before implementation.
5. Should a `SeiNetwork`-level edit's fleet-wide restart be staggered/serialized across sibling
   nodes now that `configOverrides` carries the same blast radius as an image bump (§7.3)? Out of
   scope for this spec's initial cut; a follow-up decision.

## 10. Rollout

No feature-flag mechanism exists in this controller's conventions (plans are level-triggered and
unconditional once a drift trigger is added), so stage as: (a) land behind an explicit review
sign-off on §7.2's trust-model question and §4's convention-change item if Option B is chosen;
(b) trial on a non-production `SeiNetwork` — manually edit `configOverrides`, confirm the plan
converges and `Status.CurrentOverrides` matches, confirm a *removed* override key behaves per the
§3.2 decision — before rolling to any fleet where a mistranslated/misresolved patch would
misconfigure and restart every node at once.

---

## Revision notes (v1 → v2)

An independent cross-vendor technical review (different model/vendor than the drafting pass,
against its own fresh clone plus the `sei-config` dependency) found v1 accurate on the
fundamentals (fields already mutable, running-node planners ignore `Overrides`, the
`TestPlannerConvention_InitUsesApply_UpdateUsesPatch` convention is real, `RestartSeid`'s
no-mark-not-ready semantics are correctly characterized) but flagged:

- An over-cited test (`TestFullPlanner_NoDrift_ReturnsNil` doesn't actually test an override
  edit) — fixed in §0.
- **The single biggest correction:** v1 assumed `TaskConfigPatch` + a new translator was the only
  viable delivery mechanism. Incremental `TaskConfigApply` (`ConfigIntent.Incremental`) already
  exists, is already wired sidecar-side, and already gets schema-aware validation for free —
  this is now presented as the recommended default option, not a hand-rolled-translator-only
  design (§3.4).
- Two missing correctness caveats folded in: override-*removal* semantics were undefined (§3.2),
  and newly-bootstrapped nodes would spuriously "drift" on their first Running reconcile unless
  the bootstrap path also seeds `Status.CurrentOverrides` (§3.1).
- The plan shape isn't uniform across all modes — validator/seed prepend key-validation gates
  that both new plan shapes must preserve (§2, §3.3).
