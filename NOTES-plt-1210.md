# PLT-1210 investigation notes (Spec 002, sei-k8s-controller)

Investigation phase for PLT-1210. Written before implementation, committed first
so the findings survive a host recycle.

Base: `plt-1209-configvalue-crd-field` @ 29f6f47.
All file:line citations are against that tree.

---

## Q: Does ANY mechanism today restart seid when ONLY config changes (no image change)?

**Verdict: CONFIRMED — no. Nothing restarts seid on a config-only change.**

Drift detection for a Running node is image-only (seid image or sidecar image).
A config-only edit produces no plan at all: no config write, no pod replace, no
seid restart. Config-only materialization is entirely NEW behaviour and is the
bulk of this ticket, not a detail.

### Evidence

**1. The only Running-node plan trigger is image drift.**

`NodeResolver.ResolvePlan` (internal/planner/planner.go:144) dispatches through
`plannerForMode` (planner.go:311) to a mode planner's `BuildPlan`, which for
`PhaseRunning` delegates to `buildRunningPlan`. All five implementations open
with the same gate:

| Mode | Gate |
|---|---|
| full | internal/planner/full.go:54 |
| archive | internal/planner/archive.go:45 |
| validator | internal/planner/validator.go:126 |
| seed | internal/planner/seed.go:57 |
| replayer | internal/planner/replay.go:57 |

Each is literally `if imageDrifted(node) || sidecarImageDrifted(node, p.platform)`.

- `imageDrifted` — planner.go:764-766 — `node.Spec.Image != node.Status.CurrentImage`.
- `sidecarImageDrifted` — planner.go:772-777 — effective sidecar image vs
  `Status.CurrentSidecarImage`.

Neither consults `spec.overrides`, `spec.configValues`, or any config state.

**2. The only other Running-node plan is a bare mark-ready.**

`sidecarNeedsReapproval` (planner.go:744-747) → `buildMarkReadyPlan`
(planner.go:750). One task, `mark-ready`. No config write, no restart.

**3. Config on the update path is a passenger, never a driver.**

`p2pConfigPatch` (planner.go:809-818) supplies the `config-patch` payload on an
update plan. Its own doc comment is explicit:

> Peer churn alone does NOT trigger a plan — this only piggybacks the latest
> valid state onto a deployment we are doing anyway.

**4. `spec.overrides` is init-only, so editing it on a Running node is inert.**

`mergeOverrides` (defined planner.go:856) folds `node.Spec.Overrides` into a
`ConfigIntent` at six call sites, every one of them on the init/bootstrap path:

- internal/planner/full.go:42
- internal/planner/archive.go:37
- internal/planner/validator.go:109
- internal/planner/seed.go:47
- internal/planner/replay.go:46
- internal/planner/bootstrap.go:162

`paramsForUpdateTask` (planner.go:846-851) is the update-path params factory and
its comment states "Update plans never carry a ConfigIntent — those are
init-path only."

> Correction to the task brief: the brief said `mergeOverrides` lives in
> `full.go`, `archive.go` and `bootstrap.go`. It is *defined* in `planner.go:856`
> and *called* from **six** sites — the three named plus `validator.go:109`,
> `seed.go:47` and `replay.go:46`. All five mode planners fold overrides, so the
> overlay change touches five planners, not three.

**5. `restart-seid` exists and is plan-reachable, but no planner emits it.**

- Sidecar handler: sidecar/tasks/restart_seid.go:103, registered
  sidecar/serve.go:137.
- Wire type: sidecarapi/wire/wire.go:24.
- Already in the controller's plan-task registry:
  internal/task/task.go:210 — `sidecar.TaskTypeRestartSeid: sidecarTask[sidecar.RestartSeidTask](false)`.

So a `TaskPlan` *can* carry `restart-seid` with no new task plumbing. Nothing in
`internal/planner` ever puts it in one. Its only route today is an
operator-authored `SeiNodeTask{kind: RestartSeid}` — api/v1alpha1/seinodetask_types.go:59,
internal/task/seinodetask_params.go:214 — i.e. manual and out-of-band.

**6. No content-hash / checksum-annotation restart mechanism exists.**

`grep -rni "confighash|config-hash|configChecksum|checksum" --include=*.go internal/`
returns nothing. There is no "roll the pod when the rendered config changes"
annotation anywhere.

**7. The repo already documents this in its own CRD text.**

api/v1alpha1/seinode_types.go:41, the CEL message pinning `spec.resources` as
create-only, says the quiet part out loud:

> a change is not rolled onto a running pod — the StatefulSet is OnDelete and
> **drift detection is image-only**

That is the codebase asserting the verdict about itself.

---

## Secondary finding that shapes the implementation: config-apply ERASES unknown keys

This was not in the brief and it constrains task ordering, so it is recorded here.

`config-apply` does not merge — it **regenerates** `config.toml` and `app.toml`
wholesale from sei-config's typed model:

- sidecar/tasks/config_apply.go:43 — `seiconfig.ResolveIntent(intent)`
- sidecar/tasks/config_apply.go:59 — `seiconfig.WriteConfigToDir(result.Config, a.homeDir)`
- sei-config@v0.0.28 io.go:104 — `WriteConfigToDir` encodes from
  `cfg.toLegacyTendermint()` / the typed app config.

A key that is not in sei-config's struct model cannot survive that round-trip.
Two consequences:

1. **The overlay must be applied strictly after `config-apply`,** never folded
   into `ConfigIntent.Overrides`. Folding it in would also route it through
   sei-config's allow-list, which the ticket explicitly forbids ("must NOT touch
   sei-config").
2. On the running path, re-running `config-apply` is precisely how a *removed*
   config value returns its key to the base value — it rebuilds the base, and the
   recomputed overlay is then re-applied over it. This is exactly what spec.md:300
   describes ("recomputes the overlay over the base config on every start and on
   every change").

**`config-validate` tolerates the unvalidated keys.** `seiconfig.ReadConfigFromDir`
(io.go:23) decodes via `mapstructure` with `ErrorUnused` left at its default
`false` (io.go:63-71), so unknown TOML keys are ignored rather than rejected.
An arbitrary overlay key therefore passes `config-validate` silently — which is
the correct outcome for SC-012, and leaves seid-at-load as the thing that
refuses a bad value (SC-010).

---

## Consequence for scope: this ticket is two separable pieces

1. **Overlay merge + init materialization.** Fold `spec.configValues` into an
   overlay and apply it via `tomlpatch` after `config-apply` on the init path.
   Self-contained, lands independently, delivers SC-003, SC-008, SC-011, SC-012
   (and SC-001, SC-002, SC-006, SC-009 which are outside this ticket's named set).

2. **Config-only drift trigger + restart.** New behaviour per the verdict above:
   detect that the applied overlay differs from the desired one on a Running
   node, build an update plan that recomputes and rewrites config, and restart
   seid. Delivers SC-004, and the running-node half of SC-010.

Piece 2 depends on piece 1 (there is no overlay to materialize until piece 1
exists); piece 1 does not depend on piece 2. Implemented in that order, as
separate commits, so piece 1 can land alone if piece 2 needs more review.
