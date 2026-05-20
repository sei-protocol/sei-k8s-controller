# Test Harness — MVP (LLD)

## Status

Draft — 2026-05-20

## Authors

brandon@seinetwork.io

## Background

Two nightly test orchestrators run on the harbor cluster:

- `clusters/harbor/nightly/release/` — qa-testing Mocha suite. ~230-line bash ConfigMap stands up validator + RPC SNDs, polls first block, writes admin mnemonic Secret, applies a Job running the release-test image, streams logs, polls Job terminal condition.
- `clusters/harbor/nightly/major-upgrade/` — SeiNodeTask scenario. Bash ConfigMap stands up a validator SND, fetches a Chaos Mesh Workflow YAML from `raw.githubusercontent` at a pinned `SCENARIO_REF`, `envsubst`s, applies, polls workflow phase.

Both share ~95% of their imperative shape: provision SND(s) → wait Ready + first block → inject test-runtime state → apply test driver → stream logs → wait terminal → cleanup + report upload. The bash has accreted defensive patches against YAML-bool coercion, missing `curl` in distroless, JSON-RPC envelope quirks, IRSA gotchas, and silent-empty `envsubst`. Each fix lands in one or the other configmap, never both.

Major-upgrade already uses a Chaos Mesh Workflow CR as its inner DAG. The harmonization opportunity is to make **Chaos Mesh Workflow the harness for both** — the outer orchestration becomes a Workflow CR, the bash dissolves into a small set of typed Task images, and "test scenario" becomes "Workflow YAML composing Task images." Future scenarios that need chaos overlay (e.g. major-upgrade under NetworkChaos) become a free additional Workflow node.

## Goals

1. Both nightly orchestrators become Chaos Mesh Workflow CRs applied by a thin CronJob. The bash ConfigMaps go away.
2. A small catalog of typed Task images replaces the imperative steps: SND provisioning, key generation, report upload. Each image is single-purpose, typed, and unit-testable. The existing `seitask-runner` image stays.
3. Cross-step state continues to ride the existing workflow-vars ConfigMap pattern. No new CRDs.
4. Scenario authoring is YAML-only: a new test = a new Workflow YAML that composes existing Task images.

## Non-goals (v1)

- A custom orchestrator binary (the platform-engineer "one Go binary harness" pitch). Chaos Mesh is the orchestrator.
- A scenario CRD with a controller. CronJob + Workflow CR + ConfigMap is enough.
- Native chaos overlay scenarios. Free upside; not built in v1.
- A local CLI for test iteration. Velocity-win, separate workstream.
- A typed framework-agnostic result schema. Mochawesome + workflow status are enough for v1.
- Promoting bash chain-state queries (`compute-target-height`, `resolve-proposal-id`) to new SeiNodeTask kinds. Tracked as follow-up; the existing bash steps continue to live in scenario Workflow YAMLs.

## Design

### Architecture

```
CronJob (schedule + scenario pin)
   │
   ▼
applies Workflow CR (the scenario YAML)
   │
   ▼
Chaos Mesh runs DAG of Task templates, each spawning a Pod:
   │
   ├── seitask-keygen          → writes admin Secret + workflow-vars CM
   ├── seitask-provision-snd   → applies SND; waits Ready + first block; writes endpoints to workflow-vars CM
   ├── seitask-runner          → (existing) applies SeiNodeTask CR; polls terminal
   ├── seitask-upload-report   → uploads stdout + workflow trace to S3
   └── release-test            → (existing, external) qa-testing mocha runner
```

CronJob is ~30 lines of typed YAML: image pin, schedule, ConfigMap mount of the Workflow YAML, a `kubectl apply -f /etc/scenario/workflow.yaml` command. Cleanup is owner-referenced cascade from the Workflow CR.

### Task image catalog

Each image lives in `sei-k8s-controller`, alongside the existing `runner/` (`seitask-runner`). One Dockerfile per binary, same ECR pipeline, same commit-SHA tag scheme. Co-built so the Workflow YAML's image pins move in lockstep with the repo.

| Image | Single responsibility | Inputs | Outputs |
|---|---|---|---|
| `seitask-keygen` | Generate secp256k1 admin key | `KEYNAME`, `RUN_ID`, `NAMESPACE` | Secret `${KEYNAME}-${RUN_ID}` (mnemonic), workflow-vars CM entry (`ADMIN_ADDRESS`) |
| `seitask-provision-snd` | Apply SND from typed spec; await Ready; await first block; resolve endpoints | scenario subspec (mounted ConfigMap), `RUN_ID`, `NAMESPACE` | SND CR; workflow-vars CM entries (`TM_RPC`, `EVM_RPC`, `REST`, `CHAIN_ID`) |
| `seitask-runner` (existing) | Render + apply SeiNodeTask from template; poll terminal | template, `--var=K=V` flags | SeiNodeTask CR; stdout result |
| `seitask-upload-report` | Stream stdout + workflow CR + workflownodes trace to S3 | `S3_BUCKET`, `RUN_ID`, `NAMESPACE` | S3 objects under `${NAMESPACE}/${SCENARIO}/${RUN_ID}/` |
| `release-test` (external) | qa-testing mocha runner | endpoints from envFrom + admin Secret | mochawesome.json on emptyDir; exit 0/1/2 |

