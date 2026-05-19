# Scenarios

End-to-end Chaos Mesh Workflows that compose `SeiNodeTask` CRs to exercise
chain-lifecycle behavior against real Kubernetes clusters. Each scenario is
the acceptance test for one capability surface.

> **Status: not yet runnable end-to-end.** `major-upgrade.yaml` is the
> canonical structure for the major-upgrade scenario but depends on a
> cross-step variable bridge that does not exist in Chaos Mesh's `Task`
> template (each step is its own Pod; emptyDir volumes are Pod-scoped, not
> Workflow-scoped). The `TARGET_HEIGHT` / `UPGRADE_HEIGHT` /
> `POST_UPGRADE_HEIGHT` / `PANIC_BOUNDARY` / `PROPOSAL_ID` values computed
> by early steps are not readable by subsequent steps without a bridge.
>
> The YAML retains `/workflow/vars/env.sh` references and the `vars`
> volume mounts on every step for forward-compatibility with that
> bridge. A ConfigMap-based implementation will land in a separate PR
> (follow-up issue TBD). Until then, this Workflow serves as the design
> artifact and is **schema-validated only**:
>
> ```bash
> kubectl apply -f scenarios/major-upgrade.yaml --dry-run=client -o yaml > /dev/null
> ```

## Index

| File | Mirrors | Purpose |
|---|---|---|
| `major-upgrade.yaml` | `sei-chain/integration_test/upgrade_module/major_upgrade_test.yaml` | 4-validator software-upgrade flow with early-upgrade panic, at-height panic, downgrade-and-catchup, and final convergence. MVP acceptance for the SeiNodeTask CRD. |
| `testnet-deployment.yaml` | n/a | Reference 4-validator `SeiNodeDeployment` the Workflow can target. |

## Where this runs

These scenarios are **destructive**. They submit governance proposals, mutate
SeiNode images, and drive validators through CrashLoop states. They are
designed for:

- The **harbor dev cluster** (`harbor-dev` EKS). Ephemeral testnets only.
- Local `kind`/`minikube` clusters with the controller installed.
- **Not** any cluster carrying a chain you care about.

The Workflow does not provision the chain. It assumes a 4-validator
`SeiNodeDeployment` exists in the target namespace before the Workflow
applies. See "Run" below.

## Prerequisites

1. **CRDs installed** in the target cluster:
   - `seinodedeployments.sei.io`
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
kubectl -n majorupgrade wait --for=condition=Ready=true \
  --timeout=15m seinodedeployment/majorupgrade
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
| `compute-target-height` | Wrote `TARGET_HEIGHT` / `UPGRADE_HEIGHT` / `POST_UPGRADE_HEIGHT` to env.sh. |
| `submit-upgrade-proposal` | SeiNodeTask `.status.phase=Complete`, `proposalId` set. |
| `vote-yes-all-validators` | All 4 vote tasks Complete. |
| `wait-for-proposal-to-pass` | Proposal observed `PROPOSAL_STATUS_PASSED`. |
| `early-upgrade-node-0` | SeiNode status.currentImage observed equal to post-upgrade image (NOT readiness -- see LLD). |
| `wait-for-target-height-nodes-1-2-3` | Sidecar AwaitNodesAtHeight observed local height >= `TARGET_HEIGHT` on each of nodes 1/2/3. |
| `upgrade-nodes-1-2-3` | Image patch landed on each (same semantics as early-upgrade). |
| `await-post-upgrade-progress-nodes-1-2-3` | Post-upgrade height-advance check: each of nodes 1/2/3 advanced past `POST_UPGRADE_HEIGHT` (= `TARGET_HEIGHT + 10`) via AwaitCondition. This is the liveness assertion. |
| `downgrade-node-0` | Image reverted to pre-upgrade (same semantics as upgrade). |
| `wait-for-target-height-node-0` | Node-0 caught up to `TARGET_HEIGHT - 1` (will panic at `TARGET_HEIGHT` on the pre-upgrade binary). |
| `upgrade-node-0` | Final image patch to post-upgrade on node-0. |
| `await-post-upgrade-progress-node-0` | Post-upgrade height-advance check on node-0 past `POST_UPGRADE_HEIGHT`. Final liveness assertion. |

### 5. Cleanup

```bash
kubectl delete workflow -n majorupgrade major-upgrade
# Workflow does NOT delete the SeiNodeTask CRs it created (intentional --
# you want them visible for post-mortem). Remove them explicitly:
kubectl delete seinodetasks -n majorupgrade --all
# Tear down the testnet:
kubectl delete -f scenarios/testnet-deployment.yaml
```

## Known limitations / deferred capability

The cross-step variable bridge gap (load-bearing for end-to-end runs) is
called out in the **Status** block at the top of this README. Three viable
bridges, in increasing implementation cost:

- **(a) ConfigMap-as-env-store.** A `<workflow>-vars` ConfigMap created
  alongside the Workflow. Each step mounts it at `/workflow/vars/`.
  Producer steps `kubectl patch configmap` to add KEY=value entries. The
  runner image must grow `kubectl` or learn to write outputs to a
  ConfigMap via the K8s API. RBAC additions: `configmaps: get;watch;patch`
  on the runner SA. Tradeoff: mounted ConfigMap updates take up to
  kubelet sync interval (60s default) to appear -- acceptable because
  the next Pod starts AFTER the previous Pod finishes and will see the
  snapshot at its own mount time.
- **(b) Argo Workflows native parameters.** Migrate scenarios from Chaos
  Mesh Workflow to Argo. Argo has first-class `outputs.parameters` /
  `inputs.parameters`. Chaos resources stay, but composition moves to
  Argo. Larger lift; correct long-term.
- **(c) Custom Workflow extension.** Patch Chaos Mesh to add
  workflow-scoped volume claims. Upstream PR territory.

Recommendation: ship (a) as the immediate follow-up PR. Plan (b) in
parallel for the post-MVP roadmap.

Other deferred items:

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

2. **No chain-query task kind for proposals.** `compute-target-height`
   and `wait-for-proposal-to-pass` are bash + curl against the per-pod
   headless Service RPC/REST. The right primitive is an `AwaitCondition`
   extension with `proposalStatus` / `heightAdvancing` predicates.

3. **`UpdateNodeImage` completes on image-applied, not Ready.** Required
   by this scenario (early-upgrade is expected to CrashLoop), but
   surprising for happy-path users. Documented on the kind's CRD
   description.

4. **The runner image is not yet auto-published.** Add a `runner` step to
   `.github/workflows/ecr.yml` once this scenario is wired into a CI job.

5. **No native Workflow parameter passing.** Chaos Mesh 2.5/2.6/2.7 do not
   pass values across `Task` steps -- we use the emptyDir env-file bridge
   at `/workflow/vars/env.sh`. Future Argo migration replaces this with
   `outputs.parameters` / `inputs.parameters`.

6. **No fan-out from a single step.** The 4-vote step is hard-coded to
   4 children rather than `--per-node-selector=role=validator`. We could
   collapse the four `vote-node-*` templates into one fan-out runner if
   the SeiNodes carry a consistent label, but the explicit per-node form
   is easier to diagnose in `kubectl describe workflownode` output.

## References

- `docs/design/seinode-task-lld.md` -- the canonical interface contract.
- `runner/rbac.yaml` -- RBAC the workflow expects on its ServiceAccount.
- `runner/templates/*.yaml.tmpl` -- the templates the runner ships.
- `sei-chain/integration_test/upgrade_module/major_upgrade_test.yaml` --
  the north-star scenario this Workflow replicates.
