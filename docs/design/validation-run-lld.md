# ValidationRun — Composable Validation Workloads against Ephemeral Sei Chains (LLD)

## Status

Draft — under council review (`sei-protocol/sei-k8s-controller#139`). Council gate closed 2026-04-28. Cross-review pending; PR open against this repo for review.

This LLD is the substrate for one or more handoff implementation issues. It does **not** itself authorize implementation — it is the artifact the gate reviews.

## Problem

Three differently-shaped validation workloads coexist on the Harbor cluster today with no shared abstraction: the seiload nightly performance run, the qa-testing TS/mocha suite (Phase 1 target), and the in-pod `SeiNode.spec.replayer` shadow-result sidecar. Only the third is first-class on the control plane; the first two are bash glue around `kubectl`, `envsubst`, `kubectl wait`, `kubectl exec seid status`, `aws s3 cp`. The bash pattern's documented sharp edges (missing `OwnerReferences`, no heartbeat, no structured report, race against `sei-chain` CI handled in shell) are symptoms of orchestrating outside the controller boundary; they don't grow into an abstraction, they compound as more workload shapes (fuzzers, soak, chaos suites, pectra-upgrade tests) come online.

Critically, "validation" in this context isn't just "did the workload exit 0." Many failure modes are infrastructure-level invariants the workload can't see: validators dropped peer connections mid-run, `seid` memory grew unbounded, an alert fired during the load window. The current pattern can't express **observability-as-test-oracle** — composing workload signal with alert/PromQL signal into a single pass/fail.

See `sei-protocol/sei-k8s-controller#139` for the full problem statement, OSS survey, and Phase 1 contract dependency. This LLD picks up where #139's open questions left off, integrating Round 1 specialist input and the user's directional decisions through the Round 2 gate close on 2026-04-28.

## Goals

1. **One CRD, one reconciler that drives a validation workload to completion against an ephemeral chain** — `ValidationRun` owns the SNDs that constitute its chain, the workload `Job`, and the verdict aggregation. No external orchestrator required.
2. **Workload contract parity with Phase 1** — `ValidationRun.spec.loadTest.workload` adopts the env-vars / exit-codes / termination-message / S3 contract from `sei-protocol/platform#235` verbatim. A Job manifest that runs under Phase 1 GHA glue runs unchanged under the controller.
3. **Observability-as-test-oracle** — the verdict is `workload_exit_code AND ⋀rules`, where rules are typed (`alert` and `query` in v1) and evaluated continuously over the load window with a deterministic, auditable Prometheus contract. v1 ships continuous polling with stop-on-failure as a first-class behavior.
4. **GitOps- and ad-hoc-friendly** — a `ValidationRun` is a self-contained, idempotent resource. Apply from GHA, Flux, kubectl, or a future bot; the controller takes it to a terminal phase exactly once.
5. **No cross-tenant blast radius** — same-namespace by construction (CEL-enforced) with one narrow exception (`alert.ruleRef` into namespaces labeled `sei.io/validation-shared-rules=true`). Tenants pre-provision SAs; controller is never an IAM controller.

## Non-goals

Adopted from #139 "Out of scope" plus the Round 1 + user refinements:

- **`ValidationSuite` and `ValidationSchedule` controllers.** Kind names are reserved (CRDs install with v1) and the types are sketched here so v1 `ValidationRun` doesn't paint them into a corner. No reconcilers in v1.
- **`SequenceTest` and `IntegrationTest` discriminator kinds.** v1 implements `LoadTest` only. The two future kinds are reserved one-paragraph each below to prove the discriminator union accommodates them.
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

## Architecture overview

```
                ┌─────────────────────────────────────────────────────────────┐
                │              ValidationRun  (validation.sei.io/v1alpha1)    │
                │  spec.type=LoadTest                                         │
                │  spec.chain.{validators,fullNodes}  spec.loadTest           │
                │  spec.rules[]  spec.results.s3  spec.timeouts               │
                │  status.phase  status.plan  status.rules[]  status.report   │
                └─────────────────────────────────────────────────────────────┘
                         │ owns (OwnerReferences, blockOwnerDeletion=true)
                         ▼
            ┌──────────────────────────┬──────────────────────────────────┐
            ▼                          ▼                                  ▼
   SeiNodeDeployment            SeiNodeDeployment                 batch/v1.Job
   {chainId}                    {chainId}-rpc                     {chainId}-{runId}
   role=validator               role=fullNode                     workload pod(s)
   genesis ceremony             peers→validators                  envs from contract
            │                          │                                  │
            └─── owns SeiNodes ────────┘                                  │
                                                                           ▼
                                                                  ConfigMap (rendered cfg)
                                                                  emptyDir (RESULT_DIR)
                                                                  S3 upload via Pod Identity

                ┌─────────────────────────────────────────────────────────────┐
                │  ValidationRun reconciler (plan-driven, internal/planner/)  │
                │  ResolvePlan → persist plan → ExecutePlan                   │
                │  Plan tasks (7):                                            │
                │    ensure-chain → wait-chain-ready → resolve-endpoints →    │
                │    render-config → apply-job → monitor-run → mark-done      │
                └─────────────────────────────────────────────────────────────┘
                         │                                       │
                         ▼                                       ▼
                Prometheus /api/v1/query                  workload Job + pod
                (instant query per rule per interval)     status (terminal-state poll)
```

The validator and fullNodes SNDs are **always both materialized**. There is no validator-only topology in v1. The workload always connects to fullNodes-fleet endpoints; this is a controller invariant, not a per-Run knob.

