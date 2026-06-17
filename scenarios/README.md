# Scenarios

End-to-end Chaos Mesh Workflows that compose `SeiNodeTask` CRs to exercise
chain-lifecycle behavior against real Kubernetes clusters. Each scenario is
the acceptance test for one capability surface.

> **Status: runnable, gated on runner image publishing.** PR 6 closed the
> cross-step variable bridge gap via a per-Workflow-run ConfigMap
> (`workflow-vars-<run-id>`). The bash steps that compute `TARGET_HEIGHT`
> and resolve `PROPOSAL_ID` `kubectl apply` the ConfigMap; every other
> step reads values via `envFrom`. End-to-end runs require a published
> `seitask-runner` image (see Prerequisites, item 4) -- everything else
> is in-tree.

## Index

| File | Mirrors | Purpose |
|---|---|---|
| `major-upgrade.yaml` | `sei-chain/integration_test/upgrade_module/major_upgrade_test.yaml` | 4-validator software-upgrade flow: gov proposal, vote, then a single SeiNetwork image bump that rolls all validators onto the new binary at the upgrade height. MVP acceptance for the SeiNodeTask CRD. |
| `testnet-deployment.yaml` | n/a | Reference 4-validator `SeiNetwork` the Workflow can target. |

## Where this runs

These scenarios are **destructive**. They submit governance proposals, mutate
SeiNode images, and drive validators through CrashLoop states. They are
designed for:

- The **harbor dev cluster** (`harbor-dev` EKS). Ephemeral testnets only.
- Local `kind`/`minikube` clusters with the controller installed.
- **Not** any cluster carrying a chain you care about.

The Workflow does not provision the chain. It assumes a 4-validator
`SeiNetwork` exists in the target namespace before the Workflow
applies. See "Run" below.

## Prerequisites

1. **CRDs installed** in the target cluster:
   - `seinetworks.sei.io`
   - `seinodes.sei.io`
   - `seinodetasks.sei.io`

   ```bash
   kubectl apply -f config/crd/
   ```

2. **Controller running** in `sei-k8s-controller-system` (or wherever the
   platform repo installs it) and watching the target namespace.

3. **Chaos Mesh installed** in the cluster (2.5+ verified). The dev cluster
   already ships this via `platform/clusters/dev/chaos-mesh`.

4. **`seitask-runner` image published** to a registry the cluster can pull
   from. As of PR 5 the runner image is **not yet auto-published** by the
   `ecr.yml` GitHub Action (which only builds the controller). Until that
   workflow is extended, the operator must build and push manually:

   ```bash
   make runner-image RUNNER_IMG=<registry>/sei/seitask-runner:<tag>
   make runner-push  RUNNER_IMG=<registry>/sei/seitask-runner:<tag>
   ```

   The image bundles the per-kind templates at `/templates/`; no ConfigMap
   override is required for the in-tree scenarios.

5. **RBAC for the runner ServiceAccount.** `runner/rbac.yaml` defines the
   `seitask-runner` ServiceAccount + Role + RoleBinding. Apply it to the
   namespace where the Workflow will run.

   Chaos Mesh `Task.container` does NOT expose `serviceAccountName` -- the
   synthesized Workflow pod uses the namespace's `default` ServiceAccount.
   You therefore need to either (a) bind the `seitask-runner` Role to the
   `default` SA in the test namespace, or (b) use the
   `chaos-mesh.org/inject-serviceaccount` annotation if your Chaos Mesh
   build includes the SA-injection webhook (2.6+ optional component).

   Recommended: bind to `default` SA in the ephemeral namespace.

   ```bash
   kubectl apply -n <namespace> -f runner/rbac.yaml
   kubectl create rolebinding seitask-runner-default \
     --role=seitask-runner --serviceaccount=<namespace>:default \
     -n <namespace>
   ```

## Run: major-upgrade

### 1. Apply the reference testnet

```bash
kubectl apply -f scenarios/testnet-deployment.yaml
# wait for status.replicas == status.readyReplicas == 4
kubectl -n majorupgrade wait --for=condition=NodesReady=true \
  --timeout=15m seinetwork/majorupgrade
# spot-check the validator phases
kubectl -n majorupgrade get seinodes
# expected: majorupgrade-0 .. majorupgrade-3, all Running.
```

### 2. Render and apply the Workflow

The Workflow YAML uses `envsubst`-style placeholders so the same file works
across upgrade targets. Substitute and apply:

