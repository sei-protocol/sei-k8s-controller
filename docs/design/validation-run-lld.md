# ValidationRun — Composable Validation Workloads against Ephemeral Sei Chains (LLD)

## Status

Draft — under council review (`sei-protocol/sei-k8s-controller#139`). Council gate closed 2026-04-28. Architectural refinement (sub-controller rails) added 2026-04-29. Cross-review pending; PR #143 open.

This LLD is the substrate for one or more handoff implementation issues. It does **not** itself authorize implementation — it is the artifact the gate reviews.

## Problem

Three differently-shaped validation workloads coexist on the Harbor cluster today with no shared abstraction: the seiload nightly performance run, the qa-testing TS/mocha suite (Phase 1 target), and the in-pod `SeiNode.spec.replayer` shadow-result sidecar. Only the third is first-class on the control plane; the first two are bash glue around `kubectl`, `envsubst`, `kubectl wait`, `kubectl exec seid status`, `aws s3 cp`. The bash pattern's documented sharp edges (missing `OwnerReferences`, no heartbeat, no structured report, race against `sei-chain` CI handled in shell) are symptoms of orchestrating outside the controller boundary; they don't grow into an abstraction, they compound as more workload shapes (fuzzers, soak, chaos suites, pectra-upgrade tests) come online.

Critically, "validation" in this context isn't just "did the workload exit 0." Many failure modes are infrastructure-level invariants the workload can't see: validators dropped peer connections mid-run, `seid` memory grew unbounded, an alert fired during the load window. The current pattern can't express **observability-as-test-oracle** — composing workload signal with alert/PromQL signal into a single pass/fail.

A second axis of expansion is also already visible: workloads and sequences and chaos injection all want to operate against the same ephemeral chain substrate, but they are different actors with different lifecycles. Hard-coding "ValidationRun owns one Job" forecloses on sequencing (apply N steps in order) and chaos (inject failures during the window). The architecture below treats the chain substrate as one concern and the actors as composable, additively-extensible sub-controllers.

See `sei-protocol/sei-k8s-controller#139` for the full problem statement, OSS survey, and Phase 1 contract dependency. This LLD picks up where #139's open questions left off, integrating Round 1 specialist input, the Round 2 user gate decisions through 2026-04-28, and the Round 3 architectural-refinement pass through 2026-04-29.

## Goals

1. **One CRD, two cooperating sub-controllers in one binary that drive a validation Run to a terminal verdict against an ephemeral chain.** ValidationOrchestrationReconciler always runs; ValidationLoadGenerationReconciler is the v1 actor; ValidationSequenceReconciler and ValidationChaosReconciler are designed-as-extension and gated off by default. The CR carries one plan per controller in `.status.plans.<controllerName>`.
2. **Workload contract parity with Phase 1** — `ValidationRun.spec.load.workload` adopts the env-vars / exit-codes / termination-message / S3 contract from `sei-protocol/platform#235` verbatim. A Job manifest that runs under Phase 1 GHA glue runs unchanged under the controller.
3. **Observability-as-test-oracle** — the verdict is `workload_exit_code AND ⋀rules`, where rules are typed (`alert` and `query` in v1) and evaluated continuously over the load window with a deterministic, auditable Prometheus contract. v1 ships continuous polling with stop-on-failure as a first-class behavior. Rules-only Runs (no `load`) are valid: the Run becomes passive monitoring of an existing chain.
4. **GitOps- and ad-hoc-friendly** — a `ValidationRun` is a self-contained, idempotent resource. Apply from GHA, Flux, kubectl, or a future bot; the controllers take it to a terminal phase exactly once.
5. **No cross-tenant blast radius** — same-namespace by construction (CEL-enforced) with one narrow exception (`alert.ruleRef` into namespaces labeled `sei.io/validation-shared-rules=true`). Tenants pre-provision SAs; controller is never an IAM controller.
6. **v2 actor expansion is purely additive.** Adding sequence or chaos = registering a new sub-controller behind a deployment-time opt-in, plus stamping a new plan slot on `.status.plans`. No refactor of ValidationOrchestrationReconciler or ValidationLoadGenerationReconciler.

## Non-goals

Adopted from #139 "Out of scope" plus the Round 1 + user refinements:

- **`ValidationSuite` and `ValidationSchedule` controllers.** Kind names are reserved (CRDs install with v1) and the types are sketched here so v1 `ValidationRun` doesn't paint them into a corner. No reconcilers in v1.
- **`SequenceSpec` and `ChaosSpec` reconcilers.** Field names reserved on `ValidationRunSpec` so the composable-block union does not have to change shape later. Admission-rejected at v1 until the sub-controllers register.
- **`IntegrationTest` as a separate kind/discriminator.** The composable-blocks model collapses it: "stand up a chain + run a single container that validates" is just `load + rules` with the workload acting as a verifier. No separate kind needed.
- **`container` rule type.** Reserved in the rule schema discussion; not in v1. Re-defer until a real heuristic ships outside Prometheus.
- **`window-end` and `edge` rule modes.** v1 ships `continuous` semantics via the polling loop. Mode field collapses to a single behavior in v1 (no `mode` field on the wire); expansion is additive.
- **Shadow-replayer migration.** Stays typed on `SeiNode.spec.replayer.resultExport.shadowResult`. Different shape.
- **Kueue admission control / multi-tenant fairness.** Defer until ≥3 suite-consumers contend.
- **DAG orchestration in `ValidationSuite`.** Flat sequential / parallel / `stopOnFailure` only when implemented.
- **Run-vs-Template split / `ValidationDefinition` CR.** Embed inline; reserve `spec.runRef` field name only.
- **Per-PR triggers, regression diffing, structured-report registry, multi-workload composition, mid-run RPC fleet refresh.** All deferred per #139.
- **Pushgateway / Prometheus push of report metrics.** v1 pattern is `.status.report.s3Url` only — S3 is authoritative. Aggregation lives downstream.
- **The controller minting ServiceAccounts or Pod Identity Associations.** Tenant pre-provisions. Forever.
- **`spec.status.report.raw` (or any termination-message echo into status).** Cut at gate; consumers fetch from the S3 URL.
- **`pods/exec` for sequence steps.** Rejected one-way door — when ValidationSequenceReconciler ships, transactions submit via short-lived `Job`s targeting RPC, not `kubectl exec` into validator pods.

## Architecture overview

The headline shift from earlier drafts: ValidationRun is reconciled by **two cooperating controllers in one binary**, each owning a distinct slice of the lifecycle and writing a distinct slice of `.status`. v2 actor controllers register identically alongside.

```
┌──────────────────────────────────────────────────────────────────────────┐
│                        ValidationRun (CR)                                │
│  spec.chain.deployments[]   spec.load   spec.rules[]   spec.timeouts     │
│  status.phase   status.verdict   status.chain   status.rules[]           │
│  status.report   status.workloadExitCode                                 │
│  status.plans.orchestration   status.plans.loadGeneration                │
│  status.conditions[TestRunning, TestComplete, Succeeded,                 │
│                    TestCancelled, LoadComplete]                          │
└──────────────────────────────────────────────────────────────────────────┘
            ▲                                       ▲
            │ Reconcile()                           │ Reconcile() (predicate-gated)
            │                                       │
┌───────────┴──────────────┐         ┌──────────────┴────────────┐
│ ValidationOrchestrationReconciler  │  ──IPC──▶ ValidationLoadGenerationReconciler  │
│ (required if validation  │  via    │ (opt-in; default-on within │
│  is enabled)             │  status │  validation slice)         │
│ ControllerName=          │  status │ ControllerName=           │
│   "validationrun-        │  conds  │   "validationrun-         │
│    orchestration"        │  only   │    loadgeneration"        │
│                          │         │                           │
│ Owns: SND children       │         │ Owns: Job, ConfigMap      │
│ Plan slot:               │         │ Plan slot:                │
│   .status.plans.         │         │   .status.plans.          │
│      orchestration       │         │      loadGeneration       │
│ Field manager:           │         │ Field manager:            │
│   validationrun-         │         │   validationrun-          │
│    orchestration         │         │    loadgeneration         │
│ Writes:                  │         │ Writes:                   │
│  Conditions[TestRunning, │         │  Conditions[LoadComplete] │
│             TestComplete,│         │  status.workloadExitCode  │
│             Succeeded,   │         │  status.plans.            │
│             TestCancelled│         │     loadGeneration        │
│            ],            │         │                           │
│  status.chain.*,         │         │                           │
│  status.report.*,        │         │                           │
│  status.rules[],         │         │                           │
│  status.phase,           │         │                           │
│  status.verdict,         │         │                           │
│  status.plans.           │         │                           │
│     orchestration        │         │                           │
└──────────────────────────┘         └───────────────────────────┘
            │                                       │
            ▼                                       ▼
   SeiNodeDeployments                     batch/v1.Job + ConfigMap
   (one per spec.chain.deployments[i])    workload pod(s)
   role=validator | fullNode              envs from contract
            │                                       │
            └─── owns SeiNodes ───┐                 │
                                  ▼                 ▼
                    Prometheus /api/v1/query   S3 upload via Pod Identity
                    (per rule per interval)
```

The two sub-controllers communicate **only through the CR's `.status`**. There is no direct in-process API between them; both watch the same CR and react to condition flips. This keeps each controller individually testable, individually opt-out-able by deployment configuration, and gives v2 actor sub-controllers a clean integration point.

