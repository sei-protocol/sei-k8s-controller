# sei-k8s-controller

Kubernetes operator for managing Sei blockchain nodes. Single binary, three controllers: `SeiNetwork` (genesis-ceremony orchestration: bootstraps a chain's genesis.json and founding validator set, owns the child SeiNodes), `SeiNode` (individual node lifecycle), and `SeiNodeTask` (sidecar-driven task execution).

## Architecture

- **API group**: `sei.io/v1alpha1`
- **CRD types**: `SeiNetwork`, `SeiNode`, `SeiNodeTask` (defined in `api/v1alpha1/`)
- **Controllers**: `internal/controller/seinetwork/`, `internal/controller/node/`, `internal/controller/nodetask/`
- **Entry point**: `cmd/main.go` — thin binary that creates a `manager.Manager` and registers both controllers
- **Framework**: controller-runtime v0.23.1 / kubebuilder v4.12.0

## Subagents

Always use the available subagents for relevant work:
- **kubernetes-specialist**: Use for all Kubernetes design, deployment, troubleshooting, and operational decisions. Consult before making changes to CRDs, RBAC, kustomize configs, StatefulSet specs, or any cluster-facing resource definitions.
- **platform-engineer**: Use for architectural decisions about platform composability, developer experience, CI/CD workflows, and infrastructure abstraction patterns.
- **idiomatic-reviewer**: Use to review code changes for idiomatic conformance to Go, controller-runtime, and this repo's own documented patterns. It digests this CLAUDE.md + the package's `doc.go` into a local idiom profile that outranks generic idiom, then gives two-altitude feedback (design + surgical), each finding cited. Reviews for idiom; it does not author the system (that's kubernetes-specialist). Backed by the `/idiomatic` skill.

## Code Standards

### Go
- Follow idiomatic Go. No unnecessary abstractions — three similar lines are better than a premature helper.
- Use `controller-runtime` patterns: every controller exports a reconciler struct with `SetupWithManager(mgr)`.
- All code must pass `golangci-lint` (config in `.golangci.yml`). Fix lint issues, don't suppress them.
- Imports must be grouped: stdlib, external, then `github.com/sei-protocol/sei-k8s-controller` (enforced by goimports).
- No `panic` in controller code. Return errors and let the reconciler retry.
- Keep reconcile loops idempotent — every reconcile should converge toward desired state regardless of current state.

### Status patches

Status writes must use **optimistic concurrency** so a stale reconcile cannot silently overwrite a fresher one. Two near-simultaneous reconciles can both observe `status.plan == nil`, both build a plan, and without resourceVersion-checked patches the second silently wins — corrupting plan-creation idempotency.

**Use:**

```go
patch := client.MergeFromWithOptions(obj.DeepCopy(), client.MergeFromWithOptimisticLock{})
// ... mutate obj.Status ...
if err := r.Status().Patch(ctx, obj, patch); err != nil { ... }
```

**Do not use** for status writes:
- `client.MergeFrom(...)` without the `MergeFromWithOptimisticLock{}` option — produces a merge patch with no resourceVersion precondition; stale writes succeed silently.
- `client.Status().Update(...)` without resourceVersion verification on the in-memory object.
- `client.Apply` (server-side apply) on `.status` without explicit resourceVersion handling — field-manager isolation does not by itself prevent stale-write races for our plan-creation invariant.

The single-patch reconcile model means each reconcile snapshots `obj.DeepCopy()` once, accumulates mutations in-memory, and flushes one optimistic-lock-protected `Status().Patch` at the end. Code-review checklist item: every `r.Status().Patch` call site must use a base built with `MergeFromWithOptimisticLock{}`.

### Conditions

Every `metav1.Condition` on `SeiNetwork`, `SeiNode`, or `SeiNodeTask` follows the Kubernetes upstream pattern: **once a controller sets a condition, it stays present on the reconciled object**, transitioning between `True` / `False` / `Unknown` with a stable `Reason` and a `lastTransitionTime`. This is the default. It matches Pod (`Ready`, `Initialized`, `ContainersReady`, `PodScheduled` — stably present once the kubelet has begun processing the pod), Deployment (`Available`, `Progressing` — stably present once the Deployment reconciles), Gateway-API HTTPRoute (`Accepted`, `ResolvedRefs` — stably present per ParentRef once the implementation reconciles it), and CAPI Cluster/Machine (`Ready`, `InfrastructureReady` — stably present after first reconcile). Even "feature off" or "feature broken" states are expressed as `Status=False, Reason=<stable enum value>` — never as absence.

**Use:**

```go
setCondition(obj, ConditionNetworkingReady, metav1.ConditionFalse,
    "NetworkingDisabled", "spec.networking is unset")
```

**Do not use** for steady-state transitions:

- `removeCondition(obj, ...)` to express "feature is off." Use `setCondition(False, <reason>)` instead. Removal is indistinguishable in `kubectl describe` from "controller never reached this code path" and forces PromQL consumers into brittle `absent()` queries.

The narrow exceptions to the always-present rule:

- **`*Needed`-style conditions** where `True` is the exception and `False` would be tautological with the absence of the feature. No current instances in this codebase; the exception is retained for future conditions where it genuinely fits.
- **`kubectl wait` consumer conditions** where present-vs-absent semantics are explicitly load-bearing. `SeiNodeTask.Status.Conditions[Ready|Failed]` is documented as latch-on-terminal-state because the seitask-runner depends on `kubectl wait --for=condition=Ready=true` (which matches `True` only) and `--for=condition=Failed=true` as the dual exit signal. The Ready+Failed pair is the documented exception to the "no mixed polarities for the same subject" rule below — both latch independently on terminal state.

Any new condition that doesn't fit one of these exceptions defaults to always-present.

**Naming:**

- **`<Subject>Ready`** — `True` is the desired steady state. Always-present. Use `False/<reason>` for both "not yet ready" and "not configured."
- **`<Subject>InProgress`** — `True` is the exception, `False` is steady state. Always-present. Seed `False` on first reconcile.
- **`<Subject>Complete`** — latch-True. Always-present. Seed `False/NotStarted` for discoverability.
- **`<Subject>Needed`** — absent-when-not-applicable acceptable.

Don't mix polarities for the same subject (no `XReady` + `XFailed` — pick one and use reasons). The SeiNodeTask `Ready`/`Failed` pair is the documented exception, justified by the `kubectl wait` consumer contract.

**Spec-shape changes don't remove conditions.** If a SeiNode transitions from validator to non-validator (or any condition's preconditions become structurally inapplicable), set the condition to `False/NotApplicable` rather than removing it. Removal forces consumers to treat absence as ambiguous; an explicit `NotApplicable` reason carries the intent.

**Reasons are a stable enum.** Treat `Reason` as the public API for runbooks and alerting. Use `CamelCase` value strings. Don't put dynamic data in the reason — that goes in the message. PromQL keyed on `kube_..._status_condition{type="X", reason="Y", status="..."}` should yield a small, finite set of label combinations.

**`ObservedGeneration` discipline.** Every `setCondition` call site must populate `condition.ObservedGeneration = obj.Generation` so consumers can tell whether a condition reflects the current spec. The `setCondition` helpers in each controller's `status.go` do this automatically; direct `apimeta.SetStatusCondition` calls must set it explicitly. The four direct calls in `internal/controller/nodetask/controller.go` are a known divergence; harmonize on first edit through that file.

### Testing
- Tests use `testing` + `gomega` for assertions.
- Test fixtures for platform config live in `internal/platform/platformtest/`.
- Run tests with `make test` before submitting changes.

### CRD Changes
- Edit types in `api/v1alpha1/` (e.g., `seinode_types.go`, `seinetwork_types.go`, `validator_types.go`).
- After any type change, run `make manifests generate` to regenerate CRD YAML and DeepCopy methods.
- Never hand-edit files in `manifests/` or `zz_generated.deepcopy.go`.
- When changing `SeiNodeTask` kinds or their operational behavior, update `https://github.com/sei-protocol/bdchatham-designs/blob/main/designs/seinode-task/seinode-task.md`. Its section headings are **cited anchors** for the gov-ops skill (PLT-489) — renaming one is a breaking change for that consumer.

### RBAC
- RBAC is generated from `// +kubebuilder:rbac:` markers on controller files.
- After changing markers, run `make manifests` to regenerate `manifests/role.yaml`.

## Build & Deploy

```bash
make build                    # Build the manager binary
make test                     # Run unit tests
make lint                     # Run golangci-lint
make manifests generate       # Regenerate CRDs, RBAC, DeepCopy after type changes
make docker-build IMG=<image> # Build container image
make docker-push IMG=<image>  # Push container image
```

## Key Patterns

- **SeiNetwork** creates and owns **SeiNode** resources. It orchestrates the genesis ceremony, manages deployments, and coordinates networking/monitoring.
- **SeiNode** creates StatefulSets (replicas=1), headless Services, and PVCs via server-side apply (fieldOwner: `seinode-controller`).
- **Plan-driven reconciliation** — Both controllers use ordered task plans (stored in `.status.plan`) to drive lifecycle. Plans are built by `internal/planner/` (`ResolvePlan` for nodes, `ForGroup` for deployments), executed by `planner.Executor`, with individual tasks in `internal/task/`. The reconcile loop is: `ResolvePlan → persist plan → ExecutePlan`. See `internal/planner/doc.go` for the full plan lifecycle.
- **Init plans** transition nodes from Pending → Running. They include infrastructure tasks (`ensure-data-pvc`, `apply-statefulset`, `apply-service`) followed by sidecar tasks (`configure-genesis`, `config-apply`, etc.).
- **NodeUpdate plans** roll out image changes on Running nodes. Built when `spec.image != status.currentImage`. Tasks: `apply-statefulset`, `apply-service`, `replace-pod` (proactively deletes pods at the old StatefulSet revision so the rollout proceeds even when seid is intentionally unready, e.g. halted at a chain upgrade height), `observe-image` (polls StatefulSet rollout, stamps `currentImage`), `mark-ready` (sidecar re-init). The planner sets `NodeUpdateInProgress` condition on creation and clears it on completion/failure. When no drift is detected, no plan is built — the node sits in steady state.
- **Atomic plan creation** — New plans are persisted before any tasks execute. The reconciler flushes the plan, then requeues. Execution starts on the next reconcile. This guarantees external observers see the plan before side effects occur.
- **Condition ownership** — The planner owns all condition management on the owning resource. It sets conditions when creating plans (e.g., `NodeUpdateInProgress=True`) and when observing terminal plans (e.g., `NodeUpdateInProgress=False`). The executor does not set conditions — it only mutates plan/task state and phase transitions.
- **Single-patch model** — All status mutations (plan state, conditions, phase, currentImage) accumulate in-memory during a reconcile and are flushed in a single `Status().Patch()` at the end. Tasks mutate owned resources (StatefulSets, Services, PVCs); the executor mutates plan state in-memory; the reconciler flushes once.
- **Resource generators** live in `internal/noderesource/` — pure functions that produce StatefulSets, Services, and PVCs from a SeiNode spec. Used by both the controller and plan tasks.
- **Platform config** is resolved by `platform.Load` (`internal/platform/load.go`). Infra fields (scheduling, storage, resources, snapshot/genesis/result-export buckets, images) come from the mounted app-config file (`SEI_CONTROLLER_CONFIG` → `platform.FileConfig`), which is authoritative — a required field unset in the file fails `Config.Validate` at startup. Networking/gateway fields (`SEI_GATEWAY_*`, `SEI_P2P_ENDPOINT_DOMAIN`, `SEI_NLB_TARGET_TYPE`) stay env-sourced pending their removal from the controller in PLT-451. The file is read once at startup for infra fields (an infra change needs a restart); the `stateSync` section is re-read per reconcile (it hot-reloads). See `internal/platform/platform.go` for the field list and the [controller-app-config schema](https://github.com/sei-protocol/bdchatham-designs/blob/main/designs/controller-app-config/controller-app-config.md) (in bdchatham-designs — relocated per Design 05 / PLT-497) for the file schema.
- **Genesis resolution** is handled by the sidecar autonomously: embedded sei-config for well-known chains, S3 fallback at `{SEI_GENESIS_BUCKET}/{chainID}/genesis.json` for custom chains.
- Config keys in seid's `config.toml` use **hyphens** (e.g., `persistent-peers`, `trust-height`), not underscores.