**Phase machine** (mirrors Tekton/Argo, capitalized — *not* Testkube's lowercase):

```
                           ┌──────────────┐
              ┌────────────│   Pending    │
              │            └──────┬───────┘
              │                   │ planner builds plan, persists, requeue
              │                   ▼
              │            ┌──────────────┐
              │            │   Running    │──── plan tasks execute ────┐
              │            └──────┬───────┘                            │
              │                   │                                    │
              │       ┌───────────┼─────────────┬────────────┐         │
              │       ▼           ▼             ▼            ▼         │
              │  ┌─────────┐ ┌─────────┐  ┌─────────┐  ┌─────────┐     │
              └─▶│Cancelled│ │Succeeded│  │ Failed  │  │  Error  │◀────┘
                 └─────────┘ └─────────┘  └─────────┘  └─────────┘
```

Terminal phases: `Succeeded` / `Failed` / `Error` / `Cancelled`. `Failed` = workload or rules said SUT misbehaved (test verdict). `Error` = controller couldn't ask (Prometheus 5xx, Job-create denial, infra failure that the workload signaled with exit code 2). The distinction matters for heartbeat alerting and for retry policy if/when added.

## CRD types

All three kinds live in `api/v1alpha1/` (group `validation.sei.io`, version `v1alpha1`). All three are namespaced.

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

// +kubebuilder:validation:XValidation:rule="self.type != 'LoadTest' || has(self.loadTest)",message="loadTest must be set when type=LoadTest"
// +kubebuilder:validation:XValidation:rule="self.type != 'SequenceTest' || has(self.sequenceTest)",message="sequenceTest must be set when type=SequenceTest"
// +kubebuilder:validation:XValidation:rule="self.type != 'IntegrationTest' || has(self.integrationTest)",message="integrationTest must be set when type=IntegrationTest"
// +kubebuilder:validation:XValidation:rule="(has(self.loadTest)?1:0) + (has(self.sequenceTest)?1:0) + (has(self.integrationTest)?1:0) <= 1",message="at most one of loadTest, sequenceTest, integrationTest may be set"
type ValidationRunSpec struct {
    // Type discriminates which body (loadTest / sequenceTest / integrationTest) applies.
    // Field name is `type`, not `kind`, to avoid visual collision with TypeMeta.kind.
    // +kubebuilder:validation:Enum=LoadTest;SequenceTest;IntegrationTest
    Type ValidationRunType `json:"type"`

    // Chain is the ephemeral Sei chain this run executes against.
    Chain ChainSpec `json:"chain"`

    // LoadTest is the body for type=LoadTest. Required iff Type=LoadTest.
    // +optional
    LoadTest *LoadTestSpec `json:"loadTest,omitempty"`

    // SequenceTest — reserved future kind; not implemented in v1.
    // Field reserved so the discriminator can expand additively.
    // +optional
    SequenceTest *SequenceTestSpec `json:"sequenceTest,omitempty"`

    // IntegrationTest — reserved future kind; not implemented in v1.
    // +optional
    IntegrationTest *IntegrationTestSpec `json:"integrationTest,omitempty"`

    // Rules is the set of typed validation rules evaluated alongside the
    // workload. Pass = workload exit 0 AND all rule verdicts == Passed.
    // +optional
    // +listType=map
    // +listMapKey=name
    // +kubebuilder:validation:MaxItems=32
    Rules []ValidationRule `json:"rules,omitempty"`

    // Results configures where artifacts are uploaded. The controller
    // does not upload — workloads upload via Pod Identity. The controller
    // computes and stamps `status.report.s3Url` for downstream consumers.
    // +optional
    Results *ResultsSpec `json:"results,omitempty"`

    // Timeouts bound the run's wall-clock at named lifecycle points.
    // +optional
    Timeouts *RunTimeouts `json:"timeouts,omitempty"`
}

// +kubebuilder:validation:Enum=LoadTest;SequenceTest;IntegrationTest
type ValidationRunType string

const (
    ValidationRunTypeLoadTest        ValidationRunType = "LoadTest"
    ValidationRunTypeSequenceTest    ValidationRunType = "SequenceTest"
    ValidationRunTypeIntegrationTest ValidationRunType = "IntegrationTest"
)
```

#### `ChainSpec` — embeds the `SeiNodeDeploymentSpec` shape

The user's most refined call. The controller materializes **two** `SeiNodeDeployment` children (one validator, one fullNodes) with OwnerReferences to the `ValidationRun`. Cascade delete works.

```go
// +kubebuilder:validation:XValidation:rule="self.chainId.matches('^[a-z0-9][a-z0-9-]*[a-z0-9]$')",message="chainId must be lowercase alphanumeric with hyphens"
// +kubebuilder:validation:XValidation:rule="!has(self.fullNodes.genesis)",message="chain.fullNodes.genesis must be unset; full nodes inherit genesis from the validator ceremony"
type ChainSpec struct {
    // ChainID for the ephemeral chain. The controller injects this into
    // every materialized SND's genesis and into every SeiNode's chainId.
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=64
    ChainID string `json:"chainId"`

    // Validators is the consensus-participating fleet. Required.
    // The controller materializes a single SeiNodeDeployment named {chainId}
    // in the same namespace, owned by this ValidationRun, with
    //   - genesis.chainId = chainId           (injected if unset)
    //   - template.spec.chainId = chainId     (injected if unset)
    //   - template.spec.validator: {}         (injected; user values overridden
    //                                          to enforce mode discriminator)
    //   - template.spec.peers: [{label: {selector: {sei.io/chain-id: chainId}}}]
    //                                          (injected if user did not specify)
    //
    // User-set fields take precedence on every field except validator-mode
    // discriminator and chainId — those two are controller-owned.
    Validators sei_v1alpha1.SeiNodeDeploymentSpec `json:"validators"`

    // FullNodes is the RPC/full-node fleet. REQUIRED in v1.
    // The controller materializes a second SeiNodeDeployment named
    // {chainId}-rpc, owned by this ValidationRun, with
    //   - template.spec.chainId = chainId     (injected if unset)
    //   - template.spec.fullNode: {}          (injected; mutually exclusive with validator)
    //   - template.spec.peers: [{label: {selector: {sei.io/nodedeployment: chainId}}}]
    //                                          (peer the validator SND, injected if unset)
    //   - genesis is forbidden on this fleet (CEL-enforced; full nodes inherit
    //                                          genesis from the validator ceremony)
    //
    // The workload always connects to the fullNodes fleet's headless Services.
    // There is no validator-fleet-endpoint mode in v1; consensus-poking workloads
    // (a hypothetical future need) must be filed as a separate concern.
    FullNodes sei_v1alpha1.SeiNodeDeploymentSpec `json:"fullNodes"`
}
```

CEL admission rules on `ChainSpec`:

- `validators.template.spec.validator` may be unset (controller injects); when set, must be `{}` (controller rejects user-injected non-empty validator config because it'd contradict the genesis-ceremony controller flow). Enforced via webhook (CEL on embedded structs is weak for this).
- `fullNodes.genesis` must be unset (full nodes inherit). CEL: `!has(self.fullNodes.genesis)`.
- `fullNodes.template.spec.fullNode` may be unset (controller injects); when set, must be `{}`.
- `validators.replicas >= 1`. `fullNodes.replicas >= 1` (the SND default of 1 makes this implicit).

##### Why embed instead of inline-template

The embedded `SeiNodeDeploymentSpec` has three properties the user explicitly wants:

1. **Field experience parity** — operators write the same `template.spec.image / entrypoint / overrides / dataVolume` shape they already know from the existing nightly template (`/Users/brandon/platform/clusters/harbor/nightly/templates/seinodedeployment.yaml`). One-shot migration: lift the YAML, drop into `chain.validators`, the controller fills in chainId/validator/peers.
2. **Future SND features come for free** — when SND grows new fields (e.g., a new genesis override), `ValidationRun` consumes them without LLD churn.
3. **Controller-injected fields are documented and overridable** — listing exactly what the controller touches (chainId, validator/fullNode discriminator, default peers) makes the contract auditable.

##### Why both fleets are required (not just validators)

Three things drove the gate decision to make `fullNodes` required:

1. **The workload always wants RPC parity with users.** Validators run with mempool admission rules and consensus duty cycles that surface latency artifacts unrelated to the SUT under load. Full nodes are the realistic RPC surface.
2. **Eliminating the optional path eliminates an `endpointPolicy` knob** that would otherwise be a one-way-door enum (default-flip risk, additive-only constraint). Two-fleet always = simpler RBAC discussion, simpler endpoint resolution, simpler architecture diagram.
3. **The two existing real consumers (nightly seiload and Phase 1 qa-testing) both run the two-fleet topology already.** Codifying it is descriptive, not prescriptive.

##### Trade-off (load-bearing)

This design **couples ValidationRun's CRD schema to SeiNodeDeployment's CRD schema**. SND breaking changes break ValidationRun. Mitigation: same controller binary owns both; they version together. If/when validation gets its own controller binary or separate version cadence, switch to a `ChainTemplate` projection that copies fields explicitly. Document this as a re-evaluation trigger (≥1 SND breaking change forced ValidationRun's hand).

#### `LoadTestSpec` — the v1 discriminator body

`spec.loadTest.workload` is the Phase 1 contract envelope, adopted **verbatim**.

```go
type LoadTestSpec struct {
    // Workload is the containerized load generator. The controller materializes
    // this as a batch/v1.Job named {chainId}-{runId-suffix} owned by the run.
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
// +kubebuilder:validation:XValidation:rule="!self.env.exists(e, e.name in ['CHAIN_RPC_URL','CHAIN_WS_URL','CHAIN_ID','RUN_ID','RESULT_DIR','DURATION_SECONDS','NAMESPACE','SHARD_INDEX','SHARD_COUNT'])",message="env names CHAIN_RPC_URL, CHAIN_WS_URL, CHAIN_ID, RUN_ID, RESULT_DIR, DURATION_SECONDS, NAMESPACE, SHARD_INDEX, SHARD_COUNT are reserved by the validation controller and must not be set in spec.loadTest.workload.env"
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
    // user attempt to set them. Authors get a deterministic admission error,
    // not a silent override or runtime surprise.
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
    // existence at the Pending → Running transition.
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
    // Files maps filename (key in the ConfigMap, mounted basename) to body.
    Files map[string]string `json:"files"`
    // MountPath defaults to /etc/validation/config when empty.
    // +optional
    MountPath string `json:"mountPath,omitempty"`
}

type WorkloadPodTemplate struct {
    // +optional
    NodeSelector map[string]string `json:"nodeSelector,omitempty"`
    // +optional
    Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
    // +optional
    SecurityContext *corev1.PodSecurityContext `json:"securityContext,omitempty"`
    // +optional
    Volumes []corev1.Volume `json:"volumes,omitempty"`
    // +optional
    VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`
    // +optional
    Affinity *corev1.Affinity `json:"affinity,omitempty"`
}
```

##### Reserved environment variables (controller-injected)

Adopted verbatim from Phase 1 (`/tmp/phase1-issue-body.md`):

| Var | Value |
|---|---|
| `CHAIN_RPC_URL` | First resolved RPC endpoint (always fullNodes fleet). Tendermint port. |
| `CHAIN_WS_URL`  | Same host, EVM WebSocket port (8546). |
| `CHAIN_ID` | `spec.chain.chainId` |
| `RUN_ID` | `metadata.name` of the ValidationRun (no UID — name is unique within ns) |
| `RESULT_DIR` | `/var/run/validation/results` (mounted emptyDir) |
| `DURATION_SECONDS` | `spec.loadTest.duration.Seconds()` |
| `NAMESPACE` | `metadata.namespace` |
| `SHARD_INDEX` | Injected per pod (0..replicas-1) via Pod-template field-ref expansion or downward API |
| `SHARD_COUNT` | `spec.loadTest.replicas` |

These names are CRD-rejected when supplied in `spec.loadTest.workload.env` — see the `XValidation` rule on `WorkloadSpec` above.

##### Exit-code semantics

Adopted verbatim from Phase 1:

- `0` → workload Succeeded (rules still gate the verdict)
- `1` → workload Failed (test failure; `verdict=Failed`, `Reason=WorkloadAssertionFailed`)
- `2` → infra-level failure (RPC unreachable, chain not ready); `verdict=Error`, `Reason=WorkloadInfraFailure`. Run terminates in `Error`, *not* `Failed`. Observability metric distinguishes; heartbeat alert ignores `Error` so blast radius is bounded.

#### `ValidationRule` — the alert + query schema

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

    // Type discriminates the rule body.
    // v1 implements: alert, query.
    // Reserved for future expansion: container.
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
    // RuleRef points at a monitoring.coreos.com/v1.PrometheusRule.
    // Name and Namespace are advisory (they help auditability; matching is
    // by Alertname against the Prometheus ALERTS series). All three fields
    // are required at v1 to lock the schema shape.
    RuleRef AlertRuleRef `json:"ruleRef"`
    // Timeout for the Prometheus instant query (default 30s, max 5m).
    // +optional
    // +kubebuilder:default="30s"
    Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.alertname.size() > 0",message="alertname is required"
type AlertRuleRef struct {
    // Name of the PrometheusRule resource.
    // +kubebuilder:validation:MinLength=1
    Name string `json:"name"`
    // Namespace of the PrometheusRule. Same-namespace as the Run, OR a
    // namespace labeled sei.io/validation-shared-rules=true (e.g., monitoring).
    // +kubebuilder:validation:MinLength=1
    Namespace string `json:"namespace"`
    // Alertname is the alert label that the rule fires under.
    // +kubebuilder:validation:MinLength=1
    Alertname string `json:"alertname"`
}

type QueryRule struct {
    // PromQL is the query string. Window encoded inline (e.g. rate(...[5m])).
    // +kubebuilder:validation:MinLength=1
    PromQL string `json:"promql"`
    // Op is the comparator for verdict computation.
    // +kubebuilder:validation:Enum=">=";"<=";">";"<";"==";"!="
    Op QueryComparator `json:"op"`
    // Threshold is a stringified number (locked as string per OTel-expert
    // Round 1 finding — switching numeric→string later would be a breaking
    // CRD migration; the reverse is additive). Coerced to float64 at eval.
    // +kubebuilder:validation:Pattern=`^-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$`
    Threshold string `json:"threshold"`
    // Timeout for the Prometheus instant query.
    // +optional
    // +kubebuilder:default="30s"
    Timeout *metav1.Duration `json:"timeout,omitempty"`
}

type RuleRunProperties struct {
    // Interval is the polling cadence between evaluations of this rule
    // during the run window. Default 30s. Min 5s, max 5m.
    //
    // The rule is evaluated at apply-job + Interval, then every Interval
    // until the workload Job reaches a terminal state (or stop-on-failure
    // cancels the run). A final sweep evaluates any rule whose interval
    // is still pending at workload terminal time before the verdict is
    // computed.
    // +optional
    // +kubebuilder:default="30s"
    // +kubebuilder:validation:Minimum=5
    // +kubebuilder:validation:Maximum=300
    Interval *metav1.Duration `json:"interval,omitempty"`

    // StopOnFailure: if true, a Failed verdict on this rule short-circuits
    // the run — the controller deletes the workload Job (cooperative cancel)
    // and transitions to Phase=Failed. Default false (let the workload finish
    // and aggregate all rule verdicts at the end).
    // +optional
    // +kubebuilder:default=false
    StopOnFailure bool `json:"stopOnFailure,omitempty"`

    // Retry is the number of consecutive Errors tolerated before the
    // rule is recorded as Error. Defaults to 3. Does NOT retry on Failed —
    // verdicts of Failed are intentionally monotonic.
    // +optional
    // +kubebuilder:default=3
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:validation:Maximum=10
    Retry int32 `json:"retry,omitempty"`
}
```

There is no `mode` field on `ValidationRule` in v1: there is exactly one evaluation cadence (continuous polling, with a final sweep at workload terminal). If a future use case demands `edge` (start-vs-end snapshot) or a single `window-end` instant, the field is additive — add `mode` with a default of `continuous`, store new modes only on objects that opt in.

#### `ResultsSpec` and `RunTimeouts`

```go
type ResultsSpec struct {
    // S3 is informational only — the bucket layout is controller-baked
    // (s3://harbor-validation-results/{namespace}/{job}/{runId}/) and locked.
    // This struct exists to surface the resolved URL on status. v1 has no
    // user-tunable fields; reserved for future bucket overrides.
    // +optional
    S3 *S3ResultsSpec `json:"s3,omitempty"`
}

type S3ResultsSpec struct {
    // Reserved. v1 ignores user-supplied bucket/prefix.
}

type RunTimeouts struct {
    // ChainReady caps wait-chain-ready. Default 20m.
    // +optional
    // +kubebuilder:default="20m"
    ChainReady *metav1.Duration `json:"chainReady,omitempty"`

    // RunDuration caps the workload Job's wall-clock from apply to terminal.
    // When omitted, defaults to spec.loadTest.duration + 5m grace.
    // +optional
    RunDuration *metav1.Duration `json:"runDuration,omitempty"`
}
```

#### `ValidationRunStatus`

```go
type ValidationRunStatus struct {
    // ObservedGeneration tracks the spec generation processed by the latest
    // reconcile. Required for Tekton-style Succeeded condition semantics.
    // +optional
    ObservedGeneration int64 `json:"observedGeneration,omitempty"`

    // Phase is the high-level lifecycle state. Capitalized — never lowercase.
    // +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Error;Cancelled
    // +optional
    Phase ValidationRunPhase `json:"phase,omitempty"`

    // Verdict is a denormalized human-friendly summary that mirrors a
    // condition. Passed/Failed/Awaited/Error. Computed at terminal phase
    // entry; remains stable thereafter.
    // +optional
    // +kubebuilder:validation:Enum=Passed;Failed;Awaited;Error
    Verdict ValidationVerdict `json:"verdict,omitempty"`

    // StartTime is the wall-clock time the run entered Running.
    // +optional
    StartTime *metav1.Time `json:"startTime,omitempty"`

    // CompletionTime is when the run entered a terminal phase.
    // +optional
    CompletionTime *metav1.Time `json:"completionTime,omitempty"`

    // Duration is the formatted CompletionTime - StartTime, surfaced
    // for printcolumn convenience.
    // +optional
    Duration string `json:"duration,omitempty"`

    // Plan tracks the active reconciliation plan (same shape as SeiNode/SND).
    // Nil when no plan is in progress (terminal phases or pre-plan-build).
    // +optional
    Plan *sei_v1alpha1.TaskPlan `json:"plan,omitempty"`

    // Chain reports the materialized SND children and resolved endpoints.
    // +optional
    Chain *ChainStatus `json:"chain,omitempty"`

    // Job names the workload Job materialized for this run.
    // +optional
    Job *JobStatus `json:"job,omitempty"`

    // WorkloadExitCode is the exit code captured from the workload Job's
    // pod once the Job reaches terminal state. Distinct from Job.Failed
    // (which counts pod failures) — this is the actual exit-code signal.
    // 0 = Succeeded path, 1 = Failed path, 2 = Error path. Unset until
    // Conditions[TestComplete]=True.
    // +optional
    WorkloadExitCode *int32 `json:"workloadExitCode,omitempty"`

    // Rules carries per-rule verdicts and supporting evidence. Updated
    // continuously during the run by the monitor-run task.
    // +listType=map
    // +listMapKey=name
    // +optional
    Rules []RuleStatus `json:"rules,omitempty"`

    // Report carries the resolved S3 URL. The full report (workload
    // termination message, mochawesome HTML, JUnit XML, rules JSON)
    // lives in S3 — `.status` carries only the URL, never the bytes.
    // +optional
    Report *ReportStatus `json:"report,omitempty"`

    // Conditions includes the Tekton-style Succeeded condition and the
    // monotonic TestComplete condition. See "Conditions" subsection.
    // +listType=map
    // +listMapKey=type
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
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
    ValidatorsSND string   `json:"validatorsSND"`
    FullNodesSND  string   `json:"fullNodesSND"`
    RPCEndpoints  []string `json:"rpcEndpoints,omitempty"` // resolved at resolve-endpoints
}

type JobStatus struct {
    Name        string       `json:"name"`
    UID         string       `json:"uid,omitempty"`
    StartTime   *metav1.Time `json:"startTime,omitempty"`
    Completions int32        `json:"completions,omitempty"`
    Active      int32        `json:"active,omitempty"`
    Failed      int32        `json:"failed,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.alert)?1:0) + (has(self.query)?1:0) <= 1",message="rule status carries at most one of alert or query"
type RuleStatus struct {
    Name    string             `json:"name"`
    Type    ValidationRuleType `json:"type"`
    Verdict ValidationVerdict  `json:"verdict"`
    // Reason categorizes Error verdicts:
    //   PrometheusRuleNotFound, NoSamples, AmbiguousResult, NaN,
    //   PrometheusUnavailable, RBACDenied, Timeout
    // and Failed verdicts:
    //   ThresholdViolated (query), AlertFired (alert)
    // +optional
    Reason string `json:"reason,omitempty"`
    // LastEvaluatedAt is the wall-clock time the most recent evaluation
    // completed (irrespective of verdict). Required for idempotent polling
    // across controller restarts — the monitor-run task uses this to compute
    // the next evaluation deadline.
    // +optional
    LastEvaluatedAt *metav1.Time `json:"lastEvaluatedAt,omitempty"`
    // NextEvaluationAt is LastEvaluatedAt + RunProperties.Interval, or the
    // run start + Interval before the first evaluation. The monitor-run task
    // requeues at min(this) across all rules.
    // +optional
    NextEvaluationAt *metav1.Time `json:"nextEvaluationAt,omitempty"`
    // Query carries the actualValue and threshold for query rules. Both
    // strings — see OTel Round 1 schema-typing rationale. Reflects the
    // most recent evaluation.
    // +optional
    Query *QueryRuleStatus `json:"query,omitempty"`
    // Alert carries the matched alert details for alert rules. Reflects
    // the most recent evaluation.
    // +optional
    Alert *AlertRuleStatus `json:"alert,omitempty"`
}

type QueryRuleStatus struct {
    ActualValue string `json:"actualValue"` // string, lossless
    Threshold   string `json:"threshold"`
    Op          string `json:"op"`
}

type AlertRuleStatus struct {
    FiredCount  int32        `json:"firedCount"`            // count of ALERTS samples > 0 in window
    LastFiredAt *metav1.Time `json:"lastFiredAt,omitempty"`
}

type ReportStatus struct {
    // S3URL is s3://harbor-validation-results/{namespace}/{job}/{runId}/
    // (controller-stamped at terminal phase entry). The URL is programmatically
    // derivable from the bucket layout convention; consumers fetch artifacts
    // (termination-message.json, report.html, report.junit.xml, rules/*.json)
    // directly from S3. The controller does not echo the workload's
    // termination-message bytes into status.
    // +optional
    S3URL string `json:"s3Url,omitempty"`
}
```

##### Conditions

Two condition types live on `.status.conditions`:

**`Succeeded`** — the Tekton-style "is this run done and how" signal:

| status | reason | meaning |
|---|---|---|
| `Unknown` | `Pending` / `Running` | Not yet terminal |
| `True` | `RunSucceeded` | Phase=Succeeded |
| `False` | `WorkloadFailed` | Workload exit 1 |
| `False` | `RuleFailed:{ruleName}` | One or more rules verdict=Failed |
| `False` | `RuleError` | Run terminated with Phase=Error from rule eval |
| `False` | `WorkloadInfraFailure` | Workload exit 2 |
| `False` | `ChainNotReady` | wait-chain-ready timed out |
| `False` | `Cancelled` | Phase=Cancelled |

**`TestComplete`** — monotonic "the workload Job has reached terminal state and we have its exit code." Set to `True` exactly once during a Run's lifetime; never reverts. Distinct from `Succeeded` because `TestComplete=True` does not imply pass/fail — it's the Job-side advance signal that lets `monitor-run` decide whether to stop polling rules.

| status | reason | meaning |
|---|---|---|
| `Unknown` | `Pending` / `Running` | Workload not yet terminal |
| `True` | `JobTerminated` | Job reached Complete or Failed; `status.workloadExitCode` populated |
| `True` | `JobTimedOut` | `spec.timeouts.runDuration` exceeded; controller cancelled the Job; exit code synthesized as 2 |

`Succeeded` matches Tekton TaskRun/PipelineRun semantics ([Tekton TaskRun docs](https://github.com/tektoncd/pipeline/blob/main/docs/taskruns.md)) so existing tooling that watches Succeeded works unchanged. `kubectl wait --for=condition=Succeeded validationrun/X` is the contract.

`TestComplete` exists because `monitor-run` collapses Job-watching and rule-polling into one async loop and needs a durable "Job-side is done" flag that survives controller restarts. It also gives external consumers (e.g., a UI) a way to render "workload finished, rules still aggregating" without reading the plan structure.

### Future discriminator kinds (one paragraph each, NOT detailed)

These are reserved on the discriminator union so v1's shape doesn't paint future expansion into a corner. The corresponding spec sub-structs are present in the type but **empty placeholder structs** at v1 — controller rejects `type=SequenceTest` and `type=IntegrationTest` at admission until their reconcilers ship.

#### `SequenceTest` (future)

A sequence of state changes applied to an ephemeral chain. The body holds an ordered list of "steps," each a typed action: apply a SeiNodeDeployment template patch (rolling validator update), submit a tx, register a validator, wait on a height, assert a query/alert. The driver is the controller; the steps share the same `chain` substrate as `LoadTest`. Fits cleanly because the chain owner is `ValidationRun`, not the kind body — the kind body just describes "what to do against the chain." Use case: validate Cosmos-SDK upgrade handlers (apply, halt height, switch image, resume), validator-set churn, IBC connection bring-up.

#### `IntegrationTest` (future)

Provision a chain (optionally bootstrap state from another chain via `chain.validators.genesis.fork` — already a real SND feature), then run a single container that performs end-to-end operations. Verdict is the container's exit code; rules are still optional and AND'd. Pair-of-pods cases (e.g., spin up two relayer containers and exercise IBC between two ephemeral chains) are *not* in this kind — that's `ValidationSuite`'s job. The kind exists as the cleanest expression of "stand up a chain, run one container, that container is the test."

### `ValidationSuite` — designed but NOT implemented in v1

Designed at field-level so `ValidationRun`'s shape doesn't lock out the suite parent. CRD ships with v1; no controller implements it. Attempting to create one returns an admission-time error from the controller webhook (`ValidationSuite reconciler not registered in this controller version`).

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
    // Runs is an ordered list of inline ValidationRun specs. Each child
    // run is materialized as a ValidationRun resource owned by the suite
    // (OwnerReferences, blockOwnerDeletion).
    // +listType=map
    // +listMapKey=name
    // +kubebuilder:validation:MinItems=1
    Runs []SuiteRunSpec `json:"runs"`

    // Concurrency controls how children are launched.
    // +optional
    // +kubebuilder:default=Sequential
    // +kubebuilder:validation:Enum=Sequential;Parallel
    Concurrency SuiteConcurrency `json:"concurrency,omitempty"`

    // StopOnFailure (Sequential only) halts the suite at the first child
    // that terminates Failed or Error. Default true (fail fast for CI).
    // +optional
    // +kubebuilder:default=true
    StopOnFailure bool `json:"stopOnFailure,omitempty"`
}

type SuiteRunSpec struct {
    // Name is the suite-local identity used to compose the child Run name.
    // +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
    Name string `json:"name"`
    // Spec is the inline ValidationRun spec for this child.
    Spec ValidationRunSpec `json:"spec"`
}
```

No DAG. Users wanting a DAG wrap suites in Argo (Litmus's "we outsource multi-experiment to Argo" lesson — [Litmus probes](https://docs.litmuschaos.io/docs/concepts/probes)).

### `ValidationSchedule` — designed but NOT implemented in v1

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
    // Schedules are one or more cron expressions in the IANA timezone.
    // Multi-cron permits "weekday morning" + "weekend afternoon" without
    // composing a single intersection.
    // +kubebuilder:validation:MinItems=1
    Schedules []string `json:"schedules"`

    // Timezone is an IANA tz name (e.g., "America/New_York").
    // +optional
    // +kubebuilder:default=UTC
    Timezone string `json:"timezone,omitempty"`

    // RunTemplate is the inline ValidationRun spec stamped per firing.
    RunTemplate ValidationRunSpec `json:"runTemplate"`

    // ConcurrencyPolicy: Allow | Replace | Forbid.
    // +optional
    // +kubebuilder:default=Forbid
    // +kubebuilder:validation:Enum=Allow;Replace;Forbid
    ConcurrencyPolicy SchedConcurrency `json:"concurrencyPolicy,omitempty"`

    // SuccessfulRunsHistoryLimit / FailedRunsHistoryLimit cap retained
    // child runs (default 3 / 1).
    // +optional
    SuccessfulRunsHistoryLimit *int32 `json:"successfulRunsHistoryLimit,omitempty"`
    // +optional
    FailedRunsHistoryLimit *int32 `json:"failedRunsHistoryLimit,omitempty"`

    // StartingDeadlineSeconds — if a firing is missed by more than this,
    // skip rather than backfill. Default 60.
    // +optional
    // +kubebuilder:default=60
    StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`

    // Pause via annotation (validation.sei.io/paused: "true") rather than
    // a spec field, so GitOps doesn't fight on/off toggles.
    // (No spec.suspend; field deliberately absent — Chaos Mesh pattern.)
}
```

## Lifecycle and plan tasks

The reconciler integrates with the existing `internal/planner/` plan-driven architecture (see `/Users/brandon/sei-k8s-controller/internal/planner/doc.go`). A new builder lives at `internal/planner/validationrun.go` and produces the plan; a new resolver `ValidationRunResolver` lives in the controller package and follows the same `ResolvePlan → persist → ExecutePlan` shape as `NodeResolver` and `ForGroup`.

**Plan-driven invariants inherited from the existing controllers:**

- Plan persisted before any task executes (atomic creation).
- Single-patch model: tasks mutate owned resources; executor mutates plan/phase in-memory; reconciler flushes once.
- Planner owns conditions; executor never sets conditions.
- Terminal plan observed by next reconcile, which sets the terminal phase.

### Plan structure

There is exactly one plan kind for `ValidationRun` v1: the **run plan**, built when `phase ∈ {Pending, ""}` and torn down when terminal. No update plan (the spec is immutable on a Run; mutations are rejected at admission via CEL `self == oldSelf` on the substantive fields).

```
Phase: Pending
  └─▶ planner builds plan, persists, sets phase=Running on persist

Plan tasks (ordered, sequential — 7 tasks):
  1. ensure-chain         (controller-side)
  2. wait-chain-ready     (controller-side, async)
  3. resolve-endpoints    (controller-side)
  4. render-config        (controller-side)
  5. apply-job            (controller-side)
  6. monitor-run          (controller-side, async; combined Job-watch + rule polling)
  7. mark-done            (controller-side, terminal-phase setter; stamps S3 URL)

Plan TargetPhase: Succeeded   (set at executor's plan-completion step)
Plan FailedPhase:  Failed      (set when a task fails terminally)
Plan ErrorPhase:   Error       (carried via FailedTaskDetail.Reason dispatch
                                in the planner; see Open Dependencies #2)
```

The collapse from the prior 10-task draft to 7 is deliberate. The previous `wait-job-terminal → resolve-rules → evaluate-rules → collect-report` quartet split state across four task records that actually share state — each held a piece of "is the workload done and what did the rules say" that the next task had to recover from `.status`. Folding them into `monitor-run` (with `mark-done` absorbing the trivial S3 URL stamp that `collect-report` previously owned) reduces controller-side state-shuttling. The `monitor-run` task itself is described under task #6 below.

The `Failed` vs `Error` distinction is load-bearing for the heartbeat alert (which ignores Error). v1 carries the distinction via the planner's terminal-plan observation by inspecting `FailedTaskDetail.Reason` and choosing `Failed` vs `Error` (no shared TaskPlan type change).

### Per-task contract

Each task lives at `internal/task/validation/{name}.go`, follows the existing controller-side task pattern (`Submit / Poll / Apply` interface — see `internal/task/observe_image.go` for a worked example), and is registered in `internal/task/task.go`'s deserialize map.

#### 1. `ensure-chain`

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeEnsureChain` |
| Sync/Async | Sync |
| Idempotent op | Server-side apply two SNDs (validators and fullNodes — both always materialized), with `OwnerReferences` to the Run. Field manager `validationrun-controller`. |
| Inputs | `Run.spec.chain` |
| Success | Both SNDs exist with the expected spec hash |
| Failure | API error (terminal after 3 retries → `Reason=ChainApplyFailed`); SND validation reject (terminal → `Reason=ChainSpecInvalid`) |

Injected fields the task fills before SSA:

- `genesis.chainId = chain.chainId` on the validator SND if unset; rejected at admission if set and mismatched.
- `template.spec.chainId = chain.chainId` on every template if unset.
- `template.spec.validator: {}` on the validator SND template; `template.spec.fullNode: {}` on the fullNodes template.
- Default `peers: [{label: {selector: {sei.io/chain-id: chainId}}}]` for validators and `[{label: {selector: {sei.io/nodedeployment: chainId}}}]` for fullNodes — when the user omitted `peers`.
- Run-id label on every materialized object (`sei.io/run-id`, `sei.io/managed-by=validationrun`).

#### 2. `wait-chain-ready`

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeWaitChainReady` |
| Sync/Async | Async (RequeueAfter pattern) |
| Idempotent op | GET both SNDs; check `status.phase == Ready` AND `status.conditions[NodesReady].status == True`. |
| Timeout | `spec.timeouts.chainReady` (default 20m) |
| Failure | Timeout → `Reason=ChainNotReady`. SND `phase=Failed` → `Reason=ChainFailed` |

**Open dependency on SND-readiness semantics.** The existing `SeiNodeDeployment.Status.Phase=Ready` (`/Users/brandon/sei-k8s-controller/api/v1alpha1/seinodedeployment_types.go:188`) is computed from child node `phase=Running` plus `ConditionNodesReady`. It does **not** today include a "all full nodes report `catching_up=false`" precondition — the `AwaitNodesCaughtUp` task exists (`/Users/brandon/sei-k8s-controller/internal/task/deployment.go:46-51`) and is invoked during hard-fork rollouts but not during initial bring-up.

For genesis-ceremony chains (the ValidationRun common case) the validator fleet is producing blocks from height 0 and `catching_up=false` is essentially immediate after `phase=Ready`. For full nodes that block-sync, there is a real gap. Three options surfaced for the SND maintainer (file as companion sub-issue, not a v1 ValidationRun blocker):

- **(a)** Extend SND status to include `CaughtUp` as a Ready precondition. Cleanest for ValidationRun. Minor schema add.
- **(b)** Move catching_up into the SeiNode pod's readiness probe (`/health` endpoint that returns 503 while `catching_up=true`). Pod isn't Ready until caught up; SND `phase=Ready` automatically requires it. Cleanest for *all* SND consumers. Bigger change to the SeiNode probe contract.
- **(c)** ValidationRun's `wait-chain-ready` task additionally GETs each child SeiNode and inspects a status field exposing catching_up. Requires SeiNode status to expose the field (it doesn't today), and pushes the responsibility into the wrong controller.

**Recommendation:** option (b). Files cleanly into the existing SeiNode reconciler responsibility surface; benefits hard-fork rollouts and steady-state operations equally; ValidationRun's wait task reduces to just the SND phase poll. v1 ValidationRun can ship without the fix and accept that genesis-bootstrapping nightly is the dominant case (no real catch-up gap); the qa-testing workload also tolerates a few stale-mempool seconds.

#### 3. `resolve-endpoints`

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeResolveEndpoints` |
| Sync/Async | Sync |
| Idempotent op | LIST headless Services with selector `sei.io/nodedeployment={chainId}-rpc`; build `{podDNSName}:{port}` list. Stamps `Run.status.chain.rpcEndpoints`. |
| Selection | Always the fullNodes fleet (always materialized in v1). |
| Failure | Empty result after 5 retries → `Reason=EndpointsNotResolvable` |

This is the controller-side replacement for `kubectl get services -l sei.io/nodedeployment=...` in `k8s_nightly.yml:216`.

#### 4. `render-config`

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeRenderConfig` |
| Sync/Async | Sync |
| Idempotent op | Apply ConfigMap with files from `loadTest.workload.config.files`, fixed-substitutions applied (`${chainId}`, `${rpcEndpoints}`, `${runId}`, `${namespace}`). OwnerRef → Run. Name `{runName}-config`. |
| Skipped when | `loadTest.workload.config` unset |
| Failure | API error → terminal `Reason=ConfigApplyFailed` |

#### 5. `apply-job`

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeApplyJob` |
| Sync/Async | Sync |
| Idempotent op | SSA `batch/v1.Job` named `{runName}` with `spec.parallelism=replicas`, `spec.completions=replicas`, `spec.completionMode=Indexed`, `backoffLimit=0`. Pod template carries reserved env vars, mounted ConfigMap, RESULT_DIR emptyDir, `serviceAccountName=spec.workload.serviceAccountName \|\| {ns}-runner`. OwnerRef → Run. |
| Failure | Forbidden (no SA) → terminal `Reason=ServiceAccountMissing`. Other API errors retried. |

The controller does **not** carry Flux labels (`kustomize.toolkit.fluxcd.io/*`, `app.kubernetes.io/managed-by=flux`) on the Job, ConfigMap, or any owned object. Hard invariant — see One-Way Doors.

#### 6. `monitor-run`

This is the central task. It combines workload Job watching and per-rule continuous polling into a single async loop. State lives entirely in `.status` (`Conditions[TestComplete]`, `status.workloadExitCode`, per-rule `LastEvaluatedAt` / `NextEvaluationAt`) so the task is fully idempotent across controller restarts.

| Property | Value |
|---|---|
| Type constant | `validation.TaskTypeMonitorRun` |
| Sync/Async | Async (RequeueAfter; pod-watch refines requeue cadence) |
| Inputs | `Run.spec.rules`, `Run.status.job`, `Run.status.rules`, `Run.status.conditions` |
| Timeout | `spec.timeouts.runDuration` (default `loadTest.duration + 5m`) |

**Per-iteration behavior:**

```
1. Job-side advance:
     if Conditions[TestComplete] is not True:
       GET workload Job (name = status.job.name)
       if Job has reached terminal state (condition Complete=True OR Failed=True):
         resolve workload-pod exit code via core/pods GET
         status.workloadExitCode = exitCode
         set Conditions[TestComplete] = True, Reason = JobTerminated
       else if now > runStart + spec.timeouts.runDuration:
         delete Job (cooperative cancel)
         status.workloadExitCode = 2
         set Conditions[TestComplete] = True, Reason = JobTimedOut

2. Rule-side advance (for each rule with verdict != Failed):
     if rule.LastEvaluatedAt + Interval <= now:
       (or: rule.LastEvaluatedAt is unset and now >= runStart + Interval)
       issue Prometheus instant query at time=now
       classify into Passed | Failed | Error (per evaluate semantics below)
       update status.rules[i] with verdict, reason, query/alert evidence,
         LastEvaluatedAt = now,
         NextEvaluationAt = now + Interval

3. Stop-on-failure:
     if any rule with RunProperties.StopOnFailure=true tripped to Failed
       in this iteration:
       delete workload Job (cooperative cancel — Job pods get SIGTERM)
       exit task as Failed, planner transitions Phase=Failed at next reconcile

4. Exit conditions:
     if Conditions[TestComplete] = True AND no rule has verdict=Failed:
       do a final sweep — for any rule whose NextEvaluationAt <= now+Interval
       (i.e., still pending) issue one last evaluation now
       exit task Complete → planner advances to mark-done

     if Conditions[TestComplete] = True AND some rule has verdict=Failed:
       exit task Failed → planner transitions Phase=Failed (or Phase=Error
       if the workload exit code was 2 and no rule Failed; mark-done resolves
       the Failed-vs-Error distinction)

5. Else (still running):
     RequeueAfter min(
       earliest rule's NextEvaluationAt - now,
       jobPollInterval (10s — used to bound Job-side observation latency)
     )
```

**Idempotency.** The task carries no in-memory state across reconciles. Every iteration recomputes from `.status`. Conditions[TestComplete] is monotonic — once set True, the Job-side advance step short-circuits without re-fetching Job. Per-rule LastEvaluatedAt provides the polling cursor that survives controller restarts and leader-election handover.

**Prometheus client** lives at `internal/monitoring/prom.go` (new package) — one connection pool, one `otelhttp.NewTransport`, one set of metrics. URL configured by env (`VALIDATION_PROMETHEUS_URL`, default `http://prometheus-k8s.monitoring.svc:9090`) with `--prometheus-url` flag override.

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

##### Failure model

Per-rule:

- `Failed` = "signal said SUT misbehaved" (alert fired in window, query violated threshold)
- `Error` = "couldn't ask" (Prometheus 5xx, network timeout, RBAC denial, unresolved ref, NaN, NoSamples, AmbiguousResult)

`Failed` is monotonic — once a rule trips, it stays Failed. `Error` decays — a successful subsequent evaluation overwrites Error with Passed/Failed (the Retry budget governs how many consecutive Errors mature into a permanent rule-level Error verdict).

##### PrometheusRule resolution (folded into monitor-run's first iteration)

For each rule with `type=alert`, the first evaluation iteration GETs the referenced `monitoring.coreos.com/v1.PrometheusRule`; verifies `alertname` exists in the rule's spec; validates cross-namespace policy (same-namespace OR target ns labeled `sei.io/validation-shared-rules=true`). Per-rule failure of resolution writes `verdict=Error` with `Reason=PrometheusRuleNotFound` or `Reason=CrossNamespaceForbidden`; does NOT block other rules. If **all** rules fail resolution on first iteration, the task exits Error with `Reason=AllRulesUnresolved`.

#### 7. `mark-done`

Computes the final phase + verdict + condition from accumulated state, and stamps the S3 URL. Pure in-memory aggregation plus one status patch:

```
exit := *status.workloadExitCode  (set by monitor-run)
ruleVerdicts := status.rules[]

case exit == 2:                                  → Phase=Error, Reason=WorkloadInfraFailure
case exit == 1 && any(rule.Failed):              → Phase=Failed, Reason=WorkloadFailed (primary), append RuleFailed conditions
case exit == 1:                                  → Phase=Failed, Reason=WorkloadFailed
case any(rule.Failed):                           → Phase=Failed, Reason=RuleFailed:{firstFailedRule}
case all(rule.Passed):                           → Phase=Succeeded, Reason=RunSucceeded
case any(rule.Error) && all others Passed:       → Phase=Error,  Reason=RuleError
default:                                         → Phase=Error,  Reason=Unknown
```

Sets `status.completionTime`, `status.duration`, the `Succeeded` condition, computes `status.report.s3Url = s3://harbor-validation-results/{ns}/{job}/{runId}/` from the bucket layout convention, and emits the `validation_run_terminal_total{verdict}` metric (heartbeat-alert input).

The S3 URL stamp is trivially derivable; the controller never reads from S3.

### Cancellation

`Cancelled` is reached via `metadata.deletionTimestamp != nil`. The reconciler stops issuing new tasks, lets the Job's grace period elapse, lets cascade-delete OwnerRefs purge SNDs/Job/ConfigMap, and clears its finalizer. No explicit `spec.cancel: true` field — `kubectl delete` is the contract.

Stop-on-failure (a rule trip with `RunProperties.StopOnFailure=true`) is *not* `Cancelled`; it is `Failed`. The controller deletes the workload Job to halt load generation, but the Run itself reaches a terminal verdict, not a cancellation.

## Validation rule semantics

(Concise restatement; see `monitor-run` task above for the operative detail and OTel Round 1 for the rationale.)

- v1 evaluates rules continuously: each rule polls at `RunProperties.Interval` (default 30s, min 5s, max 5m) starting at `runStart + Interval`, with a final sweep at workload terminal time before mark-done aggregates.
- v1 types: `alert`, `query`. Future `container` reserved.
- Both types use the same Prometheus HTTP client (`internal/monitoring/prom.go` — new package). One connection pool, one `otelhttp.NewTransport` instrumentation, one set of metrics.
- Prometheus URL is configured by env var (`VALIDATION_PROMETHEUS_URL`, default `http://prometheus-k8s.monitoring.svc:9090`) with `--prometheus-url` flag override. Hardcode-with-flag-override per OTel Q2.
- AND-of-rules pass semantics. OR/weighted aggregation explicitly out of scope; users compose OR with two ValidationRuns in a Suite.
- Per-rule `verdict ∈ {Passed, Failed, Awaited, Error}` (4-state). `Awaited` is the pre-first-evaluation state visible in `.status` until each rule has at least one Prometheus result.
- `Failed` is monotonic per rule (a rule that trips stays tripped — even if a later evaluation would have passed). `Error` is recoverable (decays to Passed/Failed on next successful evaluation; only matures into a permanent rule-level Error after `Retry` consecutive Errors).
- `StopOnFailure: true` deletes the workload Job when its rule trips; the Run terminates Failed.

## Observability

Per OTel Round 1, with cardinality discipline.

### Controller metrics (emitted from the reconciler binary)

| Instrument | Type | Labels | Cardinality |
|---|---|---|---|
| `validationrun_phase_transitions_total` | Counter | `from`, `to`, `reason` | ~200 series |
| `validationrun_active` | UpDownCounter | `phase` | 5 series |
| `validationrun_duration_seconds` | Histogram (`30,60,300,600,1800,3600,7200`) | `terminal_phase`, `workload_kind` | bounded |
| `validation_rule_evaluation_duration_seconds` | Histogram (`0.05,0.1,0.5,1,5,10,30`) | `type`, `verdict` | 12 series |
| `validation_rule_evaluations_total` | Counter | `type`, `verdict`, `reason` | bounded |
| `validation_prometheus_query_errors_total` | Counter | `endpoint`, `error_type` | ≤8 series |
| `validation_run_terminal_total` | Counter | `namespace`, `name`, `verdict` | **per-run** — see note below |

**Note on `validation_run_terminal_total` cardinality.** `name` is unbounded and would normally be a hard reject. Exception: this is the heartbeat-alert input (per platform-engineer), which queries `increase(... [24h])`. The cardinality is bounded by the alerting path's retention (the metric is short-lived per run). If/when Prometheus pressure shows, drop `name` and switch heartbeat to a `validation_run_terminal_total{namespace, verdict}` aggregate. Document the upgrade path; ship the loud version first.

**Note on `mode` label.** Round 1's OTel sketch carried a `mode` label on rule-evaluation metrics. v1 has no `mode` field on the wire (continuous is the only behavior), so the label is dropped. If a future mode lands, re-add it with care for cardinality.

**Labels rejected (cardinality bombs):** `chain_id`, `image_sha`, `run_id`. These go into trace attributes and structured log fields, not metrics.

### Traces

One span per reconcile (`controller.kind=ValidationRun` + standard controller-runtime attributes). Child span per planner task — these are 1+ second I/O-bound ops where p99 matters. `monitor-run`'s per-iteration loop emits one span per Prometheus query. Use `otelhttp.NewTransport` on the Prometheus client (single-flight client lives in `internal/monitoring/`). Inject `traceID`/`spanID` into structured logs.

### Structured-report aggregation

**S3-only in v1.** No Pushgateway, no remote_write of report metrics, no `.status.report.raw`. Reasons (per OTel Round 1):

- Pushgateway is the wrong tool for batch-job metrics (Prometheus docs explicitly warn).
- The report shape isn't yet stable across `seiload` and `qa-testing`; locking before two real consumers is a trap.
- Cardinality of `{run_id, image_sha, profile}` is the bomb the controller avoids.
- `.status.report.raw` (a CEL-capped echo of the termination message) was cut at the Round 2 gate. Consumers that want the report fetch from `status.report.s3Url`; the URL is programmatically derivable from the bucket layout (`s3://harbor-validation-results/{namespace}/{job}/{runId}/`) so even consumers that don't yet wire-watch can compute it.

**Un-defer when:** ≥2 distinct report consumers want the same numeric AND a stable report-schema field has shipped. Add a separate `ReportExporter` controller that subscribes to terminal Runs and emits curated low-cardinality metrics. *Not* the Run controller's job.

## RBAC and tenancy

### Controller-side ClusterRole (generated from kubebuilder markers)

```go
// +kubebuilder:rbac:groups=validation.sei.io,resources=validationruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=validation.sei.io,resources=validationruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=validation.sei.io,resources=validationruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=validation.sei.io,resources=validationsuites;validationschedules,verbs=get;list;watch
// +kubebuilder:rbac:groups=validation.sei.io,resources=validationsuites/status;validationschedules/status,verbs=get
// +kubebuilder:rbac:groups=sei.io,resources=seinodedeployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sei.io,resources=seinodedeployments/status,verbs=get
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
```

The controller deletes Jobs (cooperative cancel for stop-on-failure and run-duration timeout); `delete` verb on `batch/jobs` is load-bearing for that. SNDs are deleted only via cascade on Run delete — controller-initiated SND delete is reserved for finalizer cleanup and not exercised in steady state.

Not granted: `secrets` write, `rbac.*`, `eks.*` (pod-identity AWS calls). Per platform-engineer: controller never mints SAs or PIAs.

### Tenant-side ClusterRole bundle (shipped in controller's manifests; bound per-namespace by tenants)

- `validation-tenant-author`: full CRUD on `validation.sei.io/*` + `seinodedeployments` + view on rule-eval status.
- `validation-tenant-viewer`: get/list/watch on validation kinds only.

### CEL admission rules (CRD-level)

- Same-namespace enforcement on every reference except `alert.ruleRef.namespace` (which is allowed cross-namespace into label-allowlisted namespaces only — checked at admission via webhook because CEL can't see namespace labels).
- `chain.fullNodes.genesis` forbidden via top-level CEL on `ChainSpec`.
- `chain.fullNodes` is required (not pointer; required field on `ChainSpec`).
- `chain.validators.template.spec.validator` if set must be `{}` (webhook).
- Reserved env-var names rejected on `spec.loadTest.workload.env` via the `WorkloadSpec` `XValidation` rule.
- Rule names unique within `spec.rules`.
- Spec mutation: substantive fields are immutable on a Run after creation (CEL: `self == oldSelf` on `spec.chain`, `spec.loadTest`, `spec.rules`). Only metadata, finalizers, and status mutate.

### Workload SA convention

Tenant pre-provisions SA named `{namespace}-runner` (override via `spec.loadTest.workload.serviceAccountName`). Controller validates existence at the Pending → Running transition; missing SA terminates with `Reason=ServiceAccountMissing`. Controller never creates SAs.

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
  `{job}` is `spec.loadTest.workload.name`. New components only by *appending* — no new path levels.
- Workloads upload via Pod Identity (already wired for nightly). Controller never writes to S3, never reads from S3, only computes the URL.

## Migration: nightly GHA → ValidationRun

Per platform-engineer Round 1:

- `clusters/harbor/nightly/` Flux config is **structurally unchanged**. Same namespace, SAs, RBAC, PodMonitors. Pod labels (set by controller from `spec.loadTest.workload.podTemplate` plus chain-id + run-id) keep selector compatibility.
- `.github/workflows/k8s_nightly.yml` shrinks ~50%: each `kubectl wait`/`kubectl exec`/`envsubst`/`kubectl logs`/`aws s3 cp` step becomes a controller plan task. The workflow becomes:
  1. `cat <<EOF | envsubst | kubectl apply -f -` (one ValidationRun manifest)
  2. `kubectl wait --for=condition=Succeeded validationrun/${RUN_ID} --timeout=2h`
  3. Read `.status.report.s3Url` and `.status.verdict` for the GHA summary; pull the report artifacts from S3 directly (the URL is the S3 prefix; the workload uploaded `report.log`, `report.html`, `report.junit.xml`, `termination-message.json`, and `rules/{name}.json` under it).
- The race against `sei-chain` CI moves into the controller's `ensure-chain` task (digest-pinned image already present at apply time → no race; if not, the SND pulls and reports its own readiness).
- `templates/seinodedeployment.yaml` and `templates/seiload-job.yaml` retire (subsumed into `ValidationRun.spec`). Both fleets (validators and fullNodes) are now controller-materialized; the existing nightly's two-SND topology lifts into `chain.validators` + `chain.fullNodes` directly.

The migration is opt-in per tenant: nightly can flip to ValidationRun while qa-testing still runs Phase 1's GHA-orchestrated bash, and vice versa. The Phase 1 contract guarantees the workload manifests remain identical.

The plan-task labels in this migration narrative align with the 7-task plan above: `kubectl wait` chain readiness → `wait-chain-ready`, the `kubectl get services` endpoint resolution → `resolve-endpoints`, the `envsubst | kubectl apply` profile ConfigMap → `render-config`, the seiload Job apply → `apply-job`, the `kubectl logs` tail + `kubectl wait --for=condition=complete` → `monitor-run`, the `aws s3 cp` summary upload (still workload-side via Pod Identity) → URL stamped at `mark-done`.

## Six open questions from #139 — answers

| # | Question | Decision | Source |
|---|---|---|---|
| 1 | Embed workload spec, or reference a separate `ValidationWorkload` CR? | **Embed.** Reserve `spec.runRef` field name only (no fields). | PM Round 1 Q1 |
| 2 | Probe `mode` enum: 3-mode, 5-mode, or defer? | **Defer.** v1 has no `mode` field; the only behavior is continuous polling. Add the field additively when `edge` or `window-end` semantics ship. | PM Round 1 Q2 + user input + Round 2 gate |
| 3 | `ValidationSuite`'s own scheduler vs `ValidationSchedule`-only? | **Moot in v1** — both deferred. Schedule is the lever; Suite is a child orchestrator. | PM Round 1 Q3 |
| 4 | S3 upload: top-level field, sidecar, or controller-side? | **Workload-side via Pod Identity.** Controller stamps URL only. | PM Round 1 Q4 + Phase 1 contract |
| 5 | Kueue integration day 1? | **No.** Defer until ≥3 concurrent suites contend. | PM Round 1 Q5 |
| 6 | Numeric vs stringified result aggregation? | **Stringified.** Adding numerics later is additive; removing them is breaking. | PM Round 1 Q6 + OTel one-way doors |

## Five one-way-door warnings from OSS survey — how design avoids each

1. **Don't bake workload I/O abstractions into the Run (Tekton's `PipelineResource`).** ValidationRun has no typed I/O abstraction. The workload contract is env vars + exit codes + termination message + S3 — text-shaped, not typed Kubernetes objects. Avoided.
2. **Don't lowercase your phase enum (Testkube's `passed`/`failed`).** All phase and verdict enums are Pascal-case (`Pending`, `Running`, `Succeeded`, `Failed`, `Error`, `Cancelled`; `Passed`, `Awaited`, `Error`). Tekton/Argo precedent. Avoided.
3. **Don't pick a kind name twice (K6's `K6→TestRun`, Litmus's `Experiment→Fault`).** ValidationRun / ValidationSuite / ValidationSchedule chosen with explicit OSS-precedent alignment ("Run" not "Execution"). Discriminator (`type=LoadTest|SequenceTest|IntegrationTest`) absorbs new test shapes inside one kind name. Avoided.
4. **Don't proliferate per-workload-type CRDs (Testkube's `TestWorkflow` consolidation).** One CRD with a discriminator union. Per-runner-type kinds explicitly rejected. Avoided.
5. **Don't orchestrate complex DAGs in your suite CR (Litmus outsourced to Argo).** `ValidationSuite` (when implemented) is flat — sequential or parallel, with `stopOnFailure`. Users needing DAG wrap suites in Argo. Documented as non-goal. Avoided.

## Resolved one-way-door decisions

These are decisions baked into the LLD that the council orchestrator surfaced for explicit user approval. The Round 2 gate closed on 2026-04-28; each row records the decision the user signed off on.

| # | Decision | Rationale | Reverse cost |
|---|---|---|---|
| 1 | API group **`validation.sei.io`** (separate from `sei.io`) | Validation versions independently of node-platform CRDs | High — group rename = full migration |
| 2 | Discriminator field name **`spec.type`** (not `spec.kind`) | Avoids visual collision with `TypeMeta.kind`; Litmus / K6 precedent | High — field rename = stored-object migration |
| 3 | **`chain.validators` and `chain.fullNodes` embed `SeiNodeDeploymentSpec` directly** | Field-experience parity; future SND fields free; controller-injected fields documented | Medium — switch to projection if SND breakage forces |
| 4 | **`chain.fullNodes` is required, not optional. No `endpointPolicy` knob.** Workload always connects to the fullNodes fleet. | Two-fleet topology codifies what both real consumers run; eliminates a one-way-door enum | Medium — adding a validator-only mode later requires bringing back an enum |
| 5 | **`Failed` vs `Error` distinction** at phase level (heartbeat ignores Error) | Two terminal failure modes carry different operational signals | Low (additive) |
| 6 | **Bucket prefix `s3://harbor-validation-results/{namespace}/{job}/{runId}/`** locked verbatim | Phase 1 already wires this | High — bucket migration |
| 7 | **No Flux labels on owned children** (controller invariant) | Flux would prune controller-managed objects | High — Flux drift incident |
| 8 | **Tenant pre-provisions SAs**; controller never IAM | Stay off the IAM control plane | High — auth model creep |
| 9 | **`alert.ruleRef` cross-namespace** allowed only into label-allowlisted namespaces | Multi-tenancy by namespace; controller-mediated escape hatch | Low (additive) |
| 10 | **Reserved env-var names rejected at admission via CEL XValidation** on `WorkloadSpec.env` | Hard reject is deterministic; avoids the silent-override surprise the prior draft proposed | Low (additive — names list extends additively) |
| 11 | **`query.threshold` typed as `string`** with regex CEL | OTel Round 1 schema-typing; switch numeric→string is breaking | High |
| 12 | **No `mode` field on `ValidationRule` in v1.** Continuous polling is the only behavior; field is additive when more modes ship. | Avoids locking in an enum value before semantics solidify | Low (additive) |
| 13 | **`runProperties.interval` (default 30s, min 5s, max 5m)** is load-bearing for continuous polling | Polling cadence per rule | Low (additive — bounds may relax) |
| 14 | **`runProperties.stopOnFailure` implemented in v1** (default `false`); controller deletes the workload Job to halt load when a rule trips | Real fast-fail path; not deferred | Low (additive — semantics expansion) |
| 15 | **Two conditions: `Succeeded` (Tekton-style) and `TestComplete` (monotonic Job-side advance)** | `TestComplete` lets `monitor-run` carry idempotent state across restarts; external consumers can render "workload done, rules aggregating" | Low (additive — extra conditions stay additive) |
| 16 | **`status.report.raw` cut entirely.** S3 URL is the only artifact pointer in `.status`. | Bounded `.status` size; URL is programmatically derivable; S3 is authoritative | Low (additive — re-add capped raw if a consumer demands without S3 access) |
| 17 | **Spec immutability** (CEL `self == oldSelf` on `spec.chain`, `spec.loadTest`, `spec.rules`) | Re-runs are new ValidationRun resources; mutation semantics undefined | Medium — relaxation requires versioning |
| 18 | **Plan task list collapses to 7** (`ensure-chain` → `wait-chain-ready` → `resolve-endpoints` → `render-config` → `apply-job` → `monitor-run` → `mark-done`) — `monitor-run` combines what was previously `wait-job-terminal` + `resolve-rules` + `evaluate-rules`; `mark-done` absorbs the trivial S3 URL stamp from `collect-report` | Reduces controller-side state-shuttling between tasks; per-rule polling state lives on `.status.rules[].lastEvaluatedAt` | Medium — splitting back out would touch every persisted Plan |

## Open dependencies

These are companion sub-issues the LLD discovers. **None block v1 ValidationRun.**

1. **SND-readiness includes catching_up.** Recommend option (b): seid `/health` endpoint returns 503 while `catching_up=true`, kubelet readiness probe consumes it, SND `phase=Ready` automatically waits. File against `sei-protocol/sei-k8s-controller` (SND/SeiNode reconciler). v1 ValidationRun ships without this and accepts genesis-bootstrapped chains as the dominant case.
2. **`TaskPlan.ErrorPhase` (or Reason-based dispatch).** v1 chooses Reason-based dispatch in the planner (no shared type change). Re-evaluate if a third controller needs the `Failed` vs `Error` distinction.
3. **SND inline `genesis` rejection on full-node SND.** ValidationRun's webhook rejects `chain.fullNodes.genesis`, but the SND controller itself does not enforce "full-node SND has no genesis ceremony" — it currently relies on the user not providing it. File a small SND-side validation tightening to mirror.
4. **PodMonitor for `sei-k8s-controller` itself.** Currently absent from `clusters/harbor/sei-k8s-controller/`. Add `:8080/metrics` plaintext PodMonitor; the new controller-side metrics in this LLD assume it. Per platform-engineer Round 1.
5. **Heartbeat PrometheusRule.** One global `clusters/harbor/monitoring/alerts/validation-heartbeat.yaml` querying `validation_run_terminal_total`. Per platform-engineer Round 1 — generic across tenants. File alongside controller manifest delivery, not as a v1 ValidationRun blocker.
6. **`validation` shared-rules monitoring namespace label.** `monitoring/` namespace gets `sei.io/validation-shared-rules=true` so `alert.ruleRef.namespace=monitoring` works. Single-line label add; document in tenant-onboarding README.
7. **Shard env-var injection mechanism.** `SHARD_INDEX`/`SHARD_COUNT` per pod requires either Indexed-Job + downward-API or controller-rendered per-pod env. Recommend Indexed-Job (`spec.completionMode=Indexed`) with `JOB_COMPLETION_INDEX` mapped to `SHARD_INDEX` via env-from-fieldRef. Already a Phase 1 contract reservation.

## Future work

Each future item names the trigger that un-defers it.

| Item | Un-defer trigger |
|---|---|
| `ValidationSuite` reconciler | First multi-Run flow that one human launching kubectl can't drive (≥3 chained Runs in CI). |
| `ValidationSchedule` reconciler | Tenant wants to retire their GHA cron. Track first request. |
| `container` rule type | Real heuristic ships outside Prometheus (e.g., balance reconciliation across two RPCs). |
| `window-end` rule mode (single instant query at run end) | Use case where continuous polling cost is undesirable AND the result is only meaningful at run end. Re-add `mode` field with `continuous` default. |
| `edge` rule mode (start-vs-end snapshot) | Spec-vs-end snapshot use case (e.g., "validators set didn't change during the run"). |
| `SequenceTest` discriminator body | First Cosmos-SDK upgrade test that needs ordered state changes. |
| `IntegrationTest` discriminator body | First "stand up a chain, run one container that does N RPC calls" workload. |
| `ReportExporter` controller (Pushgateway-shaped) | ≥2 consumers want the same numeric AND a stable report-schema field ships. |
| Kueue admission control | ≥3 simultaneous suites contending; Prometheus shows queueing. |
| `spec.runRef` (separate `ValidationDefinition` CR) | ≥10 distinct Runs sharing a workload definition; ConfigMap reuse insufficient. |
| Per-PR `ValidationTrigger` event-driven CR | CI cost budget approved; Testkube-style triggers explicitly demanded. |
| Numeric `actualValue`/`threshold` typed fields on rule status | Regression-detection consumer ships and benefits from type safety. |
| Multi-workload composition in one Run | Cross-workload barrier/sync requirements emerge. |
| Live mid-run RPC fleet refresh | HPA on RPC fleet during a run becomes desired. |
| Validator-only chain topology (no fullNodes fleet) | Consensus-poking workload demands it AND the operational cost of a one-knob enum is justified by ≥2 consumers. |

### Argo Workflows re-evaluation triggers

The /coral subcouncil unanimously concluded to ship the custom CRD (Path X) over Argo Workflows for v1 — the per-Run cost of a custom controller is small, and the surface area Argo would absorb (chain-readiness orchestration, structured rule verdicts, S3 URL stamping, stop-on-failure semantics) is the surface area the validation domain wants to own first-class. The Argo conversation should re-open under any of the following triggers:

- **Argo Workflows lands on Harbor for unrelated reasons** (data pipelines, CI consolidation). Marginal cost of a `WorkflowTemplate` + `CronWorkflow` drops to ~zero; `ValidationSchedule` becomes a free wrapping layer.
- **A fourth distinct test-shape with real DAG needs lands** (chaos-mesh-driven sequences, multi-cluster fanouts). DAG-shaped orchestration is what Argo is genuinely better at; building it inside `ValidationSuite` would be reinventing.
- **`ValidationSuite` needs DAG semantics, not flat sequential/parallel orchestration.** Same trigger as above, viewed from the Suite end.
- **300-pod polling cost becomes a non-issue (i.e., we drop continuous mode and revert to window-end only).** The custom controller's main value-add over Argo is the per-rule polling loop with stop-on-failure; if the polling story collapses to a single instant query, the loop's value collapses with it and Argo's `Workflow` shape becomes broadly competitive.

If any of these trigger, the conversation re-opens with a fresh /coral pass — not a unilateral pivot. The resolved decision stands until the trigger fires.

## References

### Round 0 + Round 1 + Round 2 artifacts (this council cycle)

- `sei-protocol/sei-k8s-controller#139` — entry brief, OSS survey, six open questions, five one-way-door warnings (`/tmp/validationrun-issue-body.md`)
- `sei-protocol/platform#235` — Phase 1 workload contract issue (`/tmp/phase1-issue-body.md`)
- Round 1 PM scope cuts (`/tmp/round1/pm-scope-cuts.md`)
- Round 1 OTel rule semantics + observability (`/tmp/round1/otel-rule-semantics.md`)
- Round 1 platform-engineer Harbor integration (`/tmp/round1/platform-integration.md`)
- Round 2 user gate decisions (closed 2026-04-28) — encoded in the "Resolved one-way-door decisions" table above

### In-repo (sei-k8s-controller)

- `/Users/brandon/sei-k8s-controller/CLAUDE.md` — controller patterns, RBAC marker convention, plan-driven reconciler
- `/Users/brandon/sei-k8s-controller/internal/planner/doc.go` — plan lifecycle, condition ownership, single-patch model
- `/Users/brandon/sei-k8s-controller/internal/planner/{planner.go,group.go,full.go,validator.go,replay.go,executor.go}` — existing builder idiom; `validationrun.go` will follow the same pattern
- `/Users/brandon/sei-k8s-controller/internal/task/{await_nodes_running.go,deployment.go,observe_image.go,task.go}` — controller-side task pattern, deserialize-map registration
- `/Users/brandon/sei-k8s-controller/api/v1alpha1/{seinode_types.go,seinodedeployment_types.go,common_types.go,validator_types.go,full_node_types.go,replayer_types.go}` — existing CRD type idioms (CEL XValidation, kubebuilder markers, listType=map listMapKey)
- `/Users/brandon/sei-k8s-controller/docs/design/composable-genesis.md` — LLD style template

### In-repo (platform)

- `/Users/brandon/platform/clusters/harbor/nightly/templates/seinodedeployment.yaml` — the working two-SND topology this design generalizes
- `/Users/brandon/platform/.github/workflows/k8s_nightly.yml` — bash glue this CRD subsumes (every `kubectl wait`/`kubectl exec`/`envsubst`/`aws s3 cp` step becomes a plan task)

### OSS survey — direct precedents (only patterns cited above)

- Tekton TaskRun docs — `Succeeded` condition shape — https://github.com/tektoncd/pipeline/blob/main/docs/taskruns.md
- Tekton TEP-0074 — PipelineResource deprecation (one-way-door warning) — https://github.com/tektoncd/community/blob/main/teps/0074-deprecate-pipelineresources.md
- Argo CronWorkflow — `ValidationSchedule` shape — https://argo-workflows.readthedocs.io/en/latest/cron-workflows/
- Litmus probes overview — `ValidationRule` analog (interval, runProperties) — https://docs.litmuschaos.io/docs/concepts/probes
- K6 Operator TestRun — execution-segment sharding via env injection — https://github.com/grafana/k6-operator/blob/main/api/v1alpha1/testrun_types.go
- Chaos Mesh Schedule types — annotation-based pause pattern — https://github.com/chaos-mesh/chaos-mesh/blob/master/api/v1alpha1/schedule_types.go
- Testkube TestExecution types — what NOT to do (lowercase enum, per-runner CRDs) — https://github.com/kubeshop/testkube-operator/blob/main/api/tests/v3/test_types.go
- Sonobuoy plugins doc — done-file convention (referenced for future `container` rule type) — https://github.com/vmware-tanzu/sonobuoy/blob/main/site/content/docs/main/plugins.md