**Phase machine** (mirrors Tekton/Argo, capitalized — *not* Testkube's lowercase):

```
                         ┌──────────────┐
            ┌────────────│   Pending    │
            │            └──────┬───────┘
            │                   │ ValidationOrchestrationReconciler builds plan,
            │                   │ persists, sets phase=Running on persist
            │                   ▼
            │            ┌──────────────┐
            │            │   Running    │── Orchestration plan tasks ──┐
            │            └──────┬───────┘   (and, gated by             │
            │                   │            Conditions[TestRunning],  │
            │                   │            LoadGeneration tasks)     │
            │       ┌───────────┼─────────────┬────────────┐           │
            │       ▼           ▼             ▼            ▼           │
            │  ┌─────────┐ ┌─────────┐  ┌─────────┐  ┌─────────┐       │
            └─▶│Cancelled│ │Succeeded│  │ Failed  │  │  Error  │◀──────┘
               └─────────┘ └─────────┘  └─────────┘  └─────────┘
```

Terminal phases: `Succeeded` / `Failed` / `Error` / `Cancelled`. `Failed` = workload or rules said SUT misbehaved (test verdict). `Error` = controller couldn't ask (Prometheus 5xx, Job-create denial, infra failure that the workload signaled with exit code 2). `Cancelled` = `metadata.deletionTimestamp != nil` OR `Conditions[TestCancelled]=True` (terminal-failure short-circuit). The distinction matters for heartbeat alerting and for retry policy if/when added. **Phase is owned exclusively by ValidationOrchestrationReconciler**; ValidationLoadGenerationReconciler never sets phase, only conditions.

## CRD types

All three kinds (`ValidationRun`, `ValidationSuite`, `ValidationSchedule`) live in `api/v1alpha1/` (group `validation.sei.io`, version `v1alpha1`). All three are namespaced.

### `ValidationRun` — the load-bearing kind

```go
// validationrun_types.go

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vr
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Chain",type=string,JSONPath=`.spec.chain.chainId`
// +kubebuilder:printcolumn:name="Started",type=date,JSONPath=`.status.startTime`
// +kubebuilder:printcolumn:name="Duration",type=string,JSONPath=`.status.duration`
// +kubebuilder:printcolumn:name="Verdict",type=string,JSONPath=`.status.verdict`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ValidationRun struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   ValidationRunSpec   `json:"spec,omitempty"`
    Status ValidationRunStatus `json:"status,omitempty"`
}
```

#### `ValidationRunSpec` — composable optional blocks (no discriminator)

```go
// +kubebuilder:validation:XValidation:rule="has(self.load) || has(self.sequence) || has(self.chaos) || (has(self.rules) && size(self.rules) > 0)",message="at least one of spec.load, spec.sequence, spec.chaos must be set, or spec.rules must be non-empty (rules-only Runs are valid as passive chain monitoring)"
// +kubebuilder:validation:XValidation:rule="!has(self.sequence)",message="spec.sequence is reserved for v2; the ValidationSequenceReconciler is not registered in this controller version"
// +kubebuilder:validation:XValidation:rule="!has(self.chaos)",message="spec.chaos is reserved for v2; the ValidationChaosReconciler is not registered in this controller version"
type ValidationRunSpec struct {
    // Chain is the ephemeral Sei chain this run executes against. Required.
    Chain ChainSpec `json:"chain"`

    // Rules is the set of typed validation rules evaluated alongside any actor
    // (load/sequence/chaos) over the run window. Pass = ⋀(rule.Passed) AND
    // (workload exit 0 if load is set). A rules-only Run (rules but no actor)
    // becomes passive monitoring of an existing chain.
    // +optional
    // +listType=map
    // +listMapKey=name
    // +kubebuilder:validation:MaxItems=32
    Rules []ValidationRule `json:"rules,omitempty"`

    // Results configures where artifacts are uploaded. Workloads upload via
    // Pod Identity; the controller stamps `status.report.s3Url`.
    // +optional
    Results *ResultsSpec `json:"results,omitempty"`

    // Timeouts bound the run's wall-clock at named lifecycle points.
    // +optional
    Timeouts *RunTimeouts `json:"timeouts,omitempty"`

    // ----- Composable optional actors. Multiple may coexist
    //       (e.g., load + sequence; chaos + load).

    // Load is the v1 actor — a containerized load generator owned by the
    // ValidationLoadGenerationReconciler. Reconciler-gated; field is admissable in v1.
    // +optional
    Load *LoadSpec `json:"load,omitempty"`

    // Sequence is reserved for v2 — an ordered list of state-change steps
    // (governance proposal, validator-set churn, IBC bring-up). Admission
    // rejects spec.sequence in v1 until ValidationSequenceReconciler ships.
    // +optional
    Sequence *SequenceSpec `json:"sequence,omitempty"`

    // Chaos is reserved for v2 — a chaos-mesh-driven fault injection actor.
    // Admission rejects spec.chaos in v1 until ValidationChaosReconciler ships AND
    // tenant namespace carries label sei.io/chaos-allowed=true AND controller
    // is built with --enable-chaos-plan.
    // +optional
    Chaos *ChaosSpec `json:"chaos,omitempty"`
}
```

##### Composable-blocks rationale

The earlier draft used a single discriminator `spec.type ∈ {LoadTest, SequenceTest, IntegrationTest}` with a body wrapper per kind. Three problems surfaced:

1. **Sequence + load + chaos compose naturally.** Real test shapes mix actors: "submit a governance proposal (sequence), then drive load (load), then check rules (rules)." A discriminator forces separate Runs and an external orchestrator; composable blocks let one Run carry all three actors against one chain.
2. **`IntegrationTest` collapses into `load + rules`.** "Stand up a chain, run one container that validates" is identical to a load actor whose Job just performs assertions instead of generating load. Splitting it into a separate kind was duplication.
3. **The discriminator wrapper produced a one-way door.** If `spec.type` is enum-locked, adding "load + sequence in one Run" later requires either a fourth discriminator value or a schema migration. Composable blocks expand additively.

CEL admission keeps the safety net: at least one actor block OR rules-only must be set. v2 reconcilers register additively. v1's two reserved blocks (`sequence`, `chaos`) are CEL-rejected at admission so users see a deterministic error rather than silently-ignored fields.

#### `ChainSpec` — list of named SeiNodeDeployment configs

The chain substrate is one or more `SeiNodeDeployment` children (one per entry in `chain.deployments[]`). Each entry names a deployment, declares its role (`validator` or `fullNode`), and embeds the full `SeiNodeDeploymentSpec` shape — operators write the same `template.spec.image / entrypoint / overrides / dataVolume` they already know from the existing nightly template.

```go
// +kubebuilder:validation:XValidation:rule="self.chainId.matches('^[a-z0-9][a-z0-9-]*[a-z0-9]$')",message="chainId must be lowercase alphanumeric with hyphens"
// +kubebuilder:validation:XValidation:rule="self.deployments.exists_one(d, d.role == 'validator')",message="exactly one deployment with role=validator is required"
// +kubebuilder:validation:XValidation:rule="self.deployments.exists(d, d.role == 'fullNode')",message="at least one deployment with role=fullNode is required"
// +kubebuilder:validation:XValidation:rule="self.deployments.all(d, d.role != 'fullNode' || !has(d.spec.genesis))",message="role=fullNode deployments must not declare genesis (full nodes inherit genesis from the validator ceremony)"
type ChainSpec struct {
    // ChainID for the ephemeral chain. The controller injects this into
    // every materialized SND's genesis and into every SeiNode's chainId.
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=64
    ChainID string `json:"chainId"`

    // Deployments is the list of named SeiNodeDeployment configs that
    // constitute this chain. Exactly one must have role=validator (the
    // genesis-ceremony fleet); at least one must have role=fullNode (the
    // RPC surface workloads connect to). Names are unique within the list.
    //
    // The controller materializes one SeiNodeDeployment per entry, named
    // {chainId}-{deployments[i].name}, with OwnerReferences to the
    // ValidationRun. Cascade-delete works.
    //
    // +listType=map
    // +listMapKey=name
    // +kubebuilder:validation:MinItems=2
    // +kubebuilder:validation:MaxItems=8
    Deployments []ChainDeployment `json:"deployments"`
}

// +kubebuilder:validation:XValidation:rule="self.role != 'validator' || !has(self.spec.template.spec.fullNode)",message="role=validator deployment must not set template.spec.fullNode"
// +kubebuilder:validation:XValidation:rule="self.role != 'fullNode' || !has(self.spec.template.spec.validator)",message="role=fullNode deployment must not set template.spec.validator"
type ChainDeployment struct {
    // Name is unique within deployments[]. DNS-label. Used as the suffix
    // on the materialized SND name and as the value of the
    // sei.io/deployment-name label on owned objects.
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=63
    // +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
    Name string `json:"name"`

    // Role discriminates the deployment's purpose for genesis-ceremony
    // wiring and default peer-selector injection.
    // +kubebuilder:validation:Enum=validator;fullNode
    Role ChainDeploymentRole `json:"role"`

    // Spec is the embedded SeiNodeDeploymentSpec. Controller injects:
    //  - genesis.chainId = chainId (validator role only; if unset)
    //  - template.spec.chainId = chainId (if unset)
    //  - template.spec.validator: {} (validator role; if unset)
    //  - template.spec.fullNode: {} (fullNode role; if unset)
    //  - default peers: validators peer by sei.io/chain-id label;
    //    fullNodes peer by sei.io/nodedeployment label pointing at
    //    the validator-role SND. Injected when user omits peers.
    //  - labels: sei.io/managed-by=validationrun, sei.io/run-id=<runName>,
    //    sei.io/deployment-name=<name>, sei.io/chain-id=<chainId>
    //
    // User-set fields take precedence on every field except the role
    // discriminator and chainId — those two are controller-owned.
    Spec sei_v1alpha1.SeiNodeDeploymentSpec `json:"spec"`
}

// +kubebuilder:validation:Enum=validator;fullNode
type ChainDeploymentRole string

const (
    ChainDeploymentRoleValidator ChainDeploymentRole = "validator"
    ChainDeploymentRoleFullNode  ChainDeploymentRole = "fullNode"
)
```

##### Why exactly one validator + ≥1 fullNode

Three things drove the gate decision:

1. **The workload always wants RPC parity with users.** Validators run with mempool admission rules and consensus duty cycles that surface latency artifacts unrelated to the SUT under load. Full nodes are the realistic RPC surface. The workload always connects to fullNode-role endpoints; there is no validator-fleet-endpoint mode in v1.
2. **One genesis ceremony per chain.** Allowing N validator deployments multiplies the genesis ceremony into a coordination problem the SND controller doesn't model. v1 codifies "exactly one validator deployment" so the existing genesis-ceremony plan applies cleanly.
3. **List-of-deployments cleanly absorbs heterogeneity.** Some chains want a second fullNode fleet (e.g., archive-mode fullNodes for historical RPC, regular fullNodes for the workload). The list shape lets that compose without a new top-level field; both fleets are role=fullNode but differ in their embedded `Spec`.

This generalizes — but does not remove — the prior "two-SNDs-always-materialized" rule.

##### What the prior `endpointPolicy` knob was

The previous draft considered an `endpointPolicy: validators|fullNodes` field for which fleet the workload connects to. It is **dropped permanently**. With the deployments-list shape, the answer is always "the deployment(s) with `role: fullNode`." `resolve-endpoints` (Orchestration plan task) selects all fullNode-role SNDs' headless Services; if multiple fullNode-role deployments exist, the workload sees the union of their pod DNS names. Controllers don't pick "which fullNode fleet" — that's a future expansion handled by adding a `chain.deployments[i].endpointSelector` field if and when needed.

##### Embedded-SND trade-off (load-bearing)

The embedded `SeiNodeDeploymentSpec` couples ValidationRun's CRD schema to SeiNodeDeployment's CRD schema. SND breaking changes break ValidationRun. Mitigation: same controller binary owns both; they version together. If/when validation gets its own controller binary or separate version cadence, switch to a `ChainTemplate` projection that copies fields explicitly. Document this as a re-evaluation trigger (≥1 SND breaking change forced ValidationRun's hand).

#### `LoadSpec` — the v1 actor body

`spec.load.workload` is the Phase 1 contract envelope, adopted **verbatim**.

```go
type LoadSpec struct {
    // Workload is the containerized load generator. The
    // ValidationLoadGenerationReconciler materializes this as a batch/v1.Job named
    // {runName} owned by the ValidationRun.
    Workload WorkloadSpec `json:"workload"`

    // Duration is the run's load window. Surfaced to the workload as
    // DURATION_SECONDS. The controller enforces wallclock via
    // spec.timeouts.runDuration (defaults from this).
    // +kubebuilder:validation:MinDuration=1s
    Duration metav1.Duration `json:"duration"`

    // Replicas is the load-gen pod parallelism. Controller injects
    // SHARD_INDEX and SHARD_COUNT env per K6's execution-segment idiom
    // (CLI-arg / env-injected, not magic discovery). Default 1.
    // +optional
    // +kubebuilder:default=1
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=64
    Replicas int32 `json:"replicas,omitempty"`
}

// WorkloadSpec is the Phase 1 contract (sei-protocol/platform#235), one-to-one.
//
// Reserved-env-var rejection: any user-supplied env entry whose `name` collides
// with a controller-injected variable is rejected at admission via CEL.
//
// +kubebuilder:validation:XValidation:rule="!self.env.exists(e, e.name in ['CHAIN_RPC_URL','CHAIN_WS_URL','CHAIN_ID','RUN_ID','RESULT_DIR','DURATION_SECONDS','NAMESPACE','SHARD_INDEX','SHARD_COUNT'])",message="env names CHAIN_RPC_URL, CHAIN_WS_URL, CHAIN_ID, RUN_ID, RESULT_DIR, DURATION_SECONDS, NAMESPACE, SHARD_INDEX, SHARD_COUNT are reserved by the validation controller and must not be set in spec.load.workload.env"
type WorkloadSpec struct {
    // Name is the workload identity used in S3 paths and metric labels
    // (e.g. "evm_transfer", "tokens"). Lowercase, hyphenless preferred.
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=63
    // +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9_]*$`
    Name string `json:"name"`

    // Image is the workload container image (digest-pinned recommended).
    // +kubebuilder:validation:MinLength=1
    Image string `json:"image"`

    // Command and Args override the image entrypoint.
    // +optional
    Command []string `json:"command,omitempty"`
    // +optional
    Args []string `json:"args,omitempty"`

    // Env are user-supplied env vars merged with controller-injected ones.
    // Controller-injected vars (CHAIN_RPC_URL, CHAIN_WS_URL, CHAIN_ID, RUN_ID,
    // RESULT_DIR, DURATION_SECONDS, NAMESPACE, SHARD_INDEX, SHARD_COUNT) are
    // reserved names — the CRD-level XValidation rule above hard-rejects any
    // user attempt to set them.
    // +optional
    Env []corev1.EnvVar `json:"env,omitempty"`

    // EnvFrom is forwarded verbatim. Tenant SAs control what's mountable.
    // +optional
    EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

    // Resources is forwarded verbatim onto the Job's Pod template.
    // +optional
    Resources corev1.ResourceRequirements `json:"resources,omitempty"`

    // ServiceAccountName names the Pod Identity-bound SA. Defaults to
    // "{namespace}-runner". Tenant pre-provisions; controller validates
    // existence at the apply-job task.
    // +optional
    ServiceAccountName string `json:"serviceAccountName,omitempty"`

    // Config is rendered to a ConfigMap and mounted at /etc/validation/config/.
    // Controller-side fixed-substitution applied: ${chainId}, ${rpcEndpoints},
    // ${runId}, ${namespace} are replaced inline before the ConfigMap is
    // applied. Other ${...} sequences pass through verbatim.
    // +optional
    Config *WorkloadConfigSpec `json:"config,omitempty"`

    // PodTemplate exposes a narrow set of pod-level fields (nodeSelector,
    // tolerations, securityContext, volumes/volumeMounts merged with the
    // controller's). Locked to a small allowlist; not a full PodSpec.
    // +optional
    PodTemplate *WorkloadPodTemplate `json:"podTemplate,omitempty"`
}

// WorkloadConfigSpec is rendered to a ConfigMap with one key per file.
// File contents are arbitrary text (JSON, YAML, INI, plain).
// +kubebuilder:validation:XValidation:rule="size(self.files) >= 1",message="config.files must have at least one entry"
type WorkloadConfigSpec struct {
    Files     map[string]string `json:"files"`
    // +optional
    MountPath string            `json:"mountPath,omitempty"`
}

type WorkloadPodTemplate struct {
    // +optional
    NodeSelector    map[string]string         `json:"nodeSelector,omitempty"`
    // +optional
    Tolerations     []corev1.Toleration       `json:"tolerations,omitempty"`
    // +optional
    SecurityContext *corev1.PodSecurityContext `json:"securityContext,omitempty"`
    // +optional
    Volumes         []corev1.Volume           `json:"volumes,omitempty"`
    // +optional
    VolumeMounts    []corev1.VolumeMount      `json:"volumeMounts,omitempty"`
    // +optional
    Affinity        *corev1.Affinity          `json:"affinity,omitempty"`
}
```

##### Reserved environment variables (controller-injected)

Adopted verbatim from Phase 1 (`/tmp/phase1-issue-body.md`):

| Var | Value |
|---|---|
| `CHAIN_RPC_URL` | First resolved RPC endpoint (always fullNode-role deployment(s)). Tendermint port. |
| `CHAIN_WS_URL`  | Same host, EVM WebSocket port (8546). |
| `CHAIN_ID` | `spec.chain.chainId` |
| `RUN_ID` | `metadata.name` of the ValidationRun (no UID — name is unique within ns) |
| `RESULT_DIR` | `/var/run/validation/results` (mounted emptyDir) |
| `DURATION_SECONDS` | `spec.load.duration.Seconds()` |
| `NAMESPACE` | `metadata.namespace` |
| `SHARD_INDEX` | Injected per pod (0..replicas-1) via Indexed-Job + downward-API on `JOB_COMPLETION_INDEX` |
| `SHARD_COUNT` | `spec.load.replicas` |

These names are CRD-rejected when supplied in `spec.load.workload.env` — see the `XValidation` rule on `WorkloadSpec` above.

##### Exit-code semantics

Adopted verbatim from Phase 1:

- `0` → workload Succeeded path (rules still gate the verdict)
- `1` → workload Failed path (`verdict=Failed`, `Reason=WorkloadAssertionFailed`)
- `2` → infra-level failure (`verdict=Error`, `Reason=WorkloadInfraFailure`). Run terminates in `Error`, *not* `Failed`. Heartbeat alert ignores `Error` so blast radius is bounded.

#### `SequenceSpec` and `ChaosSpec` — reserved v2 placeholders

```go
// SequenceSpec is reserved for v2. Field name and outer struct shape are
// load-bearing one-way doors; the body is intentionally empty until
// ValidationSequenceReconciler ships. Admission rejects spec.sequence in v1.
//
// Sketch of v2 shape: ordered list of typed steps {applyPatch, submitTx,
// awaitHeight, registerValidator, …}. Each step submits its work via a
// short-lived Job targeting the chain RPC service — pods/exec is rejected
// (see one-way doors). Reconciler is gated by predicate:
// spec.sequence != nil AND Conditions[TestRunning]=True.
type SequenceSpec struct {
    // Reserved. Adding fields here is additive; renaming SequenceSpec is breaking.
}

// ChaosSpec is reserved for v2. Body intentionally empty. Admission rejects
// spec.chaos in v1; in v2 admission additionally requires the run's namespace
// to carry label sei.io/chaos-allowed=true AND controller built with
// --enable-chaos-plan flag. Reconciler is gated by predicate:
// spec.chaos != nil AND Conditions[TestRunning]=True.
type ChaosSpec struct {
    // Reserved. v2 sketch: list of chaos-mesh experiments (NetworkChaos,
    // PodChaos, IOChaos) scoped to the chain's SND children, applied during
    // the run window with cleanup at finalize.
}
```

#### `ValidationRule` — alert + query schema

```go
// +kubebuilder:validation:XValidation:rule="(has(self.alert)?1:0) + (has(self.query)?1:0) == 1",message="exactly one of alert or query must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.alert) || self.type == 'alert'",message="alert body requires type=alert"
// +kubebuilder:validation:XValidation:rule="!has(self.query) || self.type == 'query'",message="query body requires type=query"
type ValidationRule struct {
    // Name is unique within spec.rules. DNS-label.
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=63
    // +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
    Name string `json:"name"`

    // Type discriminates the rule body. v1: alert, query.
    // +kubebuilder:validation:Enum=alert;query
    Type ValidationRuleType `json:"type"`

    // +optional
    Alert *AlertRule `json:"alert,omitempty"`
    // +optional
    Query *QueryRule `json:"query,omitempty"`

    // RunProperties tunes evaluation behavior. Defaults documented per-type.
    // +optional
    RunProperties *RuleRunProperties `json:"runProperties,omitempty"`
}

// +kubebuilder:validation:Enum=alert;query
type ValidationRuleType string

const (
    ValidationRuleTypeAlert ValidationRuleType = "alert"
    ValidationRuleTypeQuery ValidationRuleType = "query"
)

type AlertRule struct {
    RuleRef AlertRuleRef     `json:"ruleRef"`
    // +optional
    // +kubebuilder:default="30s"
    Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.alertname.size() > 0",message="alertname is required"
type AlertRuleRef struct {
    // +kubebuilder:validation:MinLength=1
    Name      string `json:"name"`
    // Same-namespace as the Run, OR a namespace labeled
    // sei.io/validation-shared-rules=true (e.g., monitoring).
    // +kubebuilder:validation:MinLength=1
    Namespace string `json:"namespace"`
    // +kubebuilder:validation:MinLength=1
    Alertname string `json:"alertname"`
}

type QueryRule struct {
    // +kubebuilder:validation:MinLength=1
    PromQL    string           `json:"promql"`
    // +kubebuilder:validation:Enum=">=";"<=";">";"<";"==";"!="
    Op        QueryComparator  `json:"op"`
    // OTel Round 1: numeric→string is breaking; string→numeric is additive.
    // +kubebuilder:validation:Pattern=`^-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$`
    Threshold string           `json:"threshold"`
    // +optional
    // +kubebuilder:default="30s"
    Timeout   *metav1.Duration `json:"timeout,omitempty"`
}

type RuleRunProperties struct {
    // Interval is the polling cadence. Default 30s. Min 5s, max 5m.
    // +optional
    // +kubebuilder:default="30s"
    // +kubebuilder:validation:Minimum=5
    // +kubebuilder:validation:Maximum=300
    Interval *metav1.Duration `json:"interval,omitempty"`

    // StopOnFailure: a Failed verdict short-circuits the run. The
    // ValidationOrchestrationReconciler sets Conditions[TestCancelled]=True,
    // which actor reconcilers (ValidationLoadGenerationReconciler) observe and
    // halt cooperatively. The orchestration plan's monitor-task-completion
    // task reads this condition and exits Failed.
    // +optional
    // +kubebuilder:default=false
    StopOnFailure bool `json:"stopOnFailure,omitempty"`

    // Retry is the number of consecutive Errors tolerated before
    // a rule matures into a permanent Error verdict. Defaults to 3.
    // Does NOT retry on Failed (verdicts of Failed are monotonic).
    // +optional
    // +kubebuilder:default=3
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:validation:Maximum=10
    Retry int32 `json:"retry,omitempty"`
}
```

There is no `mode` field on `ValidationRule` in v1: there is exactly one evaluation cadence (continuous polling, with a final sweep at run terminal). `edge` and `window-end` are additive future modes.

#### `ResultsSpec` and `RunTimeouts`

```go
type ResultsSpec struct {
    // Reserved for future bucket overrides. v1 has no user-tunable fields.
    // +optional
    S3 *S3ResultsSpec `json:"s3,omitempty"`
}

type S3ResultsSpec struct {
    // Reserved. v1 ignores user-supplied bucket/prefix.
}

type RunTimeouts struct {
    // ChainReady caps wait-chain-ready (Orchestration plan task #2). Default 20m.
    // +optional
    // +kubebuilder:default="20m"
    ChainReady *metav1.Duration `json:"chainReady,omitempty"`

    // RunDuration caps the workload Job's wall-clock from apply to terminal.
    // When omitted and spec.load is set, defaults to spec.load.duration + 5m.
    // For rules-only Runs (no spec.load), defaults to 1h.
    // +optional
    RunDuration *metav1.Duration `json:"runDuration,omitempty"`
}
```

#### `ValidationRunStatus` — partitioned across plan slots and conditions

```go
type ValidationRunStatus struct {
    // ObservedGeneration tracks the spec generation processed by the latest
    // reconcile. Set by ValidationOrchestrationReconciler — it is the single
    // generation-tracking authority across both controllers.
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // Phase is the high-level lifecycle state. Pascal-case.
    // OWNER: ValidationOrchestrationReconciler.
    // +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Error;Cancelled
    // +optional
    Phase ValidationRunPhase `json:"phase,omitempty"`

    // Verdict is a denormalized human-friendly summary that mirrors a
    // condition. Computed at terminal phase entry.
    // OWNER: ValidationOrchestrationReconciler.
    // +optional
    // +kubebuilder:validation:Enum=Passed;Failed;Awaited;Error
    Verdict ValidationVerdict `json:"verdict,omitempty"`

    // StartTime / CompletionTime / Duration. OWNER: ValidationOrchestrationReconciler.
    // +optional
    StartTime      *metav1.Time `json:"startTime,omitempty"`
    // +optional
    CompletionTime *metav1.Time `json:"completionTime,omitempty"`
    // +optional
    Duration       string       `json:"duration,omitempty"`

    // Plans carries one TaskPlan slot per registered controller. Each
    // controller writes ONLY its own slot via its dedicated SSA field
    // manager. There is no shared "active plan" — Orchestration and
    // LoadGeneration progress in parallel against their own slots.
    // +optional
    Plans ValidationRunPlans `json:"plans,omitempty"`

    // Chain reports the materialized SND children and resolved endpoints.
    // OWNER: ValidationOrchestrationReconciler.
    // +optional
    Chain *ChainStatus `json:"chain,omitempty"`

    // WorkloadExitCode is the exit code captured from the workload Job's
    // pod once the Job reaches terminal state. 0/1/2 per Phase 1 contract.
    // OWNER: ValidationLoadGenerationReconciler.
    // Unset until Conditions[LoadComplete]=True.
    // +optional
    WorkloadExitCode *int32 `json:"workloadExitCode,omitempty"`

    // Rules carries per-rule verdicts and supporting evidence. Updated
    // continuously by the orchestration plan's monitor-task-completion task.
    // OWNER: ValidationOrchestrationReconciler.
    // +listType=map
    // +listMapKey=name
    // +optional
    Rules []RuleStatus `json:"rules,omitempty"`

    // Report carries the resolved S3 URL. OWNER: ValidationOrchestrationReconciler.
    // +optional
    Report *ReportStatus `json:"report,omitempty"`

    // FailedPlan names the plan slot that caused the run to terminate in
    // Failed/Error. v1 only ever stamps "loadGeneration" or empty (when the
    // orchestration plan itself failed in chain bring-up). Reserved now so
    // v2 reconcilers (sequence, chaos) can stamp "sequence"/"chaos" without
    // schema churn. OWNER: ValidationOrchestrationReconciler.
    // +optional
    FailedPlan string `json:"failedPlan,omitempty"`

    // Conditions includes Tekton-style Succeeded plus the run's coordination
    // signals. See "Conditions and the single-writer table".
    // +listType=map
    // +listMapKey=type
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

type ValidationRunPlans struct {
    // Orchestration is the chain-bring-up + monitoring + finalize plan.
    // OWNER: ValidationOrchestrationReconciler (field manager: validationrun-orchestration).
    // +optional
    Orchestration *sei_v1alpha1.TaskPlan `json:"orchestration,omitempty"`

    // LoadGeneration is the workload-Job lifecycle plan.
    // OWNER: ValidationLoadGenerationReconciler (field manager: validationrun-loadgeneration).
    // Nil when spec.load is unset (rules-only Run) or when the
    // ValidationLoadGenerationReconciler is disabled by deployment opt-in.
    // +optional
    LoadGeneration *sei_v1alpha1.TaskPlan `json:"loadGeneration,omitempty"`

    // Reserved for v2 actor controllers. Adding fields here is additive.
    // +optional
    // Sequence *sei_v1alpha1.TaskPlan `json:"sequence,omitempty"`
    // +optional
    // Chaos    *sei_v1alpha1.TaskPlan `json:"chaos,omitempty"`
}

// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Error;Cancelled
type ValidationRunPhase string

const (
    PhasePending   ValidationRunPhase = "Pending"
    PhaseRunning   ValidationRunPhase = "Running"
    PhaseSucceeded ValidationRunPhase = "Succeeded"
    PhaseFailed    ValidationRunPhase = "Failed"
    PhaseError     ValidationRunPhase = "Error"
    PhaseCancelled ValidationRunPhase = "Cancelled"
)

// +kubebuilder:validation:Enum=Passed;Failed;Awaited;Error
type ValidationVerdict string

type ChainStatus struct {
    // Deployments mirrors spec.chain.deployments[].name with the resolved
    // SND child name and its current phase.
    // +listType=map
    // +listMapKey=name
    Deployments  []ChainDeploymentStatus `json:"deployments"`
    // RPCEndpoints is the union of fullNode-role deployments' resolved
    // headless-Service pod DNS names + ports. Stamped at resolve-endpoints.
    // +optional
    RPCEndpoints []string `json:"rpcEndpoints,omitempty"`
}

type ChainDeploymentStatus struct {
    Name    string                  `json:"name"`
    Role    ChainDeploymentRole     `json:"role"`
    SNDName string                  `json:"sndName"`
    Phase   string                  `json:"phase,omitempty"` // mirror of SND.status.phase
}

// +kubebuilder:validation:XValidation:rule="(has(self.alert)?1:0) + (has(self.query)?1:0) <= 1",message="rule status carries at most one of alert or query"
type RuleStatus struct {
    Name             string             `json:"name"`
    Type             ValidationRuleType `json:"type"`
    Verdict          ValidationVerdict  `json:"verdict"`
    // Reason categorizes Error verdicts (PrometheusRuleNotFound, NoSamples,
    // AmbiguousResult, NaN, PrometheusUnavailable, RBACDenied, Timeout) and
    // Failed verdicts (ThresholdViolated, AlertFired).
    // +optional
    Reason           string             `json:"reason,omitempty"`
    // +optional
    LastEvaluatedAt  *metav1.Time       `json:"lastEvaluatedAt,omitempty"`
    // +optional
    NextEvaluationAt *metav1.Time       `json:"nextEvaluationAt,omitempty"`
    // +optional
    Query            *QueryRuleStatus   `json:"query,omitempty"`
    // +optional
    Alert            *AlertRuleStatus   `json:"alert,omitempty"`
}

type QueryRuleStatus struct {
    ActualValue string `json:"actualValue"` // string, lossless
    Threshold   string `json:"threshold"`
    Op          string `json:"op"`
}

type AlertRuleStatus struct {
    FiredCount  int32        `json:"firedCount"`
    LastFiredAt *metav1.Time `json:"lastFiredAt,omitempty"`
}

type ReportStatus struct {
    // S3URL = s3://harbor-validation-results/{namespace}/{job}/{runId}/
    // OWNER: ValidationOrchestrationReconciler. Stamped at finalize.
    // +optional
    S3URL string `json:"s3Url,omitempty"`
}
```

##### Conditions and the single-writer table

Each condition has exactly one writing controller. SSA field-manager isolation enforces this — `validationrun-orchestration` and `validationrun-loadgeneration` never touch each other's owned condition keys. The orchestrator's status patch lists only the conditions it owns; the loadgenerator's patch lists only `LoadComplete`. controller-runtime + Kubernetes server-side apply preserves both sets correctly because they are partitioned by `type`.

| Condition / status field | Owner controller | Field manager | Purpose |
|---|---|---|---|
| `Conditions[TestRunning]` | ValidationOrchestrationReconciler | `validationrun-orchestration` | Set True at the `mark-test-running` task; gates ValidationLoadGenerationReconciler's predicate |
| `Conditions[TestComplete]` | ValidationOrchestrationReconciler | `validationrun-orchestration` | Set True at finalize; monotonic |
| `Conditions[Succeeded]` | ValidationOrchestrationReconciler | `validationrun-orchestration` | Tekton-style; set at finalize |
| `Conditions[TestCancelled]` | ValidationOrchestrationReconciler | `validationrun-orchestration` | Set True on stop-on-failure rule trip; observed by actor reconcilers |
| `Conditions[LoadComplete]` | ValidationLoadGenerationReconciler | `validationrun-loadgeneration` | Set True when wait-job-terminal captures exit code |
| `status.chain.*` | ValidationOrchestrationReconciler | `validationrun-orchestration` | |
| `status.report.*` | ValidationOrchestrationReconciler | `validationrun-orchestration` | |
| `status.rules[]` | ValidationOrchestrationReconciler | `validationrun-orchestration` | |
| `status.phase` / `.verdict` / `.startTime` / `.completionTime` / `.duration` | ValidationOrchestrationReconciler | `validationrun-orchestration` | |
| `status.failedPlan` | ValidationOrchestrationReconciler | `validationrun-orchestration` | |
| `status.workloadExitCode` | ValidationLoadGenerationReconciler | `validationrun-loadgeneration` | Stamped at wait-job-terminal |
| `status.plans.orchestration` | ValidationOrchestrationReconciler | `validationrun-orchestration` | |
| `status.plans.loadGeneration` | ValidationLoadGenerationReconciler | `validationrun-loadgeneration` | |

`Succeeded` matches Tekton TaskRun/PipelineRun semantics — `kubectl wait --for=condition=Succeeded validationrun/X` is the contract.

`TestRunning` is the gate the ValidationLoadGenerationReconciler watches: the controller's predicate fires only when this condition transitions to True. It is set by the orchestration plan's `mark-test-running` task after chain readiness and endpoint resolution succeed. Before that point the ValidationLoadGenerationReconciler short-circuits (no plan, no work).

`TestCancelled` is the cross-controller stop signal: when monitor-task-completion observes a stop-on-failure rule trip, it sets this condition; ValidationLoadGenerationReconciler observes it on its next reconcile and halts (stops submitting tasks; in-flight work proceeds; chain teardown via cascade-delete is the rollback).

| Condition `Succeeded` | status | reason | meaning |
|---|---|---|---|
| `Unknown` | `Pending` / `Running` | Not yet terminal |
| `True` | `RunSucceeded` | Phase=Succeeded |
| `False` | `WorkloadFailed` | Workload exit 1 |
| `False` | `RuleFailed:{ruleName}` | One or more rules verdict=Failed |
| `False` | `RuleError` | Run terminated with Phase=Error from rule eval |
| `False` | `WorkloadInfraFailure` | Workload exit 2 |
| `False` | `ChainNotReady` | wait-chain-ready timed out |
| `False` | `Cancelled` | Phase=Cancelled |

| Condition `TestComplete` | status | reason | meaning |
|---|---|---|---|
| `Unknown` | `Pending` / `Running` | Run not yet terminal |
| `True` | `RunFinalized` | Orchestration plan reached finalize task; phase + verdict + S3 URL set |

| Condition `TestRunning` | status | reason | meaning |
|---|---|---|---|
| `Unknown` | `Pending` | Chain not yet ready |
| `True` | `ChainReady` | Endpoints resolved; actor reconcilers may proceed |
| `False` | `Cancelled` | Set False when the run terminates (closes the gate behind us) |

| Condition `LoadComplete` | status | reason | meaning |
|---|---|---|---|
| `Unknown` | `Pending` / `Running` | Workload not yet terminal |
| `True` | `JobTerminated` | Job reached Complete or Failed; `status.workloadExitCode` populated |
| `True` | `JobTimedOut` | `spec.timeouts.runDuration` exceeded; controller cancelled the Job; exit code synthesized as 2 |

| Condition `TestCancelled` | status | reason | meaning |
|---|---|---|---|
| `Unknown` | `Pending` / `Running` | Not signalled |
| `True` | `RuleStopOnFailure:{ruleName}` | Stop-on-failure tripped; actor reconcilers halt |

### Future kinds (one paragraph each, NOT detailed)

#### `ValidationSuite` — designed but NOT implemented in v1

Designed at field-level so `ValidationRun`'s shape doesn't lock out the suite parent. CRD ships with v1; no controller implements it. Attempting to create one returns an admission-time error (`ValidationSuite reconciler not registered in this controller version`).

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vsuite
type ValidationSuite struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ValidationSuiteSpec   `json:"spec,omitempty"`
    Status ValidationSuiteStatus `json:"status,omitempty"`
}

type ValidationSuiteSpec struct {
    // +listType=map
    // +listMapKey=name
    // +kubebuilder:validation:MinItems=1
    Runs []SuiteRunSpec `json:"runs"`
    // +optional
    // +kubebuilder:default=Sequential
    // +kubebuilder:validation:Enum=Sequential;Parallel
    Concurrency SuiteConcurrency `json:"concurrency,omitempty"`
    // +optional
    // +kubebuilder:default=true
    StopOnFailure bool `json:"stopOnFailure,omitempty"`
}

type SuiteRunSpec struct {
    // +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
    Name string            `json:"name"`
    Spec ValidationRunSpec `json:"spec"`
}
```

No DAG. Users wanting a DAG wrap suites in Argo (Litmus's "we outsource multi-experiment to Argo" lesson — [Litmus probes](https://docs.litmuschaos.io/docs/concepts/probes)).

#### `ValidationSchedule` — designed but NOT implemented in v1

Modeled on Argo `CronWorkflow` ([Argo CronWorkflow](https://argo-workflows.readthedocs.io/en/latest/cron-workflows/)) plus Chaos Mesh's annotation-pause refinement.

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vsched
type ValidationSchedule struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`
    Spec   ValidationScheduleSpec   `json:"spec,omitempty"`
    Status ValidationScheduleStatus `json:"status,omitempty"`
}

type ValidationScheduleSpec struct {
    // +kubebuilder:validation:MinItems=1
    Schedules []string `json:"schedules"`
    // +optional
    // +kubebuilder:default=UTC
    Timezone string `json:"timezone,omitempty"`
    RunTemplate ValidationRunSpec `json:"runTemplate"`
    // +optional
    // +kubebuilder:default=Forbid
    // +kubebuilder:validation:Enum=Allow;Replace;Forbid
    ConcurrencyPolicy SchedConcurrency `json:"concurrencyPolicy,omitempty"`
    // +optional
    SuccessfulRunsHistoryLimit *int32 `json:"successfulRunsHistoryLimit,omitempty"`
    // +optional
    FailedRunsHistoryLimit *int32 `json:"failedRunsHistoryLimit,omitempty"`
    // +optional
    // +kubebuilder:default=60
    StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`
    // Pause via annotation (validation.sei.io/paused: "true").
}
```

## Lifecycle and plan tasks

The two sub-controllers each integrate with `internal/planner/` (see `/Users/brandon/sei-k8s-controller/internal/planner/doc.go`) but produce **independent plans** that progress in parallel. There is no barrier task; the ValidationLoadGenerationReconciler is gated **at event-delivery time** by a controller-runtime predicate.

### ValidationOrchestrationReconciler — required when validation is enabled

**Purpose:** chain bring-up, endpoint resolution, gate-flip, monitoring, finalize. Owns every status field except `status.workloadExitCode`, `status.plans.loadGeneration`, and `Conditions[LoadComplete]`.

**Builder location:** `internal/planner/validationrun_orchestration.go`. Resolver in the controller package follows the same `ResolvePlan → persist → ExecutePlan` shape as `NodeResolver` and `ForGroup`.

**Plan tasks (ordered, sequential — 6 tasks):**

```
1. ensure-chain               (controller-side; SSA all SND children)
2. wait-chain-ready           (controller-side, async; SND.status.phase=Ready)
3. resolve-endpoints          (controller-side; LIST fullNode-role headless Services)
4. mark-test-running          (controller-side; sets Conditions[TestRunning]=True;
                               this is the gate-flip that lets actor reconcilers
                               start their plans)
5. monitor-task-completion    (controller-side, async; reads Conditions[LoadComplete],
                               status.rules[], status.workloadExitCode; polls rules
                               per RunProperties.Interval; computes stop-on-failure)
6. finalize                   (controller-side; computes phase + verdict + report.s3Url;
                               sets Conditions[TestComplete]=True, Conditions[Succeeded];
                               flips Conditions[TestRunning]=False)

Plan TargetPhase: ignored (see "TargetPhase decoupling" below)
Plan FailedPhase: ignored
```

The 6-task collapse from the prior 7-task design pulls the workload-side concerns (`render-config` and `apply-job`) out of the orchestration plan and into ValidationLoadGenerationReconciler's plan. The orchestration plan no longer touches the Job at all; it observes condition state and aggregates verdicts.

### ValidationLoadGenerationReconciler — opt-in (default on within validation in v1)

**Purpose:** materialize the workload Job and ConfigMap, watch to terminal, capture the exit code, set `Conditions[LoadComplete]=True`. That is the entire surface area.

**Builder location:** `internal/planner/validationrun_loadgen.go`.

**Plan tasks (ordered, sequential — 3 tasks):**

```
1. render-config            (controller-side; SSA ConfigMap with substitutions
                             ${chainId}, ${rpcEndpoints}, ${runId}, ${namespace};
                             skipped when spec.load.workload.config is unset)
2. apply-job                (controller-side; SSA batch/v1.Job with Indexed
                             completion mode, reserved-env injection, OwnerRef→Run)
3. wait-job-terminal        (controller-side, async; watches Job to terminal,
                             captures pod exit code, stamps status.workloadExitCode,
                             sets Conditions[LoadComplete]=True)
```

**Predicate-gated reconciliation.** The reconciler's `SetupWithManager` registers with a `WithEventFilter` that fires only when both:

- `spec.load != nil`, AND
- `Conditions[TestRunning]` exists with `Status=True`

```go
// internal/controller/validationrun/loadgen_predicate.go
var loadActorPredicate = predicate.Funcs{
    CreateFunc: func(e event.CreateEvent) bool {
        return shouldReconcileLoadGen(e.Object.(*validationv1alpha1.ValidationRun))
    },
    UpdateFunc: func(e event.UpdateEvent) bool {
        return shouldReconcileLoadGen(e.ObjectNew.(*validationv1alpha1.ValidationRun))
    },
    DeleteFunc: func(e event.DeleteEvent) bool { return true },
    GenericFunc: func(_ event.GenericEvent) bool { return false },
}

func shouldReconcileLoadGen(vr *validationv1alpha1.ValidationRun) bool {
    if vr.Spec.Load == nil {
        return false
    }
    cond := meta.FindStatusCondition(vr.Status.Conditions, "TestRunning")
    return cond != nil && cond.Status == metav1.ConditionTrue
}
```

Before `Conditions[TestRunning]=True`, the reconciler never sees an event for the CR (controller-runtime drops the event before invoking Reconcile). This is the controller-runtime-native "wait for upstream readiness" pattern — no barrier task, no polling, no synchronization primitive.

`ValidationLoadGenerationReconciler.ResolvePlan` returns `(nil, nil)` if `spec.load == nil` even after the predicate fires (defense-in-depth) so re-running with the controller enabled but `spec.load=nil` Run is a no-op rather than an error.

### Plan ownership and the executor

Each controller passes its own `Named()` to the `controller-runtime` builder (so leader-election locks are independent and metric labels are partitioned):

```go
// ValidationOrchestrationReconciler
ctrl.NewControllerManagedBy(mgr).
    Named("validationrun-orchestration").
    For(&validationv1alpha1.ValidationRun{}).
    Owns(&seiv1alpha1.SeiNodeDeployment{}).
    Complete(r)

// ValidationLoadGenerationReconciler
ctrl.NewControllerManagedBy(mgr).
    Named("validationrun-loadgeneration").
    For(&validationv1alpha1.ValidationRun{}, builder.WithPredicates(loadActorPredicate)).
    Owns(&batchv1.Job{}).
    Owns(&corev1.ConfigMap{}).
    Complete(r)
```

The `planner.Executor[*ValidationRun]` is parameterized by the plan slot: each controller passes its own `slotAccessor` to the executor (a function `func(*ValidationRun) **TaskPlan` returning a pointer-to-pointer for the right slot, e.g., `&vr.Status.Plans.Orchestration`). The executor's existing single-patch model writes only the slot it was given. SSA's field-manager isolation handles the rest.

### Plan-driven invariants inherited from the existing controllers

- Each plan persisted before any task executes (atomic creation).
- Single-patch model per controller per reconcile: tasks mutate owned resources; executor mutates plan/phase in-memory; reconciler flushes once via SSA with its dedicated field manager.
- Planner owns conditions; executor never sets conditions.
- Terminal plan observed by next reconcile, which (for ValidationOrchestrationReconciler only) sets the terminal phase. ValidationLoadGenerationReconciler does NOT set phase on terminal — it only sets `Conditions[LoadComplete]=True`; the orchestrator's monitor-task-completion task observes that condition and continues.

### TargetPhase decoupling for v1

`TaskPlan.TargetPhase` is typed `seiv1alpha1.SeiNodePhase` — its enum values (`Pending|Initializing|Running|Failed|Terminating`) do not align with `ValidationRunPhase` (`Pending|Running|Succeeded|Failed|Error|Cancelled`). For v1:

- Both ValidationRun controllers leave `TaskPlan.TargetPhase` and `FailedPhase` empty in their built plans.
- The executor's `setTargetPhase` short-circuit (when TargetPhase is empty, skip the phase write) handles this cleanly — already exercised by the existing NodeUpdate plans.
- Phase transitions are owned exclusively by the orchestration plan's `finalize` task, which computes `Phase + Verdict + status.failedPlan` from `Conditions` + `status.workloadExitCode` + `status.rules[]` and writes them in the orchestrator's status patch.
- ValidationLoadGenerationReconciler does NOT write `status.phase` ever. Its terminal-plan observation only sets `Conditions[LoadComplete]=True` and clears its own plan slot.

This is documented as Open Dependency #2 — when v2 lands a third actor, we may need a `TaskPlan` type with controller-typed `TargetPhase`/`FailedPhase`. v1 sidesteps the issue.

### Per-task contract

Each task lives at `internal/task/validation/{name}.go`, follows the existing controller-side task pattern (`Submit / Poll / Apply` interface — see `internal/task/observe_image.go`), and is registered in `internal/task/task.go`'s deserialize map.

#### Orchestration plan tasks

##### 1. `ensure-chain`

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeEnsureChain` |
| Sync/Async | Sync |
| Idempotent op | Server-side apply one SND per `spec.chain.deployments[]` entry, named `{chainId}-{name}`, with `OwnerReferences` to the Run. Field manager `validationrun-orchestration`. |
| Inputs | `Run.spec.chain` |
| Success | All SNDs exist with the expected spec hash |
| Failure | API error (terminal after 3 retries → `Reason=ChainApplyFailed`); SND validation reject (terminal → `Reason=ChainSpecInvalid`) |

Injected fields the task fills before SSA:

- `genesis.chainId = chain.chainId` on the validator-role SND if unset.
- `template.spec.chainId = chain.chainId` on every template if unset.
- `template.spec.validator: {}` on validator-role SND templates; `template.spec.fullNode: {}` on fullNode-role templates.
- Default `peers: [{label: {selector: {sei.io/chain-id: chainId}}}]` for validator-role and `[{label: {selector: {sei.io/nodedeployment: <validator-snd-name>}}}]` for fullNode-role — when the user omitted `peers`.
- Labels on every materialized object: `sei.io/managed-by=validationrun`, `sei.io/run-id=<runName>`, `sei.io/chain-id=<chainId>`, `sei.io/deployment-name=<chainDeployment.name>`.

##### 2. `wait-chain-ready`

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeWaitChainReady` |
| Sync/Async | Async (RequeueAfter pattern) |
| Idempotent op | GET each materialized SND; check `status.phase == Ready` AND `status.conditions[NodesReady].status == True`. |
| Timeout | `spec.timeouts.chainReady` (default 20m) |
| Failure | Timeout → `Reason=ChainNotReady`. Any SND `phase=Failed` → `Reason=ChainFailed` |

**SND readiness already gates on chain catch-up.** `internal/noderesource/noderesource.go:366-377` wires `readinessProbe.httpGet` at `seid:RPC_PORT/lag_status` (sei-tendermint's threshold-configured endpoint that returns non-200 while lagging behind chain tip). kubelet sees probe failure → Pod stays NotReady → Service excludes it → SND `phase=Ready` waits via the existing `ConditionNodesReady` aggregation. The `wait-chain-ready` task's poll on SND `phase=Ready` therefore does include the catching_up precondition transitively. (Earlier drafts of this LLD claimed this was a gap and proposed extending the seictl sidecar; that claim was based on incorrect investigation. See `sei-protocol/sei-k8s-controller#144` for the closed-as-already-implemented record.)

##### 3. `resolve-endpoints`

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeResolveEndpoints` |
| Sync/Async | Sync |
| Idempotent op | LIST headless Services with selector `sei.io/managed-by=validationrun, sei.io/run-id=<runName>, sei.io/role=fullNode`; build `{podDNSName}:{port}` list across all fullNode-role deployments. Stamps `Run.status.chain.rpcEndpoints`. |
| Selection | Always the union of all fullNode-role deployments. |
| Failure | Empty result after 5 retries → `Reason=EndpointsNotResolvable` |

##### 4. `mark-test-running`

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeMarkTestRunning` |
| Sync/Async | Sync (one-shot) |
| Idempotent op | `meta.SetStatusCondition` with `Type=TestRunning, Status=True, Reason=ChainReady`. |
| Side effect | Triggers ValidationLoadGenerationReconciler's predicate-gated event delivery on the next informer dispatch — controller-runtime's update event (the conditions array changed) is delivered through the predicate; if `spec.load != nil`, ValidationLoadGenerationReconciler reconciles and starts its plan. |
| Failure | None (in-memory condition write; persisted via the normal status patch) |

##### 5. `monitor-task-completion`

This is the central observation task. It is the renamed-and-reduced successor to the prior `monitor-run` task. It does **not** watch the workload Job — that is ValidationLoadGenerationReconciler's job. It reads condition + status state and decides whether the run is done.

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeMonitorTaskCompletion` |
| Sync/Async | Async (RequeueAfter; refines requeue cadence by min rule interval) |
| Inputs | `Run.spec.rules`, `Run.status.rules`, `Run.status.conditions[LoadComplete, ...]`, `Run.status.workloadExitCode` |
| Timeout | `spec.timeouts.runDuration` |

**Per-iteration behavior:**

```
1. Condition gathering:
     loadComplete = Conditions[LoadComplete].Status == True (or spec.load == nil → treat as True at runStart+0)
     // For rules-only Runs: monitor-task-completion's loadComplete defaults to
     // True at task start; the run's "completion" is purely rule-driven.

2. Rule-side advance (for each rule with verdict != Failed):
     if rule.LastEvaluatedAt + Interval <= now (or unset and now >= runStart + Interval):
       issue Prometheus instant query at time=now
       classify into Passed | Failed | Error (per per-rule semantics)
       update status.rules[i] with verdict, reason, query/alert evidence,
         LastEvaluatedAt = now,
         NextEvaluationAt = now + Interval

3. Stop-on-failure:
     if any rule with RunProperties.StopOnFailure=true tripped to Failed
       in this iteration:
       set Conditions[TestCancelled] = True, Reason=RuleStopOnFailure:{ruleName}
       exit task as Failed → planner advances to finalize, which sets
         Phase=Failed and FailedPlan="" (orchestration's own ruling, no actor blame)

4. Exit conditions:
     if loadComplete AND no rule has verdict=Failed:
       do a final sweep — for any rule whose NextEvaluationAt <= now+Interval
       (i.e., still pending) issue one last evaluation now
       exit task Complete → planner advances to finalize

     if loadComplete AND some rule has verdict=Failed:
       exit task Failed → planner advances to finalize

5. Else (still running):
     RequeueAfter min(
       earliest rule's NextEvaluationAt - now,
       loadCompletePollInterval (10s — bound on observing a condition flip
                                    even though informer events drive most updates)
     )
```

**Idempotency.** The task carries no in-memory state across reconciles. Every iteration recomputes from `.status` (rules) and `.status.conditions` (LoadComplete). All advance is monotonic.

**Implementation invariants for this task** (load-bearing — see Implementation invariants subsection below):

- Transient errors (Prometheus 5xx, network timeout, RBAC denial) return `RequeueAfter`, NEVER `task.TerminalError`. The existing executor's `RetryCount/MaxRetries` was designed for short-running sidecar tasks that re-execute on retry; a long-running poller would mature `MaxRetries` into a permanent failure incorrectly.
- Per-rule `Retry` (in `RuleRunProperties`) governs the rule's *own* error budget — that is the right per-rule retry surface; the task-level retry is not used here.

##### Per-rule evaluation semantics

For an `alert` rule (instant query against the synthetic `ALERTS` series):

```promql
max_over_time(ALERTS{alertname="X",alertstate="firing"}[<runDuration-so-far>s]) > 0
```

- Result > 0 → `verdict=Failed`, `Reason=AlertFired`, fill `AlertRuleStatus.FiredCount` + `LastFiredAt`
- Result == 0 → `verdict=Passed`
- Prometheus 5xx / network error → `verdict=Error`, `Reason=PrometheusUnavailable` (counted against `Retry` budget; resets to Awaited on next iteration if budget remains)

For a `query` rule (instant query of user PromQL):

- Result must be **scalar** or **vector of length 1**. Vector length 0 → `verdict=Error`, `Reason=NoSamples`. Length > 1 → `Error/AmbiguousResult`.
- `NaN` → `Error/NaN`. Never silent pass/fail.
- Comparator floats: document recommendation `>=`/`<=`; `==`/`!=` is exact-bits.
- Threshold coerced from string to float64 (`strconv.ParseFloat`); CEL admission already gated the regex.
- Comparator passes → `verdict=Passed`. Comparator fails → `verdict=Failed`, `Reason=ThresholdViolated`.
- Store `actualValue: string` and `threshold: string` losslessly in `.status.rules[].query`.

##### Failure model (per rule)

- `Failed` = "signal said SUT misbehaved" (alert fired in window, query violated threshold). Monotonic.
- `Error` = "couldn't ask" (Prometheus 5xx, network timeout, RBAC denial, unresolved ref, NaN, NoSamples, AmbiguousResult). Decays — successful evaluation overwrites Error with Passed/Failed; matures into permanent Error after `Retry` consecutive Errors.

##### PrometheusRule resolution (folded into monitor-task-completion's first iteration)

For each rule with `type=alert`, the first evaluation iteration GETs the referenced `monitoring.coreos.com/v1.PrometheusRule`; verifies `alertname` exists in the rule's spec; validates cross-namespace policy. Per-rule failure of resolution writes `verdict=Error` with `Reason=PrometheusRuleNotFound` or `Reason=CrossNamespaceForbidden`; does NOT block other rules. If **all** rules fail resolution on first iteration, the task exits Error with `Reason=AllRulesUnresolved`.

##### 6. `finalize`

Computes the final phase + verdict + condition from accumulated state, stamps the S3 URL, and closes the `TestRunning` gate. Pure in-memory aggregation plus one status patch:

```
loadComplete = Conditions[LoadComplete].Status == True (or spec.load == nil)
exit         = *status.workloadExitCode (set by ValidationLoadGenerationReconciler; nil if load unset)
ruleVerdicts = status.rules[]
cancelled    = Conditions[TestCancelled].Status == True

case cancelled:                                  → Phase=Failed, Reason=RuleStopOnFailure:{ruleName}, FailedPlan="" (orchestration's ruling)
case spec.load != nil && exit == 2:              → Phase=Error, Reason=WorkloadInfraFailure, FailedPlan="loadGeneration"
case spec.load != nil && exit == 1 && any(rule.Failed):
                                                 → Phase=Failed, Reason=WorkloadFailed (primary), FailedPlan="loadGeneration", append RuleFailed conditions
case spec.load != nil && exit == 1:              → Phase=Failed, Reason=WorkloadFailed, FailedPlan="loadGeneration"
case any(rule.Failed):                           → Phase=Failed, Reason=RuleFailed:{firstFailedRule}, FailedPlan=""
case all(rule.Passed) && (spec.load==nil || exit==0):
                                                 → Phase=Succeeded, Reason=RunSucceeded
case any(rule.Error) && all others Passed:       → Phase=Error,  Reason=RuleError, FailedPlan=""
default:                                         → Phase=Error,  Reason=Unknown
```

Sets `status.completionTime`, `status.duration`, the `Succeeded` condition, sets `Conditions[TestComplete]=True, Reason=RunFinalized`, flips `Conditions[TestRunning]=False, Reason=Cancelled` (closes the gate so any in-flight ValidationLoadGenerationReconciler reconcile observes the cancellation), computes `status.report.s3Url = s3://harbor-validation-results/{ns}/{job}/{runId}/`, and emits the `validation_run_terminal_total{verdict}` metric (heartbeat-alert input).

The S3 URL stamp is trivially derivable; the controller never reads from S3.

#### LoadGeneration plan tasks

##### 1. `render-config`

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeRenderConfig` |
| Sync/Async | Sync |
| Idempotent op | Apply ConfigMap with files from `load.workload.config.files`, fixed-substitutions applied (`${chainId}`, `${rpcEndpoints}`, `${runId}`, `${namespace}` — sourced from `Run.status.chain.rpcEndpoints`). Field manager `validationrun-loadgeneration`. OwnerRef → Run. Name `{runName}-config`. |
| Skipped when | `load.workload.config` unset |
| Failure | API error → terminal `Reason=ConfigApplyFailed`, plan FailedPlan="loadGeneration" stamped by orchestrator's finalize |

The substitution sources `${rpcEndpoints}` from `Run.status.chain.rpcEndpoints`, which ValidationOrchestrationReconciler stamped during `resolve-endpoints` before flipping `Conditions[TestRunning]`. The predicate guarantees this status field is populated before ValidationLoadGenerationReconciler reconciles.

##### 2. `apply-job`

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeApplyJob` |
| Sync/Async | Sync |
| Idempotent op | SSA `batch/v1.Job` named `{runName}` with `spec.parallelism=replicas`, `spec.completions=replicas`, `spec.completionMode=Indexed`, `backoffLimit=0`. Pod template carries reserved env vars (with `JOB_COMPLETION_INDEX` exposed as `SHARD_INDEX` via env-from-fieldRef), mounted ConfigMap, RESULT_DIR emptyDir, `serviceAccountName=spec.load.workload.serviceAccountName \|\| {ns}-runner`. Field manager `validationrun-loadgeneration`. OwnerRef → Run. |
| Failure | Forbidden (no SA) → terminal `Reason=ServiceAccountMissing`. Other API errors retried. |

The Job carries no Flux labels (`kustomize.toolkit.fluxcd.io/*`, `app.kubernetes.io/managed-by=flux`). Hard invariant — see One-Way Doors.

##### 3. `wait-job-terminal`

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeWaitJobTerminal` |
| Sync/Async | Async (RequeueAfter; informer events on Owns(Job) refine cadence) |
| Idempotent op | GET workload Job; if condition `Complete=True OR Failed=True`, GET the workload pod (label-selected), parse `pod.status.containerStatuses[].state.terminated.exitCode`, stamp `status.workloadExitCode` and set `Conditions[LoadComplete]=True, Reason=JobTerminated`. If `Conditions[TestCancelled]=True` was set by orchestrator, halt cooperatively (delete the Job; the cascade-delete on Run terminal will clean up regardless). |
| Timeout | `spec.timeouts.runDuration` (default `load.duration + 5m`). On timeout, delete Job, synthesize exit code 2, set `Conditions[LoadComplete]=True, Reason=JobTimedOut`. |
| Failure | API error → RequeueAfter (transient); plan-level terminal failure if Job spec is invalid (would have been caught at apply-job). |

**Cancellation observation.** The task's idempotent loop checks `Conditions[TestCancelled]` first; when set True (by ValidationOrchestrationReconciler's monitor-task-completion stop-on-failure path), the task deletes the Job, sets `Conditions[LoadComplete]=True, Reason=Cancelled`, and exits Complete. This is the cooperative-halt contract: actor reconcilers do NOT undo work already done; the chain teardown via cascade-delete handles cleanup.

### Cancellation contract

`metadata.deletionTimestamp != nil` triggers the standard finalizer flow. ValidationOrchestrationReconciler clears its finalizer last; the cascade-delete OwnerReferences purge SNDs/Job/ConfigMap.

`Conditions[TestCancelled]=True` is the **stop-on-failure** signal, set exclusively by ValidationOrchestrationReconciler's monitor-task-completion task when a stop-on-failure rule trips. Per-actor reconcilers (ValidationLoadGenerationReconciler) observe this condition on their next reconcile and halt cooperatively:

- They stop submitting new tasks (the predicate continues to fire; the reconciler's plan resolver sees `TestCancelled=True` and short-circuits the build of new plans).
- In-flight tasks proceed naturally to completion, but their `wait-job-terminal`-shaped tasks observe the cancellation and delete their owned Job to halt the actor's work.
- They do NOT undo work already done. Chain teardown via cascade-delete is the rollback when the Run itself is deleted.
- "Halt" is best-effort cooperative; the Run reaches `Phase=Failed`, NOT `Phase=Cancelled`. (`Cancelled` is reserved for `metadata.deletionTimestamp != nil`.)

This contract scales to v2: when ValidationSequenceReconciler observes `TestCancelled=True`, it stops applying new sequence steps; when ValidationChaosReconciler observes it, it stops scheduling new fault injections.

### Implementation invariants

These are runtime rules every controller and task implementation must follow. They are not visible in the CRD schema but are load-bearing for correctness.

1. **Transient errors return `RequeueAfter`, never `task.TerminalError`.** `monitor-task-completion` (Prometheus poller) and `wait-job-terminal` (Job watcher) are long-running; the executor's `MaxRetries` budget would mature transient errors into permanent failures. Implementation invariant: only schema-violation, RBAC-denial, and missing-resource errors are terminal in these tasks; everything else requeues.
2. **Plan-creation idempotency uses optimistic concurrency.** Status patches use resourceVersion-checked update, not blind SSA on the status subresource. Existing controllers (`SeiNodeReconciler`, etc.) already do this; both ValidationRun controllers reinforce the pattern. Two reconciles in the same generation that race on plan creation see a conflict; one retries.
3. **Each controller's status patch carries only its owned fields.** Field-manager isolation is preserved by listing only the fields the controller owns in its SSA patch — never echoing back the other controller's conditions or plan slot.
4. **No controller writes another controller's plan slot.** The executor is parameterized by slot; passing the wrong slot is a compile-time error in Go (different field type per slot).
5. **`spec.load == nil` short-circuits ValidationLoadGenerationReconciler.** The predicate gate is the primary defense; the resolver's `(nil, nil)` return is the secondary defense.
6. **`monitor-task-completion` treats absent `spec.load` as `Conditions[LoadComplete]=True` from t=0.** Rules-only Runs are well-formed; the orchestrator does not wait for a never-arriving load-complete signal.

## Validation rule semantics

(Concise restatement; see `monitor-task-completion` task above for operative detail and OTel Round 1 for rationale.)

- v1 evaluates rules continuously: each rule polls at `RunProperties.Interval` (default 30s, min 5s, max 5m) starting at `runStart + Interval`, with a final sweep at run terminal time before finalize aggregates.
- v1 types: `alert`, `query`. Future `container` reserved.
- Both types use the same Prometheus HTTP client (`internal/monitoring/prom.go`). One connection pool, one `otelhttp.NewTransport`, one set of metrics.
- Prometheus URL via env var (`VALIDATION_PROMETHEUS_URL`, default `http://prometheus-k8s.monitoring.svc:9090`) with `--prometheus-url` flag override.
- AND-of-rules pass semantics. OR/weighted aggregation explicitly out of scope.
- Per-rule `verdict ∈ {Passed, Failed, Awaited, Error}` (4-state). `Awaited` is the pre-first-evaluation state.
- `Failed` is monotonic per rule. `Error` is recoverable (decays to Passed/Failed; matures into permanent Error after `Retry` consecutive Errors).
- `StopOnFailure: true` on a rule that trips → ValidationOrchestrationReconciler sets `Conditions[TestCancelled]=True`; ValidationLoadGenerationReconciler halts; Run terminates Failed.

## Controller registration and opt-in deployment

The validation machinery — `ValidationOrchestrationReconciler`, `ValidationLoadGenerationReconciler`, and any future actor controllers — is **opt-in at deployment time**. The default `sei-k8s-controller` deployment runs `SeiNodeReconciler` and `SeiNodeDeploymentReconciler` only; cluster operators running production validators with Kubernetes do not need the validation slice.

Validation is opted in by chain developers and release engineers who own ephemeral test environments. When opted in, `ValidationOrchestrationReconciler` is the required orchestrator; opting into specific actor controllers (`ValidationLoadGenerationReconciler` in v1, future `ValidationSequenceReconciler` and `ValidationChaosReconciler` in v2) is independent and additive within the validation slice.

**The specific deployment-time opt-in mechanism is left to the implementation** — values flags, build tags, separate Deployment manifests, env-var-gated controller registration, or any combination. What this LLD locks in is the architectural property: each validation controller's `SetupWithManager` call is conditional on a deployment-time signal, v2 actor controllers register identically, and nothing in the existing `SeiNodeReconciler` / `SeiNodeDeploymentReconciler` code path depends on the validation controllers being registered.

Sketch (mechanism-agnostic):

```go
// cmd/main.go
if cfg.Validation.Enabled {
    if err := (&validationcontroller.ValidationOrchestrationReconciler{...}).SetupWithManager(mgr); err != nil {
        setupLog.Error(err, "Failed to create controller", "controller", "ValidationRun-Orchestration")
        os.Exit(1)
    }

    if cfg.Validation.LoadGenerationEnabled { // default-on within validation
        if err := (&validationcontroller.ValidationLoadGenerationReconciler{...}).SetupWithManager(mgr); err != nil {
            setupLog.Error(err, "Failed to create controller", "controller", "ValidationRun-LoadGeneration")
            os.Exit(1)
        }
    }

    // v2 actor controllers register identically with their own opt-in signals.
    // Until v2 ships, attempting to enable Sequence or Chaos errors at startup.
}
```

The "v2 expansion is purely additive" claim becomes a deployment-config-level invariant: adding `ValidationSequenceReconciler` is a new conditional `SetupWithManager` block plus an opt-in signal, with admission un-rejecting `spec.sequence` and a new `status.plans.sequence` slot. Existing v1 controllers (`ValidationOrchestrationReconciler`, `ValidationLoadGenerationReconciler`) do not change.

## Observability

Per OTel Round 1, with cardinality discipline. Metrics are emitted by both reconcilers; the controller name in metric labels distinguishes them.

### Controller metrics (emitted from the reconciler binary)

| Instrument | Type | Labels | Cardinality | Owner |
|---|---|---|---|---|
| `validationrun_phase_transitions_total` | Counter | `from`, `to`, `reason` | ~200 series | ValidationOrchestrationReconciler |
| `validationrun_active` | UpDownCounter | `phase` | 5 series | ValidationOrchestrationReconciler |
| `validationrun_duration_seconds` | Histogram (`30,60,300,600,1800,3600,7200`) | `terminal_phase`, `actor` | bounded | ValidationOrchestrationReconciler |
| `validation_rule_evaluation_duration_seconds` | Histogram (`0.05,0.1,0.5,1,5,10,30`) | `type`, `verdict` | 12 series | ValidationOrchestrationReconciler |
| `validation_rule_evaluations_total` | Counter | `type`, `verdict`, `reason` | bounded | ValidationOrchestrationReconciler |
| `validation_prometheus_query_errors_total` | Counter | `endpoint`, `error_type` | ≤8 series | ValidationOrchestrationReconciler |
| `validationrun_loadgen_jobs_terminal_total` | Counter | `exit_code` (0/1/2/timeout) | 4 series | ValidationLoadGenerationReconciler |
| `validationrun_loadgen_active_jobs` | UpDownCounter | (none) | 1 series | ValidationLoadGenerationReconciler |
| `validation_run_terminal_total` | Counter | `namespace`, `name`, `verdict` | **per-run** — see note below | ValidationOrchestrationReconciler |

`actor` label values: `load`, `sequence`, `chaos`, `rules-only` (and combinations joined by `+`, e.g., `load+rules`). Bounded at ~8 distinct values.

**Note on `validation_run_terminal_total` cardinality.** `name` is unbounded and would normally be a hard reject. Exception: this is the heartbeat-alert input (per platform-engineer), which queries `increase(... [24h])`. The cardinality is bounded by the alerting path's retention. If/when Prometheus pressure shows, drop `name` and switch heartbeat to a `validation_run_terminal_total{namespace, verdict}` aggregate. Document the upgrade path; ship the loud version first.

**Note on `mode` label.** Round 1's OTel sketch carried a `mode` label on rule-evaluation metrics. v1 has no `mode` field on the wire (continuous is the only behavior), so the label is dropped. Re-add with care for cardinality if a future mode lands.

**Labels rejected (cardinality bombs):** `chain_id`, `image_sha`, `run_id`. These go into trace attributes and structured log fields, not metrics.

### Traces

One span per reconcile per controller (`controller.kind=ValidationRun, controller.name=validationrun-orchestration|loadgeneration` + standard controller-runtime attributes). Child span per planner task. `monitor-task-completion`'s per-iteration loop emits one span per Prometheus query. Use `otelhttp.NewTransport` on the Prometheus client (single-flight client lives in `internal/monitoring/`). Inject `traceID`/`spanID` into structured logs.

### Structured-report aggregation

**S3-only in v1.** No Pushgateway, no remote_write of report metrics, no `.status.report.raw`. Reasons (per OTel Round 1):

- Pushgateway is the wrong tool for batch-job metrics (Prometheus docs explicitly warn).
- The report shape isn't yet stable across `seiload` and `qa-testing`; locking before two real consumers is a trap.
- Cardinality of `{run_id, image_sha, profile}` is the bomb the controller avoids.
- `.status.report.raw` (a CEL-capped echo of the termination message) was cut at the Round 2 gate. Consumers fetch from `status.report.s3Url`; the URL is programmatically derivable from the bucket layout.

**Un-defer when:** ≥2 distinct report consumers want the same numeric AND a stable report-schema field has shipped. Add a separate `ReportExporter` controller that subscribes to terminal Runs and emits curated low-cardinality metrics. *Not* the Run controllers' job.

## RBAC and tenancy

### Controller-side ClusterRole (generated from kubebuilder markers)

The two controllers share one ClusterRole — they live in one binary and one ServiceAccount. Markers go on each reconciler's package, but verbs are unioned at generation time.

```go
// Both controllers
// +kubebuilder:rbac:groups=validation.sei.io,resources=validationruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=validation.sei.io,resources=validationruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=validation.sei.io,resources=validationruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=validation.sei.io,resources=validationsuites;validationschedules,verbs=get;list;watch
// +kubebuilder:rbac:groups=validation.sei.io,resources=validationsuites/status;validationschedules/status,verbs=get
// ValidationOrchestrationReconciler
// +kubebuilder:rbac:groups=sei.io,resources=seinodedeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodedeployments/status,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules,verbs=get;list;watch
// ValidationLoadGenerationReconciler
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch
// Both
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
```

ValidationLoadGenerationReconciler deletes Jobs (cooperative cancel for stop-on-failure and run-duration timeout); `delete` verb on `batch/jobs` is load-bearing. SNDs are deleted only via cascade on Run delete — controller-initiated SND delete is reserved for finalizer cleanup.

**Not granted:** `secrets` write, `rbac.*`, `eks.*` (pod-identity AWS calls), **`pods/exec`** (one-way-door — see Resolved decisions). Per platform-engineer: controller never mints SAs or PIAs.

### Tenant-side ClusterRole bundle (shipped in controller's manifests; bound per-namespace by tenants)

- `validation-tenant-author`: full CRUD on `validation.sei.io/*` + `seinodedeployments` + view on rule-eval status.
- `validation-tenant-viewer`: get/list/watch on validation kinds only.

### CEL admission rules (CRD-level)

- Same-namespace enforcement on every reference except `alert.ruleRef.namespace` (which is allowed cross-namespace into label-allowlisted namespaces only — checked at admission via webhook because CEL can't see namespace labels).
- `chain.deployments[role=fullNode].spec.genesis` forbidden (full nodes inherit; CEL on the list-map).
- `chain.deployments` exactly one `role=validator` and ≥1 `role=fullNode` (CEL on the list-map).
- `chain.deployments[].spec.template.spec.{validator|fullNode}` if set must align with role; if unset, controller injects `{}`.
- Reserved env-var names rejected on `spec.load.workload.env` via the `WorkloadSpec` `XValidation` rule.
- Rule names unique within `spec.rules` (list-map convention).
- At least one of `spec.load`, `spec.sequence`, `spec.chaos` must be set OR `spec.rules` non-empty.
- `spec.sequence` and `spec.chaos` admission-rejected in v1.
- Spec mutation: substantive fields immutable on a Run after creation (CEL: `self == oldSelf` on `spec.chain`, `spec.load`, `spec.rules`, `spec.sequence`, `spec.chaos`). Only metadata, finalizers, and status mutate.

### Workload SA convention

Tenant pre-provisions SA named `{namespace}-runner` (override via `spec.load.workload.serviceAccountName`). ValidationLoadGenerationReconciler validates existence at apply-job; missing SA terminates with `Reason=ServiceAccountMissing`. Controller never creates SAs.

## IRSA / Pod Identity / S3

Per platform-engineer Round 1:

- Pod Identity Association lifecycle: Terraform, **per-namespace**, never per-Run. Adding a tenant = ~30-line Terraform block in `terraform/aws/.../harbor/validation.tf`.
- One IAM Role per namespace → one S3 prefix.
- **Bucket layout LOCKED:**
  ```
  s3://harbor-validation-results/{namespace}/{job}/{runId}/
    report.log
    report.html        # qa-testing mochawesome
    report.junit.xml
    termination-message.json
    rules/{ruleName}.json
  ```
  `{job}` is `spec.load.workload.name` (or `"rules-only"` for rules-only Runs that skip the workload). New components only by *appending* — no new path levels.
- Workloads upload via Pod Identity (already wired for nightly). Controller never writes to S3, never reads from S3, only computes the URL.

## Migration: nightly GHA → ValidationRun

Per platform-engineer Round 1:

- `clusters/harbor/nightly/` Flux config is **structurally unchanged**. Same namespace, SAs, RBAC, PodMonitors. Pod labels (set by controller from `spec.load.workload.podTemplate` plus chain-id + run-id + deployment-name) keep selector compatibility.
- `.github/workflows/k8s_nightly.yml` shrinks ~50%: each `kubectl wait`/`kubectl exec`/`envsubst`/`kubectl logs`/`aws s3 cp` step becomes a controller plan task (split across the orchestration plan and the loadgeneration plan). The workflow becomes:
  1. `cat <<EOF | envsubst | kubectl apply -f -` (one ValidationRun manifest)
  2. `kubectl wait --for=condition=Succeeded validationrun/${RUN_ID} --timeout=2h`
  3. Read `.status.report.s3Url` and `.status.verdict` for the GHA summary; pull the report artifacts from S3 directly.
- The race against `sei-chain` CI moves into the controller's `ensure-chain` task.
- `templates/seinodedeployment.yaml` and `templates/seiload-job.yaml` retire (subsumed into `ValidationRun.spec`). Both fleets (validators and fullNodes) lift into `chain.deployments[]` directly — the prior YAML's two SND templates become two list entries.

Migration is opt-in per tenant: nightly can flip to ValidationRun while qa-testing still runs Phase 1's GHA-orchestrated bash, and vice versa. The Phase 1 contract guarantees the workload manifests remain identical.

The plan-task labels in this migration narrative align with the new task split:

| Old workflow step | New plan task | Plan slot |
|---|---|---|
| `kubectl apply -f seinodedeployment.yaml` (×2) | `ensure-chain` | orchestration |
| `kubectl wait --for=condition=Ready snd/...` | `wait-chain-ready` | orchestration |
| `kubectl get services -l sei.io/nodedeployment=...` | `resolve-endpoints` | orchestration |
| (gate flip — implicit in old workflow) | `mark-test-running` | orchestration |
| `envsubst | kubectl apply -f profile.cm.yaml` | `render-config` | loadgeneration |
| `kubectl apply -f seiload-job.yaml` | `apply-job` | loadgeneration |
| `kubectl wait --for=condition=complete job/...` + `kubectl logs` tail | `wait-job-terminal` | loadgeneration |
| (rule polling — new) | `monitor-task-completion` | orchestration |
| `aws s3 cp` summary upload (workload-side via Pod Identity) | URL stamped at `finalize` | orchestration |

## Six open questions from #139 — answers

| # | Question | Decision | Source |
|---|---|---|---|
| 1 | Embed workload spec, or reference a separate `ValidationWorkload` CR? | **Embed.** Reserve `spec.runRef` field name only. | PM Round 1 Q1 |
| 2 | Probe `mode` enum: 3-mode, 5-mode, or defer? | **Defer.** v1 has no `mode` field; only behavior is continuous polling. | PM Round 1 Q2 + user input |
| 3 | `ValidationSuite`'s own scheduler vs `ValidationSchedule`-only? | **Moot in v1** — both deferred. Schedule is the lever; Suite is a child orchestrator. | PM Round 1 Q3 |
| 4 | S3 upload: top-level field, sidecar, or controller-side? | **Workload-side via Pod Identity.** Controller stamps URL only. | PM Round 1 Q4 + Phase 1 contract |
| 5 | Kueue integration day 1? | **No.** Defer until ≥3 concurrent suites contend. | PM Round 1 Q5 |
| 6 | Numeric vs stringified result aggregation? | **Stringified.** Adding numerics later is additive; removing them is breaking. | PM Round 1 Q6 + OTel one-way doors |

The Round 3 architectural pass also resolved one unstated question: **how to orchestrate "chain bring-up + workload + rules" without a barrier task**. Answer: predicate-gated event delivery on a second sub-controller; `Conditions[TestRunning]` is the gate. (See Resolved decision #19.)

## Five one-way-door warnings from OSS survey — how design avoids each

1. **Don't bake workload I/O abstractions into the Run (Tekton's `PipelineResource`).** ValidationRun has no typed I/O abstraction. The workload contract is env vars + exit codes + termination message + S3 — text-shaped, not typed Kubernetes objects. Avoided.
2. **Don't lowercase your phase enum (Testkube's `passed`/`failed`).** All phase and verdict enums are Pascal-case. Tekton/Argo precedent. Avoided.
3. **Don't pick a kind name twice (K6's `K6→TestRun`, Litmus's `Experiment→Fault`).** ValidationRun / ValidationSuite / ValidationSchedule chosen with explicit OSS-precedent alignment. Composable-blocks shape (`load`, `sequence`, `chaos`) absorbs new test shapes inside one kind name. Avoided.
4. **Don't proliferate per-workload-type CRDs (Testkube's `TestWorkflow` consolidation).** One CRD with composable optional blocks. Per-runner-type kinds explicitly rejected. Avoided.
5. **Don't orchestrate complex DAGs in your suite CR (Litmus outsourced to Argo).** `ValidationSuite` (when implemented) is flat — sequential or parallel, with `stopOnFailure`. Users needing DAG wrap suites in Argo. Documented as non-goal. Avoided.

## Resolved one-way-door decisions

These are decisions baked into the LLD that the council orchestrator surfaced for explicit user approval. Round 2 closed 2026-04-28; Round 3 architectural-refinement pass added rows 19–24 and modified rows 3, 4, 18 on 2026-04-29.

| # | Decision | Rationale | Reverse cost |
|---|---|---|---|
| 1 | API group **`validation.sei.io`** (separate from `sei.io`) | Validation versions independently of node-platform CRDs | High — group rename = full migration |
| 2 | **Composable optional actor blocks** (`spec.load`, `spec.sequence`, `spec.chaos`) **instead of `spec.type` discriminator** | Real test shapes mix actors; discriminator was a one-way door | High — schema migration; v1 ships composable from start |
| 3 | **`chain.deployments[]` list of named SeiNodeDeployment configs** with `role: validator|fullNode` | Generalizes the "validator + fullNodes" topology; absorbs heterogeneous fullNode fleets without new top-level fields | High — list shape rename = stored-object migration |
| 4 | **Exactly one `role: validator` deployment, ≥1 `role: fullNode` deployment.** Workload always connects to fullNode-role endpoints. **No `endpointPolicy` knob — permanently dropped.** | Two-fleet topology codifies what both real consumers run; one validator + N fullNodes generalizes; removing endpointPolicy eliminates a one-way enum | Medium — adding a validator-only or selectable mode later requires new shape |
| 5 | **`Failed` vs `Error` distinction** at phase level (heartbeat ignores Error) | Two terminal failure modes carry different operational signals | Low (additive) |
| 6 | **Bucket prefix `s3://harbor-validation-results/{namespace}/{job}/{runId}/`** locked verbatim | Phase 1 already wires this | High — bucket migration |
| 7 | **No Flux labels on owned children** (controller invariant) | Flux would prune controller-managed objects | High — Flux drift incident |
| 8 | **Tenant pre-provisions SAs**; controller never IAM | Stay off the IAM control plane | High — auth model creep |
| 9 | **`alert.ruleRef` cross-namespace** allowed only into label-allowlisted namespaces | Multi-tenancy by namespace; controller-mediated escape hatch | Low (additive) |
| 10 | **Reserved env-var names rejected at admission via CEL XValidation** on `WorkloadSpec.env` | Hard reject is deterministic; avoids the silent-override surprise | Low (additive — names list extends additively) |
| 11 | **`query.threshold` typed as `string`** with regex CEL | OTel Round 1 schema-typing; switch numeric→string is breaking | High |
| 12 | **No `mode` field on `ValidationRule` in v1** | Avoids locking in an enum value before semantics solidify | Low (additive) |
| 13 | **`runProperties.interval` (default 30s, min 5s, max 5m)** is load-bearing for continuous polling | Polling cadence per rule | Low (additive — bounds may relax) |
| 14 | **`runProperties.stopOnFailure` implemented in v1** (default `false`); orchestrator sets `Conditions[TestCancelled]=True` and actor reconcilers halt cooperatively | Real fast-fail path; no barrier task needed | Low (additive — semantics expansion) |
| 15 | **Five conditions: `TestRunning`, `TestComplete`, `Succeeded`, `TestCancelled`, `LoadComplete`** with single-writer field-manager isolation | Gate-flip + finalize + Tekton-style pass/fail + cross-controller cancel signal + actor-side complete signal | Low (additive — extra conditions stay additive) |
| 16 | **`status.report.raw` cut entirely.** S3 URL is the only artifact pointer in `.status`. | Bounded `.status` size; URL is programmatically derivable | Low (additive — re-add capped raw if a consumer demands without S3 access) |
| 17 | **Spec immutability** (CEL `self == oldSelf` on `spec.chain`, `spec.load`, `spec.rules`, `spec.sequence`, `spec.chaos`) | Re-runs are new ValidationRun resources | Medium — relaxation requires versioning |
| 18 | **Two plans per Run, partitioned across `.status.plans.<controllerName>` slots.** Orchestration plan: `ensure-chain → wait-chain-ready → resolve-endpoints → mark-test-running → monitor-task-completion → finalize` (6 tasks). LoadGeneration plan: `render-config → apply-job → wait-job-terminal` (3 tasks). | Each controller owns its own plan; no cross-controller plan mutation; v2 actor controllers append plan slots additively | Medium — collapsing back to one plan would require status migration AND re-introducing a barrier task |
| 19 | **Two cooperating sub-controllers in one binary** (`ValidationOrchestrationReconciler` required when validation is enabled, `ValidationLoadGenerationReconciler` opt-in default-on within the validation slice). v2 actor controllers register identically. | Each controller is independently testable, opt-out-able, and v2-extensible. Replaces the prior single-controller-with-7-tasks design. | High — unwinding to one controller would re-introduce the cross-actor-orchestration problem composable blocks are designed to absorb |
| 20 | **Predicate-gated event delivery** on `ValidationLoadGenerationReconciler` (`spec.load != nil AND Conditions[TestRunning]=True`). No barrier task; controller-runtime native gating. | Idiomatic; survives controller restart; no synchronization primitive required | Low (additive — predicate refines additively) |
| 21 | **Field-manager isolation per controller** (`validationrun-orchestration`, `validationrun-loadgeneration`). Single-writer table enforces no two controllers ever write the same condition or status field. | SSA preserves both partitions correctly; concurrent writes are conflict-free by construction | Low (additive — adding fields keeps isolation) |
| 22 | **`TaskPlan.TargetPhase` ignored by both ValidationRun controllers** (typed as `SeiNodePhase`, doesn't fit ValidationRunPhase). Phase transitions owned exclusively by orchestration plan's `finalize` task. | Avoids forcing a `TaskPlan` schema migration in v1; phase logic centralizes in one place | Medium — if v2 needs typed TargetPhase across actor plans, file the SND/SeiNode-side common-types update |
| 23 | **Deployment-time opt-in pattern** for validation controllers; specific mechanism (values flags, build tags, env vars, separate Deployment manifests) deferred to implementation. v1 binary errors at startup if `sequence` or `chaos` is signaled enabled. SeiNode / SeiNodeDeployment controllers are the always-on default; the validation slice is layered on top. | Makes "v2 expansion is additive" a deployment-config-level invariant; node operators running production validators don't carry validation machinery | Low (additive — new actor opt-in signals append) |
| 24 | **`pods/exec` REJECTED for v2 sequence steps.** When ValidationSequenceReconciler ships, sequence steps that submit chain transactions do so via short-lived `Job`s targeting RPC, NOT via `kubectl exec` into validator pods. Controller's RBAC will not include `pods/exec` ever. | `pods/exec` is a privileged escape hatch with audit-trail and security implications. Job-based sequence steps reuse the same Pod Identity / RBAC story already established. | Very High — adding `pods/exec` later requires a separate security review and an explicit RBAC promotion |
| 25 | **Chaos plan namespace-label gate (v2).** When ValidationChaosReconciler ships, chaos-mesh CRDs in tenant namespaces require namespace label `sei.io/chaos-allowed=true` (admission-webhook-enforced) AND controller built with `--enable-chaos-plan` (compile-time opt-in for v1 deployments). | Chaos is a privileged operation; namespace-label is the multi-tenancy gate; compile-time flag prevents accidental enablement | Low (additive — relaxing the label gate is an additive admission-rule change) |
| 26 | **`status.failedPlan` field reserved in v1, populated by orchestration's `finalize` task.** Stamps `"loadGeneration"` (or `""` for orchestration's own ruling) on terminal Failed/Error in v1. v2 reconcilers stamp `"sequence"` / `"chaos"` without schema churn. | 2am on-call sees at a glance which actor caused the run to fail; v2-additive | Low (additive — new actor names append) |

## Open dependencies

These are companion sub-issues the LLD discovers. **None block v1 ValidationRun.**

1. ~~**SND-readiness includes catching_up.**~~ **Resolved — already implemented.** The `seid:RPC_PORT/lag_status` readiness probe wired at `internal/noderesource/noderesource.go:366-377` (probing sei-tendermint's threshold-configured endpoint) gates Pod readiness on chain catch-up; SND `phase=Ready` waits via `ConditionNodesReady` aggregation. `sei-protocol/sei-k8s-controller#144` closed-as-already-implemented. Original draft of this LLD specified a multi-day seictl sidecar `/health` extension based on incorrect investigation; the existing seid-side mechanism is functionally equivalent and architecturally cleaner (no sidecar↔seid hop).
2. **`TaskPlan.TargetPhase` typing.** v1 sidesteps by leaving the field empty in both ValidationRun controllers' plans and centralizing phase transitions in `finalize`. Re-evaluate when v2 lands a third actor plan that wants phase-shape parity with SND/SeiNode plans.
3. **SND inline `genesis` rejection on full-node-role SND.** ValidationRun's CEL rejects `chain.deployments[role=fullNode].spec.genesis`, but the SND controller itself does not enforce "full-node SND has no genesis ceremony" — it currently relies on the user not providing it. File a small SND-side validation tightening to mirror.
4. **PodMonitor for `sei-k8s-controller` itself.** Currently absent from `clusters/harbor/sei-k8s-controller/`. Add `:8080/metrics` plaintext PodMonitor; the new controller-side metrics in this LLD assume it. Per platform-engineer Round 1.
5. **Heartbeat PrometheusRule.** One global `clusters/harbor/monitoring/alerts/validation-heartbeat.yaml` querying `validation_run_terminal_total`. Per platform-engineer Round 1 — generic across tenants.
6. **`validation` shared-rules monitoring namespace label.** `monitoring/` namespace gets `sei.io/validation-shared-rules=true` so `alert.ruleRef.namespace=monitoring` works. Single-line label add.
7. **Shard env-var injection mechanism.** `SHARD_INDEX`/`SHARD_COUNT` per pod via Indexed-Job (`spec.completionMode=Indexed`) with `JOB_COMPLETION_INDEX` mapped to `SHARD_INDEX` via env-from-fieldRef. Already a Phase 1 contract reservation.
8. **Controller registration opt-in plumbing.** Deployment-time mechanism (values flags, env vars, build tags, separate Deployment manifests, or any combination) for gating each validation controller's `SetupWithManager` call. Specific mechanism TBD — not shipping a Helm chart in v1; lands alongside the v1 controller manifest delivery.

## Future work

Each future item names the trigger that un-defers it.

| Item | Un-defer trigger |
|---|---|
| **`ValidationSequenceReconciler` sub-controller** | First Cosmos-SDK upgrade test that needs ordered state changes. Lands as additive sub-controller; deployment-time opt-in signal; admission un-rejects `spec.sequence`; new `status.plans.sequence` slot. Sequence steps submit via short-lived Jobs (not `pods/exec`). |
| **`ValidationChaosReconciler` sub-controller** | First chaos-mesh-driven failure-injection test. Lands as additive sub-controller; deployment-time opt-in signal + compile-time `--enable-chaos-plan` flag + tenant namespace label `sei.io/chaos-allowed=true`. New `status.plans.chaos` slot. |
| `ValidationSuite` reconciler | First multi-Run flow that one human launching kubectl can't drive (≥3 chained Runs in CI). |
| `ValidationSchedule` reconciler | Tenant wants to retire their GHA cron. Track first request. |
| `container` rule type | Real heuristic ships outside Prometheus (e.g., balance reconciliation across two RPCs). |
| `window-end` rule mode (single instant query at run end) | Use case where continuous polling cost is undesirable AND the result is only meaningful at run end. Re-add `mode` field with `continuous` default. |
| `edge` rule mode (start-vs-end snapshot) | Spec-vs-end snapshot use case. |
| **`status.failedPlan` populated in ValidationOrchestrationReconciler when v2 actor plans land** | Already reserved in v1 schema; v1 stamps `"loadGeneration"` only. v2 reconcilers stamp `"sequence"` / `"chaos"` for at-a-glance fault attribution. |
| `ReportExporter` controller (Pushgateway-shaped) | ≥2 consumers want the same numeric AND a stable report-schema field ships. |
| Kueue admission control | ≥3 simultaneous suites contending; Prometheus shows queueing. |
| `spec.runRef` (separate `ValidationDefinition` CR) | ≥10 distinct Runs sharing a workload definition; ConfigMap reuse insufficient. |
| Per-PR `ValidationTrigger` event-driven CR | CI cost budget approved; Testkube-style triggers explicitly demanded. |
| Numeric `actualValue`/`threshold` typed fields on rule status | Regression-detection consumer ships and benefits from type safety. |
| Multi-workload composition in one Run | Cross-workload barrier/sync requirements emerge. |
| Live mid-run RPC fleet refresh | HPA on RPC fleet during a run becomes desired. |
| Validator-only chain topology (no fullNode-role deployments) | Consensus-poking workload demands it AND ≥2 consumers justify the operational cost of optional-fullNodes mode. |

**v2 expansion is purely additive — by design.** Adding a sequence or chaos sub-controller does not refactor ValidationOrchestrationReconciler or ValidationLoadGenerationReconciler. The integration touch-points are: (a) new deployment-time opt-in signal, (b) admission un-rejects the corresponding `spec.<actor>` block, (c) new `status.plans.<name>` slot, (d) new condition `<Actor>Complete` written by the new controller via its own field manager, (e) the new controller's predicate watches `Conditions[TestRunning]=True` exactly like ValidationLoadGenerationReconciler does. No existing controller code changes.

### Argo Workflows re-evaluation triggers

The /coral subcouncil unanimously concluded to ship the custom CRD over Argo Workflows for v1 — the per-Run cost of a custom controller is small, and the surface area Argo would absorb (chain-readiness orchestration, structured rule verdicts, S3 URL stamping, stop-on-failure semantics) is the surface area the validation domain wants to own first-class. The Argo conversation should re-open under any of the following triggers:

- **Argo Workflows lands on Harbor for unrelated reasons** (data pipelines, CI consolidation). Marginal cost of a `WorkflowTemplate` + `CronWorkflow` drops to ~zero; `ValidationSchedule` becomes a free wrapping layer.
- **A fourth distinct test-shape with real DAG needs lands** (chaos-mesh-driven sequences, multi-cluster fanouts). DAG-shaped orchestration is what Argo is genuinely better at.
- **`ValidationSuite` needs DAG semantics, not flat sequential/parallel orchestration.**
- **300-pod polling cost becomes a non-issue (i.e., we drop continuous mode and revert to window-end only).** The custom controller's main value-add over Argo is the per-rule polling loop with stop-on-failure; if the polling story collapses to a single instant query, Argo's `Workflow` shape becomes broadly competitive.

If any of these trigger, the conversation re-opens with a fresh /coral pass — not a unilateral pivot.

## References

### Round 0 + Round 1 + Round 2 + Round 3 artifacts (this council cycle)

- `sei-protocol/sei-k8s-controller#139` — entry brief, OSS survey, six open questions, five one-way-door warnings (`/tmp/validationrun-issue-body.md`)
- `sei-protocol/platform#235` — Phase 1 workload contract issue (`/tmp/phase1-issue-body.md`)
- Round 1 PM scope cuts (`/tmp/round1/pm-scope-cuts.md`)
- Round 1 OTel rule semantics + observability (`/tmp/round1/otel-rule-semantics.md`)
- Round 1 platform-engineer Harbor integration (`/tmp/round1/platform-integration.md`)
- Round 2 user gate decisions (closed 2026-04-28) — encoded in the "Resolved one-way-door decisions" table rows 1–18
- Round 3 architectural-refinement pass (closed 2026-04-29) — encoded in rows 2, 3, 4, 18, 19–26 above

### In-repo (sei-k8s-controller)

- `/Users/brandon/sei-k8s-controller/CLAUDE.md` — controller patterns, RBAC marker convention, plan-driven reconciler
- `/Users/brandon/sei-k8s-controller/internal/planner/doc.go` — plan lifecycle, condition ownership, single-patch model
- `/Users/brandon/sei-k8s-controller/internal/planner/{planner.go,group.go,full.go,validator.go,replay.go,executor.go}` — existing builder idiom; `validationrun_orchestration.go` and `validationrun_loadgen.go` will follow the same pattern
- `/Users/brandon/sei-k8s-controller/internal/task/{await_nodes_running.go,deployment.go,observe_image.go,task.go}` — controller-side task pattern, deserialize-map registration
- `/Users/brandon/sei-k8s-controller/api/v1alpha1/{seinode_types.go,seinodedeployment_types.go,common_types.go,validator_types.go,full_node_types.go,replayer_types.go}` — existing CRD type idioms (CEL XValidation, kubebuilder markers, listType=map listMapKey)
- `/Users/brandon/sei-k8s-controller/cmd/main.go` — controller registration pattern (the new `ValidationOrchestrationReconciler` and `ValidationLoadGenerationReconciler` register here, the latter behind `cfg.LoadGenerationEnabled`)
- `/Users/brandon/sei-k8s-controller/docs/design/composable-genesis.md` — LLD style template

### In-repo (platform)

- `/Users/brandon/platform/clusters/harbor/nightly/templates/seinodedeployment.yaml` — the working two-SND topology this design generalizes into `chain.deployments[]`
- `/Users/brandon/platform/.github/workflows/k8s_nightly.yml` — bash glue this CRD subsumes

### OSS survey — direct precedents (only patterns cited above)

- Tekton TaskRun docs — `Succeeded` condition shape — https://github.com/tektoncd/pipeline/blob/main/docs/taskruns.md
- Tekton TEP-0074 — PipelineResource deprecation (one-way-door warning) — https://github.com/tektoncd/community/blob/main/teps/0074-deprecate-pipelineresources.md
- Argo CronWorkflow — `ValidationSchedule` shape — https://argo-workflows.readthedocs.io/en/latest/cron-workflows/
- Litmus probes overview — `ValidationRule` analog (interval, runProperties) — https://docs.litmuschaos.io/docs/concepts/probes
- K6 Operator TestRun — execution-segment sharding via env injection — https://github.com/grafana/k6-operator/blob/main/api/v1alpha1/testrun_types.go
- Chaos Mesh Schedule types — annotation-based pause pattern — https://github.com/chaos-mesh/chaos-mesh/blob/master/api/v1alpha1/schedule_types.go
- Testkube TestExecution types — what NOT to do (lowercase enum, per-runner CRDs) — https://github.com/kubeshop/testkube-operator/blob/main/api/tests/v3/test_types.go
- Sonobuoy plugins doc — done-file convention (referenced for future `container` rule type) — https://github.com/vmware-tanzu/sonobuoy/blob/main/site/content/docs/main/plugins.md
- controller-runtime `WithEventFilter` predicate pattern — predicate-gated reconciliation idiom (used for ValidationLoadGenerationReconciler's `Conditions[TestRunning]` gate) — https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/predicate
