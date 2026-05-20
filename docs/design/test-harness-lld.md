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

- **Inputs**: env vars (rendered by Chaos Mesh from Workflow template) + optionally a mounted ConfigMap with a typed spec (for `seitask-provision-snd`, which needs a structured SND payload too large for env).
- **Outputs**: writes to the workflow-vars ConfigMap (`workflow-vars-${RUN_ID}`) via `kubectl create configmap --dry-run=client -o yaml | kubectl apply -f -` merge. Downstream Tasks consume via `envFrom: configMapRef:`. This is the existing major-upgrade pattern; we standardize it.
- **Exit codes**: 0 pass / 1 task-fail / 2 infra-fail. Matches release-test.ts. Chaos Mesh treats non-zero as Failed; the infra-fail vs test-fail split surfaces in S3-uploaded result records.
- **Cleanup**: each Task image is single-shot and stateless. Resources it creates (SND, Secret) carry `ownerReferences` to the parent Workflow CR. Workflow deletion cascades. No trap-on-EXIT logic.

### Cross-step state

The major-upgrade workflow already uses a per-run ConfigMap (`workflow-vars-${RUN_ID}`) with an `ownerReference` to the Workflow for GC. We standardize this:

- Every scenario's first step (typically `compute-run-id` or `provision-snd`) creates the ConfigMap with the run's seed values.
- Subsequent Tasks read via `envFrom: configMapRef: name: workflow-vars-${SEI_WORKFLOW_RUN_ID}` and write new entries with `kubectl apply` merges.
- Test pods (release-test mocha, seitask-runner) consume the same ConfigMap.
- Sensitive values (admin mnemonic) live in a separate Secret per run; the ConfigMap holds the Secret name as a string and downstream pods reference it via `secretKeyRef`.

This isn't typed end-to-end (it's strings), but the schema is small enough to document per Task. We accept the tradeoff for the v1 simplicity gain over a custom orchestrator.

### Example: release-test scenario

```yaml
apiVersion: chaos-mesh.org/v1alpha1
kind: Workflow
metadata:
  name: release-test-${RUN_ID}
spec:
  entry: release-test
  templates:
    - name: release-test
      templateType: Serial
      children:
        - keygen
        - provision-validator
        - provision-rpc
        - run-mocha
        - upload-report

    - name: keygen
      templateType: Task
      task:
        container:
          image: seitask-keygen:<sha>
          env: [{ name: KEYNAME, value: admin }, { name: RUN_ID, value: ${RUN_ID} }]

    - name: provision-validator
      templateType: Task
      task:
        container:
          image: seitask-provision-snd:<sha>
          envFrom: [{ configMapRef: { name: workflow-vars-${RUN_ID} } }]
          volumeMounts: [{ name: scenario, mountPath: /etc/scenario }]
          # /etc/scenario/validator.yaml = typed SND subspec (preset, replicas, overrides…)

    - name: provision-rpc
      templateType: Task
      # same shape; /etc/scenario/rpc.yaml

    - name: run-mocha
      templateType: Task
      task:
        container:
          image: release-test:<sha>
          envFrom: [{ configMapRef: { name: workflow-vars-${RUN_ID} } }]
          env:
            - name: SEI_ADMIN_MNEMONIC
              valueFrom: { secretKeyRef: { name: admin-${RUN_ID}, key: mnemonic } }
            - name: TEST_TARGET
              value: chain-agnostic

    - name: upload-report
      templateType: Task
      task:
        container:
          image: seitask-upload-report:<sha>
          envFrom: [{ configMapRef: { name: workflow-vars-${RUN_ID} } }]
          env: [{ name: S3_BUCKET, value: harbor-validation-results }]
```

The current 230-line release-test bash collapses to ~60 lines of Workflow YAML. The four template references plus an envsubst run-ID substitution at apply time is the whole orchestration story.

### Example: major-upgrade scenario

The existing `scenarios/major-upgrade.yaml` already has the shape. v1 migration: replace the three bash `compute-target-height` / `resolve-proposal-id` / `wait-for-proposal-to-pass` Task templates with calls to a new `seitask-runner` template that wraps an `AwaitGovProposalStatus` SeiNodeTask kind — OR keep them as bash for v1 and migrate them later as part of the SeiNodeTask kind expansion. The `provision-validator` step replaces the existing `seictl nd apply` bash that lives in the orchestrate.sh wrapper.

### Repo / image layout

```
sei-k8s-controller/
  cmd/
    manager/             # existing controller binary
    keygen/              # NEW — seitask-keygen entry
    provision-snd/       # NEW — seitask-provision-snd entry
    upload-report/       # NEW — seitask-upload-report entry
    runner/              # existing seitask-runner — already at runner/
  internal/
    taskimg/             # NEW — shared library for the small task images
      cm.go              # workflow-vars CM read/write helpers
      ownerref.go        # Workflow ownerReference helper
      exit.go            # 0/1/2 exit code mapping
    ...                  # existing controller internals
  scenarios/
    release-test.yaml    # NEW — the example above
    major-upgrade.yaml   # existing, migrated to new Task images in phase 2
  runner/                # existing seitask-runner Dockerfile
```

ECR builds remain in `.github/workflows/ecr.yml`. Add 3 new `docker/build-push-action` steps (one per new image), all SHA-tagged from the same commit. Coherent versioning is intrinsic — no SCENARIO_REF vs SEITASK_RUNNER_IMG drift.

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

## Open questions

- **Should `seitask-provision-snd` consume a typed Go struct or a YAML manifest of the SND spec?** Leaning Go struct (typed all the way) but YAML is closer to what operators write today. To resolve in the Phase 1 PR.
- **Mocha report shape.** v1 ships mochawesome JSON to S3 unchanged. A unified result schema is a separate workstream; do we sketch the schema in this LLD or punt entirely? Currently punted.
- **Sidecar uploader vs in-process upload in the last Task.** kubernetes-specialist argued for sidecar (no AWS SDK in the orchestrator binary). With our Task-image split, the upload image is dedicated — in-process AWS SDK is fine there, since it's the only thing that image does. Resolved in favor of in-process.

## References

- Existing orchestrators: `/Users/brandon/platform/clusters/harbor/nightly/{release,major-upgrade}/`
- seitask-runner image: `runner/`
- SeiNodeTask MVP LLD: `docs/design/seinode-task-lld.md`
- qa-testing repo: `github.com/sei-protocol/qa-testing` (release-test image source)
- Coral session inputs (platform-engineer, kubernetes-specialist, solidity-developer) consolidated in this design.