The four sei-side images are all small Go binaries using controller-runtime client. Each ships as distroless. No bash inside.

### Task image contract

- **Inputs**: env vars (rendered by Chaos Mesh from Workflow template) + optionally a mounted ConfigMap with a typed spec (for `seitask-provision-snd`, which needs a structured SND payload too large for env). **No `envsubst`-style placeholders** inside scenario YAML. Per-run substitution uses Chaos Mesh's native `{{ .Workflow.Name }}` / `inputs.parameters.*` (rendered by the workflow controller, not by the orchestrator). The CronJob applies the scenario YAML literally; the per-run identifier is injected as a Workflow parameter.
- **Outputs**: typed read/write of the per-run ConfigMap (`workflow-vars-<workflow-name>`) via controller-runtime `client.Patch(obj, client.MergeFromWithOptimisticLock{})`. **No `kubectl` shell-out from inside the Task images** — same status-patch discipline the controller uses (see `CLAUDE.md`). The CM ops live in `internal/taskruntime/cm.go`. Downstream Tasks consume the merged CM via `envFrom: configMapRef:`.
- **Exit codes**: 0 pass / 1 task-fail / 2 infra-fail. Matches release-test.ts. Chaos Mesh treats any non-zero as Failed identically; the 1-vs-2 split surfaces only in `seitask-upload-report` (reads a sentinel key the failing Task writes to the CM before exit).
- **Cleanup**: every resource a Task image creates (SND, Secret, ConfigMap entry) carries an `ownerReference` to the parent Workflow CR. The Workflow's UID is injected into each Task pod via the downward API on a fixed env var (`SEI_WORKFLOW_NAME`, `SEI_WORKFLOW_UID`) so the helper in `internal/taskruntime/ownerref.go` can stamp it without an extra API lookup. Workflow deletion cascades to everything; no trap-on-EXIT logic. ownerReferences point at the Workflow CR, never at the Task pod or WorkflowNode (both go away before SND lifecycle completes).

### Cross-step state

The major-upgrade workflow already uses a per-run ConfigMap with an `ownerReference` to the Workflow for GC. We standardize this:

- Every scenario's first step (`seitask-keygen` or `seitask-provision-snd`) creates the ConfigMap with the run's seed values.
- Subsequent Tasks read via `envFrom: configMapRef:` and write new entries via typed `client.Patch(MergeFromWithOptimisticLock{})`.
- Test pods (release-test mocha, seitask-runner) consume the same ConfigMap.
- Sensitive values (admin mnemonic) live in a separate Secret per run; the ConfigMap holds the Secret name as a string and downstream pods reference it via `secretKeyRef`.

#### workflow-vars schema

The key vocabulary is enumerated in a Go file (`internal/taskruntime/vars.go`) as typed constants so producer/consumer drift is a compile error, not a runtime mystery. Initial schema:

| Key | Type | Writer | Stability |
|---|---|---|---|
| `RUN_ID` | string | `seitask-keygen` (first) | additive — never renamed |
| `CHAIN_ID` | string | `seitask-provision-snd` (validator) | one-way door |
| `TM_RPC` | URL | `seitask-provision-snd` (rpc / validator) | one-way door |
| `EVM_RPC` | URL | `seitask-provision-snd` (rpc, optional) | one-way door |
| `REST` | URL | `seitask-provision-snd` (rpc / validator) | one-way door |
| `ADMIN_ADDRESS` | bech32 | `seitask-keygen` | one-way door |
| `ADMIN_SECRET_NAME` | string | `seitask-keygen` | one-way door |
| `TARGET_HEIGHT` / `UPGRADE_HEIGHT` / `POST_UPGRADE_HEIGHT` | int | scenario-specific (major-upgrade) | scenario-private |
| `PROPOSAL_ID` | int | scenario-specific (major-upgrade) | scenario-private |
| `EXIT_REASON` | enum (`pass`/`test-fail`/`infra-fail`) | failing Task | one-way door — read by `seitask-upload-report` |

Renames of one-way-door keys are not permitted. New keys are added freely; downstream Tasks must tolerate missing optional keys.