```bash
export SEI_DEPLOYMENT=majorupgrade
export SEI_NAMESPACE=majorupgrade
export SEI_CHAIN_ID=majorupgrade-1
export SEI_PRE_UPGRADE_IMG=ghcr.io/sei-protocol/sei:v6.3.0
export SEI_POST_UPGRADE_IMG=ghcr.io/sei-protocol/sei:v6.4.0
export SEI_UPGRADE_NAME=v6.4.0
export SEITASK_RUNNER_IMG=<registry>/sei/seitask-runner:<tag>
# Unique per Workflow run -- drives the `workflow-vars-<id>` ConfigMap
# name. Two concurrent runs of the same Workflow must not collide.
export SEI_WORKFLOW_RUN_ID="$(date +%s)-$(openssl rand -hex 3)"

envsubst < scenarios/major-upgrade.yaml \
  | kubectl apply -n "${SEI_NAMESPACE}" -f -
```

`envsubst` is from the `gettext` package (preinstalled on most Linux distros;
`brew install gettext` on macOS). For Flux/ArgoCD-managed deployments, replace
the envsubst step with a kustomize patch or `flux create kustomization
--substitute-from=secret/...`.

### 3. Watch progress

```bash
# Workflow node states
kubectl -n majorupgrade get workflownodes -l workflow=major-upgrade
# top-level
kubectl -n majorupgrade describe workflow major-upgrade
# tail step containers as they spin up
kubectl -n majorupgrade get pods -l chaos-mesh.org/workflow=major-upgrade -w
```

### 4. Interpret results

Each step exits 0 (PASS) or 1 (FAIL). Chaos Mesh records terminal status on
the corresponding `WorkflowNode`. The Workflow itself is `Succeeded` when
every step in the entry Serial path completed; `Failed` when any required
step (no `conditionalBranches` override) failed.

Per-step interpretation:

| Step | What success means |
|---|---|
| `compute-target-height` | Created `workflow-vars-${SEI_WORKFLOW_RUN_ID}` ConfigMap with `TARGET_HEIGHT` / `UPGRADE_HEIGHT` / `POST_UPGRADE_HEIGHT`. |
| `submit-upgrade-proposal` | SeiNodeTask `.status.phase=Complete`. proposalId is NOT extracted here (sidecar structured outputs are intentionally empty post-PR 3); `resolve-proposal-id` derives it from the chain. |
| `resolve-proposal-id` | Polled gov REST for a voting-period proposal whose plan name matches `$SEI_UPGRADE_NAME`, merged `PROPOSAL_ID` into the workflow-vars ConfigMap. |
| `vote-yes-all-validators` | All 4 vote tasks Complete. |
| `wait-for-proposal-to-pass` | Proposal observed `PROPOSAL_STATUS_PASSED`. |
| `bump-snd-image` | `kubectl patch seinetwork` set `spec.image` to the post-upgrade build. The SeiNetwork controller re-asserts the image onto every child and rolls all validators onto the new binary. |
| `await-post-upgrade-progress` | Post-upgrade height-advance check: each of nodes 0/1/2/3 advanced past `POST_UPGRADE_HEIGHT` (= `TARGET_HEIGHT + 10`) via AwaitCondition. This is the liveness assertion -- a node that crosses the boundary has survived the upgrade. |

### 5. Cleanup

```bash
kubectl delete workflow -n majorupgrade major-upgrade
# Workflow does NOT delete the SeiNodeTask CRs it created (intentional --
# you want them visible for post-mortem). Remove them explicitly:
kubectl delete seinodetasks -n majorupgrade --all
# Per-run ConfigMaps (labeled with sei.io/workflow-run) accumulate across
# runs. The Workflow does not garbage-collect them; an operator clears
# them out by label:
kubectl delete configmap -n majorupgrade -l sei.io/workflow-run
# or for a single run:
kubectl delete configmap -n majorupgrade "workflow-vars-${SEI_WORKFLOW_RUN_ID}"
# Tear down the testnet:
kubectl delete -f scenarios/testnet-deployment.yaml
```

## Cross-step variable bridge (PR 6)

Chaos Mesh Workflow Task steps are each their own Pod, so emptyDir
volumes cannot carry state across steps. The bridge is a per-run
ConfigMap named `workflow-vars-${SEI_WORKFLOW_RUN_ID}` in the same
namespace as the Workflow:

- **Producer steps** (`compute-target-height`, `resolve-proposal-id`)
  use `alpine/k8s` (curl + kubectl + jq) to compute or query values and
  `kubectl apply` the ConfigMap. `compute-target-height` creates it with
  `--from-literal` x4 and labels it `sei.io/workflow-run`; later
  producers read-modify-write via `kubectl get -o json | jq | apply`.