### Example: release-test scenario

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: Workflow
metadata:
  generateName: release-test-       # workflow controller assigns name → RUN_ID downstream
spec:
  entry: release-test
  templates:
    - name: release-test
      templateType: Serial
      children: [keygen, provision-validator, provision-rpc, run-mocha, upload-report]

    - name: keygen
      templateType: Task
      task:
        container:
          image: <ECR>/seitask-keygen:<sha>
          env:
            - { name: KEYNAME, value: admin }
            - name: SEI_WORKFLOW_NAME
              valueFrom: { fieldRef: { fieldPath: "metadata.labels['chaos-mesh.org/workflow']" } }
            - name: SEI_WORKFLOW_UID
              valueFrom: { fieldRef: { fieldPath: "metadata.annotations['chaos-mesh.org/workflow-uid']" } }

    - name: provision-validator
      templateType: Task
      task:
        container:
          image: <ECR>/seitask-provision-snd:<sha>
          envFrom: [{ configMapRef: { name: workflow-vars } }]   # name resolved from Workflow name
          volumeMounts: [{ name: scenario, mountPath: /etc/scenario }]
          # /etc/scenario/validator.yaml = typed SND subspec (preset, replicas, overrides…)

    - name: provision-rpc
      templateType: Task                                          # same shape

    - name: run-mocha
      templateType: Task
      task:
        container:
          image: <ECR>/release-test:<sha>
          envFrom: [{ configMapRef: { name: workflow-vars } }]
          env:
            - { name: TEST_TARGET, value: chain-agnostic }
            - name: SEI_ADMIN_MNEMONIC
              valueFrom: { secretKeyRef: { name: admin, key: mnemonic } }   # name resolved at run

    - name: upload-report
      templateType: Task
      task:
        container:
          image: <ECR>/seitask-upload-report:<sha>
          envFrom: [{ configMapRef: { name: workflow-vars } }]
          env: [{ name: S3_BUCKET, value: harbor-validation-results }]
```

The current 230-line release-test bash collapses to ~70 lines of Workflow YAML. Per-run identifier is the Workflow name itself (`metadata.generateName` lets the controller assign it); Task pods read it via the downward API. **No `envsubst`, no shell-string templating** — the YAML is applied literally.

#### Per-TEST_TARGET dispatch

`TEST_TARGET` is part of the scenario YAML, not a Workflow parameter. One scenario file per target (`release-test-chain-agnostic.yaml`, `release-test-evm-precompiles.yaml`, …), each pinned by its own CronJob with its own schedule. This keeps the failure surface explicit per-target and avoids hiding matrix dispatch inside CronJob args.

### Example: major-upgrade scenario

The existing `scenarios/major-upgrade.yaml` already has the shape. v1 migration: replace the three bash `compute-target-height` / `resolve-proposal-id` / `wait-for-proposal-to-pass` Task templates with calls to a new `seitask-runner` template that wraps an `AwaitGovProposalStatus` SeiNodeTask kind — OR keep them as bash for v1 and migrate them later as part of the SeiNodeTask kind expansion. The `provision-validator` step replaces the existing `seictl nd apply` bash that lives in the orchestrate.sh wrapper.

### RBAC

One shared `ServiceAccount: seitask-sa` per scenario namespace, bound to a `Role` generated from `// +kubebuilder:rbac:` markers on the Task-image source files — same generation pipeline the controller uses, same `make manifests` workflow, same audit trail. Initial verb set (union over all Task images):

- `sei.io/seinodedeployments`: create/get/list/watch/delete/update
- `sei.io/seinodetasks`: create/get/list/watch
- `chaos-mesh.org/workflows`: get (Task images need the Workflow UID for ownerReferences)
- `core/configmaps`: create/get/patch (workflow-vars)
- `core/secrets`: create/get (keygen writes, mocha reads)
- `core/pods`, `core/pods/log`: get/list/watch (upload-report tailing)

We do **not** reuse the existing `workload-service-account`; that SA has a wider blast radius than what these Tasks need. Per-image SAs are deferred to v2.

### Supply chain

Task images get the same supply-chain treatment as the controller: cosign signing, SBOM attestation, and pinning by `@sha256:` digest in scenario YAML. RBAC-bearing images get no less rigor than the controller manager.

### Scenario scaffold

Adding a scenario in v1 follows a template. `scenarios/_template/` ships in the repo with:

- `workflow.yaml.tmpl` — a minimal Workflow with `keygen → provision → <your test> → upload-report` skeleton
- `validator.yaml` / `rpc.yaml` — example typed SND subspecs the provision Task consumes
- `README.md` — the workflow-vars key vocabulary, the downward-API env-var convention, the one-way-door discipline

New scenarios fork `_template/` and edit. This is the only way to keep scenario conventions consistent as the catalog grows; without it the harmonization regresses within ~2 scenarios.

### Repo / image layout

```
sei-k8s-controller/
  cmd/
    manager/             # existing controller binary
    keygen/              # NEW — seitask-keygen entry
    provision-snd/       # NEW — seitask-provision-snd entry
    upload-report/       # NEW — seitask-upload-report entry
    runner/              # NEW location for existing seitask-runner (moved from runner/)
  internal/
    taskruntime/             # NEW — shared library for the small task images
      vars.go            # typed const keys for workflow-vars CM
      cm.go              # typed client.Patch helpers (MergeFromWithOptimisticLock)
      ownerref.go        # Workflow ownerReference helper (reads SEI_WORKFLOW_UID env)
      exit.go            # 0/1/2 exit code mapping
    ...                  # existing controller internals
  scenarios/
    _template/           # NEW — scenario scaffold (workflow.yaml.tmpl, validator.yaml, README.md)
    release-test-chain-agnostic.yaml
    major-upgrade.yaml   # existing, migrated to new Task images in phase 2
```

ECR builds extend `.github/workflows/ecr.yml` with 3 new `docker/build-push-action` steps (one per new Task image), all SHA-tagged from the same commit. Each image gets cosign signing + SBOM, matching the controller manager's pipeline. Coherent versioning is intrinsic — no SCENARIO_REF vs SEITASK_RUNNER_IMG drift across the four sei-side images.

### Migration plan

1. **Phase 1**: Ship `seitask-keygen`, `seitask-provision-snd`, `seitask-upload-report` images. Each as a separate PR with unit tests. No scenario changes yet.
2. **Phase 2**: Add `scenarios/release-test.yaml`. Migrate `clusters/harbor/nightly/release/` to use it. Delete the bash ConfigMap. Validate on harbor for one nightly cycle.
3. **Phase 3**: Rewrite `scenarios/major-upgrade.yaml` Tasks that today are bash (`seictl nd apply`, the cleanup bash) to use the new Task images. Delete the `orchestrate.sh` ConfigMap. Validate.
4. **Phase 4 (deferred follow-up)**: Promote the three bash chain-query steps (`compute-target-height`, `resolve-proposal-id`, `wait-for-proposal-to-pass`) to typed `SeiNodeTask` kinds. Removes the last bash from any scenario.

Each phase is independently deployable and reversible. Phase 1 ships without touching any nightly orchestrator. Phases 2 and 3 are scenario-by-scenario; if one breaks, the other keeps running.

## Alternatives considered

- **Custom Go orchestrator binary** (platform-engineer's max position). ~1500 lines reinventing what Chaos Mesh Workflow already does: DAG composition, retries, deadlines, log capture, terminal-state semantics, status tracking. The chaos-overlay path costs nothing under Chaos Mesh and is impossible under a custom orchestrator. Rejected.
- **Argo Workflows**. Same DAG semantics as Chaos Mesh Workflow, requires a separate install + RBAC + version-pinning surface. We already run Chaos Mesh for the major-upgrade scenario. Rejected.
- **Tekton Pipelines**. Pipeline-resource-shaped, doesn't compose well with our SeiNodeTask CRs which are already operator-driven. Rejected.
- **Single shared bash framework + symlinks**. Doesn't fix the type-safety failure modes. Rejected.

## Resolved decisions

- **`seitask-provision-snd` input shape**: mounted ConfigMap with YAML SND subspec, validated against a Go struct at Task startup. Operators edit YAML; the Task fails fast on schema drift.
- **Report uploader**: in-process AWS SDK inside `seitask-upload-report`. The image is dedicated; no need for a separate sidecar.
- **OwnerReference target**: parent Workflow CR (not the Task pod, not the WorkflowNode).

## Open questions

- **Mocha report shape.** v1 ships mochawesome JSON to S3 unchanged. A typed framework-agnostic result schema is a separate workstream.
- **`deploy-fixtures` as a SeiNodeTask kind.** Currently lives inside the release-test image. v1-acceptable; un-defer when a fixture-deploy flake costs a nightly.
- **Promoting bash chain-state queries to SeiNodeTask kinds.** Phase 4 follow-up; some bash remains in `scenarios/major-upgrade.yaml` until that ships.

## References

- Existing orchestrators: `/Users/brandon/platform/clusters/harbor/nightly/{release,major-upgrade}/`
- seitask-runner image: `runner/`
- SeiNodeTask MVP LLD: `docs/design/seinode-task-lld.md`
- qa-testing repo: `github.com/sei-protocol/qa-testing` (release-test image source)
- Coral session inputs (platform-engineer, kubernetes-specialist, solidity-developer) consolidated in this design.