- **Consumer steps** receive every key as a container env var via
  `envFrom: configMapRef: name: workflow-vars-$SEI_WORKFLOW_RUN_ID`. The
  runner's `--var KEY=$(KEY)` arguments use the Kubernetes container env
  expansion (`$(VAR)`), which kubelet resolves against the env at
  container start. The runner sees concrete `--var KEY=<value>` strings
  and no longer needs to source any file.

- **Concurrency:** the ConfigMap name is parameterized on
  `$SEI_WORKFLOW_RUN_ID`, which the operator generates at apply time
  (see the `export SEI_WORKFLOW_RUN_ID=...` line above). Two concurrent
  runs of the same Workflow get distinct ConfigMaps.
  **Caveat:** `resolve-proposal-id` filters voting-period proposals by
  `content.plan.name` (= `$SEI_UPGRADE_NAME`). Running two concurrent
  scenarios on the **same chain** with the **same upgrade name** lets
  either run resolve to whichever proposal sorts first. Use a distinct
  `$SEI_UPGRADE_NAME` per concurrent run, or treat the chain as serially
  owned by one scenario at a time.

- **Cleanup:** the ConfigMap carries an `ownerReference` pointing at the
  parent Workflow CR (`major-upgrade-$SEI_WORKFLOW_RUN_ID`). Deleting the
  Workflow cascades garbage-collection of the ConfigMap automatically
  via kube-controller-manager. Operators can still clean up by label
  (`-l sei.io/workflow-run`) if multiple Workflows are torn down at once.

## Known limitations / deferred capability

1. **Liveness via post-upgrade height-advance check only.** This Workflow
   does not assert pre-upgrade running state, does not detect the panic
   directly (no RPC-down / stuck-at-`TARGET_HEIGHT-1` polling), and does
   not assert the `UPGRADE "<v>" NEEDED` log line that the source
   `major_upgrade_test.yaml` greps for. The post-upgrade height-advance
   check (each upgraded node advances past `TARGET_HEIGHT + 10` via
   AwaitCondition) is the actual liveness signal -- a node that crosses
   that boundary has by construction survived the upgrade. Explicit panic
   detection and log-line assertions are future SeiNodeTask kinds
   (`AssertLogContains`, `AwaitCondition` with a `panicked` predicate)
   that no current scenario actually requires.

2. **No chain-query task kind for proposals.** `compute-target-height`,
   `resolve-proposal-id`, and `wait-for-proposal-to-pass` are bash +
   curl against the per-pod headless Service RPC/REST. The right
   primitive is an `AwaitCondition` extension with `proposalStatus` /
   `proposalIdByPlanName` / `heightAdvancing` predicates that emits the
   resolved value to the standard outputs path. Migrating those three
   steps to a structured kind also lets us delete the `configmaps` RBAC
   verbs (only the runner's outputs ConfigMap-write would remain).

3. **Upgrade rolls the whole fleet, not staggered per-node.** This
   Workflow bumps the SeiNetwork spec.image once and lets the SeiNetwork controller
   roll all validators together. It does NOT exercise the staggered
   early-upgrade-one-node-then-the-rest path the source
   `major_upgrade_test.yaml` does. Per-child `UpdateNodeImage` against a
   SeiNetwork-owned node fights the controller's spec.image re-assertion (the child
   image flip-flops, the StatefulSet churns, `observe-image` never settles),
   so staggered rollout needs a different primitive (e.g. SeiNetwork-level
   partition/maxUnavailable) before it can return.

4. **The runner image is not yet auto-published.** Add a `runner` step to
   `.github/workflows/ecr.yml` once this scenario is wired into a CI job.

5. **Argo Workflows migration is still on the long-term roadmap.** The
   ConfigMap bridge is the MVP. Argo's `outputs.parameters` /
   `inputs.parameters` is more ergonomic and avoids the per-run
   ConfigMap garbage. Plan that migration once we have more than one
   scenario worth porting.

6. **No fan-out from a single step.** The 4-vote step is hard-coded to
   4 children rather than `--per-node-selector=role=validator`. We could
   collapse the four `vote-node-*` templates into one fan-out runner if
   the SeiNodes carry a consistent label, but the explicit per-node form
   is easier to diagnose in `kubectl describe workflownode` output.

## References

- `https://github.com/sei-protocol/bdchatham-designs/blob/main/designs/seinode-task/seinode-task-lld.md` -- the canonical interface contract.
- `runner/rbac.yaml` -- RBAC the workflow expects on its ServiceAccount.
- `runner/templates/*.yaml.tmpl` -- the templates the runner ships.
- `sei-chain/integration_test/upgrade_module/major_upgrade_test.yaml` --
  the north-star scenario this Workflow replicates.
