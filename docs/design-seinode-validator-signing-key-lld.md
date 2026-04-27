# Design: SeiNode — Validator Signing Key (LLD)

**Status:** Draft / LLD
**Date:** 2026-04-27
**Tracks:** validator migration from sei-infra (EC2) → sei-k8s-controller
**Related:** [`.tide/validator-migration.md`](../.tide/validator-migration.md), [`docs/design-seinode-import-volume-lld.md`](design-seinode-import-volume-lld.md), [`docs/design/composable-genesis.md`](design/composable-genesis.md)

This LLD specifies how an existing external validator identity is mounted onto a `SeiNode` so the node, once synced to the chain tip, takes over signing from a previously-running validator instance with zero risk of double-signing.

The companion direction doc (`.tide/validator-migration.md`) sets the threat model and migration phases. This LLD fixes the API shape, mount mechanics, validation lifecycle, and v1 scope. No new keystore variants, no remote-signer integration, no automated cutover orchestration — those are explicitly deferred (§11).

## 0. Background: what is already in tree

The bootstrap-Job + StatefulSet pattern that powers this design is already implemented:

- `ValidatorSpec.Snapshot *SnapshotSource` carries `BootstrapImage` + `S3.TargetHeight` (`api/v1alpha1/validator_types.go:9`, `api/v1alpha1/common_types.go:85-101`).
- The validator planner dispatches to `buildBootstrapPlan` when these are set (`internal/planner/validator.go:50-51`), validating the combo upfront (`internal/planner/validator.go:30-34`).
- `buildBootstrapPlan` runs a one-shot Job with `BootstrapImage`, halts at `TargetHeight`, tears down, then creates the production StatefulSet against the **same** `data-<name>` PVC (`internal/planner/bootstrap.go:47-95`).

So "Phase 1: pre-sync without keys" from the direction doc is already operational. This LLD adds the missing piece: how to land validator signing keys on the production StatefulSet pod.

## 1. CRD schema changes

A single new optional sub-struct is added to `ValidatorSpec` in `api/v1alpha1/validator_types.go`. The shape is a discriminated union with one v1 variant; future variants (TMKMS, remote signer, Vault) slot in as sibling pointer fields.

```go
// api/v1alpha1/validator_types.go

type ValidatorSpec struct {
    Snapshot        *SnapshotSource             `json:"snapshot,omitempty"`
    GenesisCeremony *GenesisCeremonyNodeConfig  `json:"genesisCeremony,omitempty"`

    // SigningKey declares the source of this validator's consensus signing
    // key (priv_validator_key.json). When omitted, the node runs as a
    // non-signing observer — suitable for pre-sync (Phase 1 of the
    // validator-migration runbook) or for genesis-ceremony bootstraps that
    // produce keys on-cluster.
    // +optional
    SigningKey *SigningKeySource `json:"signingKey,omitempty"`
}

// SigningKeySource declares where a validator's consensus signing key
// material comes from. Exactly one variant must be set. Variants are
// mutually exclusive — a validator has one consensus identity.
// +kubebuilder:validation:XValidation:rule="(has(self.secret) ? 1 : 0) == 1",message="exactly one signing key source must be set"
type SigningKeySource struct {
    // Secret loads signing material from a Kubernetes Secret in the
    // SeiNode's namespace.
    // +optional
    Secret *SecretSigningKeySource `json:"secret,omitempty"`

    // Future siblings: TMKMS *TMKMSSigningKeySource,
    // Remote *RemoteSigningKeySource, Vault *VaultSigningKeySource.
    // When added, update the XValidation exactly-one rule.
}

// SecretSigningKeySource references a Kubernetes Secret containing the
// validator's consensus signing key. The Secret must contain a data key
// `priv_validator_key.json` holding the Tendermint validator key (consensus
// identity), mounted read-only at $SEI_HOME/config/priv_validator_key.json.
// Rotating this value without a paired on-chain MsgEditValidator will cause
// the validator to miss blocks.
//
// priv_validator_state.json (CometBFT's slashing-protection ledger) is owned
// by seid on the data PVC and is created automatically on first start. The
// controller does not inject this file — see LLD §11.
type SecretSigningKeySource struct {
    // SecretName is the name of a Secret in the SeiNode's namespace.
    // The controller never creates, mutates, or deletes this Secret —
    // its lifecycle is fully external (kubectl, ESO, CSI Secrets Store, etc.).
    //
    // +kubebuilder:validation:MinLength=1
    // +kubebuilder:validation:MaxLength=253
    // +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
    // +kubebuilder:validation:XValidation:rule="self == oldSelf",message="secretName is immutable"
    SecretName string `json:"secretName"`
}
```

### Why these markers

- **`XValidation: exactly-one` on `SigningKeySource`** ships in v1 with one variant. Cost is one CEL expression that always returns 1; benefit is the rule body is reviewed once and v2 is a one-line edit (`+ (has(self.tmkms) ? 1 : 0)`). Skipping it now means a v2 PR adds the rule for the first time and a reviewer has to re-derive the variant invariant from scratch.
- **`secretName` immutable.** Re-pointing a running validator's consensus key is a slashing risk — force delete-and-recreate. Matches the immutability discipline applied to `import.pvcName` in the import-volume LLD §1.
- **Pattern + length** mirror the K8s DNS1123Label constraint on Secret names, catching typos at admission time.
- **`SigningKey` itself is not made immutable.** The cutover use case explicitly requires *adding* `SigningKey` to a pre-sync SeiNode that booted without it. The field can be added; once added, the variant choice and `secretName` are pinned.
- **No `Key` field for the data-key name.** The well-known name `priv_validator_key.json` matches the seid filename; an override field is YAGNI in v1.

### Backward compatibility

Existing SeiNodes serialize without `signingKey`. The new field is optional; old objects remain valid. The validator planner's existing genesis-ceremony and snapshot-bootstrap paths continue to behave identically when `SigningKey` is unset.

### Regenerated artifacts

- `zz_generated.deepcopy.go` — new `DeepCopy`/`DeepCopyInto` for `SigningKeySource` and `SecretSigningKeySource`; updated `ValidatorSpec.DeepCopyInto`. Produced by `make generate`.
- `manifests/sei.io_seinodes.yaml` and `config/crd/sei.io_seinodes.yaml` — new `spec.validator.signingKey` subtree with validation. Produced by `make manifests`.

## 2. Pod spec changes — production StatefulSet only

All changes land in `buildNodePodSpec` (`internal/noderesource/noderesource.go:229-270`), which generates the StatefulSet pod template. The bootstrap Job pod-spec generator (`task.GenerateBootstrapJob`) is **deliberately unchanged** — see §3.

When `node.Spec.Validator != nil && node.Spec.Validator.SigningKey != nil && node.Spec.Validator.SigningKey.Secret != nil`:

### 2.1 Add a Secret volume

```yaml
volumes:
- name: data
  persistentVolumeClaim: { claimName: data-<name> }   # existing
- name: signing-key                                    # NEW
  secret:
    secretName: <signingKey.secret.secretName>
    defaultMode: 0400
    items:
    - key: priv_validator_key.json
      path: priv_validator_key.json
```

One Secret-backed volume, scoped to a single data key. Mounted into the seid container via `subPath` — see §2.2.

### 2.2 Mount the key into the seid container

In `buildNodeMainContainer` (`internal/noderesource/noderesource.go:397`):

```yaml
volumeMounts:
- { name: data, mountPath: /sei }                                       # existing
- name: signing-key                                                     # NEW
  mountPath: /sei/config/priv_validator_key.json
  subPath: priv_validator_key.json
  readOnly: true
```

`subPath` is deliberate. Normally, a Secret-volume mount is auto-refreshed by the kubelet when the Secret changes (within seconds). A `subPath` mount is **not** refreshed — the file is pinned at pod-start until the next pod restart. For most workloads this is a footgun; for a consensus key, it's the safety property — a `kubectl edit secret` cannot swap the consensus key under a running seid (which would risk signing two different blocks at the same height under two different keys). Rotating the consensus key requires a deliberate pod restart paired with an on-chain `MsgEditValidator` — out of scope here.

The sidecar container does **not** mount `signing-key`. It has no business reading consensus material.

### 2.3 Pod security context

The seid container runs as a non-root user. The Secret volume's `defaultMode: 0400` plus the StatefulSet's existing `fsGroup` make the mounted file readable by the seid process. Test on the actual EKS CSI driver before staging — some CSI implementations interact poorly with `defaultMode` + `fsGroup`, leaving files as `root:root 0400` and locking out the non-root container.

### 2.4 The underlying-PVC stale-key concern

If a SeiNode previously bootstrapped without `SigningKey` (Phase 1) and later gets `SigningKey` added (Phase 2 cutover), the `seid-init` container during Phase 1 may have created a generated `priv_validator_key.json` on the PVC. The `subPath` mount overlays this file at runtime, so seid reads the Secret-mounted key. But the stale generated file remains on the PVC underneath the mount.

For v1, this is acceptable: `SigningKey` is immutable once set (per §1), so the overlay is permanent for the SeiNode's lifetime. The stale file is invisible to seid and never used. If a future operator unsets `SigningKey` (currently blocked by no field-level immutability rule, but not a supported workflow), they'd see the stale key surface — flagged in §11.

## 3. Bootstrap Job invariant

The bootstrap Job pod-spec is generated by `task.GenerateBootstrapJob`, a **separate code path** from `buildNodePodSpec`. The bootstrap Job mounts only the data PVC and runs `seid` to restore a snapshot and sync to halt-height. It does not consult `SigningKey` and never mounts the Secret.

This is the v1 safety property: **the bootstrap Job is physically incapable of signing**, because it has no key file in `$SEI_HOME/config/`. Even if a misconfigured cluster wedged a bootstrap Job into a running validator role, it would refuse to sign for lack of key material.

A one-line code comment at the head of `GenerateBootstrapJob` will pin this invariant explicitly so a future refactor doesn't accidentally share volume code with `buildNodePodSpec`:

```go
// GenerateBootstrapJob deliberately omits ValidatorSpec.SigningKey volumes.
// The bootstrap pod must never have access to consensus signing material —
// see docs/design-seinode-validator-signing-key-lld.md §3.
```

## 4. Pre-flight validation task

Like the import-volume LLD's `ensure-data-pvc` validation pattern (§2-3), we want the operator to learn that a referenced Secret is missing or malformed *before* the StatefulSet pod hits `ContainerCreating`-stuck purgatory. A new controller-side task validates the Secret's existence and shape, surfacing failures via a Status condition.

### Task: `validate-signing-key`

A new task type at `internal/task/validate_signing_key.go`. Inserted into the validator plan immediately before `apply-statefulset` (so the StatefulSet only attempts pod creation if the Secret is valid).

```go
// internal/task/validate_signing_key.go (sketch)

type ValidateSigningKeyParams struct {
    NodeName   string `json:"nodeName"`
    Namespace  string `json:"namespace"`
    SecretName string `json:"secretName"`
}
```

### Validation rules (in order)

| # | Check | Failure | State |
|---|---|---|---|
| 1 | Secret exists | `IsNotFound` | transient — operator may be about to apply it |
| 2 | `deletionTimestamp == nil` | being deleted | transient |
| 3 | `data["priv_validator_key.json"]` non-empty | missing/empty | terminal — no recovery without operator action |
| 4 | Value parses as valid JSON | malformed | terminal |
| 5 | Has expected Tendermint shape (top-level `address`, `pub_key.type`, `priv_key.type`) | malformed | terminal |

Transient failures keep the task `Running` and requeue at the executor's `TaskPollInterval` (5s), matching the `ensure-data-pvc` import path (import-volume LLD §3). Terminal failures mark the plan `Failed`.

### Reason strings (alerting contract)

```go
const (
    ReasonSigningKeyValidated         = "SigningKeyValidated"
    ReasonSigningKeySecretNotFound    = "SecretNotFound"
    ReasonSigningKeySecretTerminating = "SecretTerminating"
    ReasonSigningKeyMissingKeyFile    = "MissingPrivValidatorKey"   // terminal
    ReasonSigningKeyMalformedKey      = "MalformedPrivValidatorKey" // terminal
)
```

These mirror the `ImportPVC*` reason taxonomy from the import-volume LLD §2 and are part of the public alerting contract — adding a Reason is a minor-version addition; renaming or removing one is a breaking change.

### Plan integration

The validator plan is amended to insert `validate-signing-key` only when `SigningKey != nil`. Both bootstrap and base paths get the insertion:

- `buildBootstrapPlan` — insert after `EnsureDataPVC`, before `DeployBootstrapSvc`. Validation gates the bootstrap Job from even creating the Job resource.
- `buildBasePlan` (validator path) — insert after `EnsureDataPVC`, before `ApplyStatefulSet`.

For genesis-ceremony validators, `SigningKey` is mutually exclusive with `GenesisCeremony` (validation rule §6). Genesis-ceremony validators generate keys on-cluster and never reference an external Secret.

### RBAC

A new marker on the SeiNode controller:

```go
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
```

This is the only RBAC addition. `make manifests` regenerates `manifests/role.yaml`. The verbs are read-only — the controller never creates, updates, patches, or deletes Secrets. Operators provision the Secret out-of-band.

## 5. Status condition: `SigningKeyReady`

Mirrors `ImportPVCReady` from the import-volume LLD §4.

```go
// api/v1alpha1/seinode_types.go
const (
    ConditionNodeUpdateInProgress = "NodeUpdateInProgress"
    ConditionImportPVCReady       = "ImportPVCReady"
    ConditionSigningKeyReady      = "SigningKeyReady"  // new
)
```

### Status / Reason / Message matrix

| Status | Reason | Message template | When set |
|---|---|---|---|
| `True` | `SigningKeyValidated` | `Secret "<name>" passes all signing-key validation rules` | task transitions to Complete |
| `False` | `SecretNotFound` | `Secret "<name>" not found in namespace "<ns>"` | Get returns NotFound |
| `False` | `SecretTerminating` | `Secret "<name>" is being deleted` | deletionTimestamp set |
| `False` | `MissingPrivValidatorKey` | `Secret "<name>" missing required data key "priv_validator_key.json"` | data key missing/empty |
| `False` | `MalformedPrivValidatorKey` | `Secret "<name>" data key "priv_validator_key.json" is not a valid Tendermint validator key` | shape check fails |

### Condition lifecycle

- Set on every reconcile after `validate-signing-key` runs, by the planner (matching the planner-owns-conditions pattern from CLAUDE.md).
- Removed via `meta.RemoveStatusCondition` when `SigningKey` is unset on the spec.
- The condition's presence in `.status.conditions` is itself a signal that the node is configured to mount external signing keys.
- `ObservedGeneration` is set to `node.Generation` on every `meta.SetStatusCondition` call, mirroring `NodeUpdateInProgress` at `internal/planner/planner.go:195`.

### Why no `SigningKeyMounted=True/False` condition

We could express "the pod has actually picked up the key files." That's harder to observe — would require the sidecar to check filesystem state and report back, or a webhook into seid's logs. For v1, `SigningKeyReady=True` + the pod being `Ready` per its standard probes is sufficient signal that mounting succeeded; mount failure surfaces as `kubectl describe pod` events on the StatefulSet pod. Deferred: a richer `Signing=True/False` condition driven by sidecar observation of seid's RPC `signing_info` or proposer-presence on chain (§11).

## 6. Validator planner validation additions

In `validatorPlanner.Validate` (`internal/planner/validator.go:17-36`), add:

```go
if v.SigningKey != nil {
    if v.GenesisCeremony != nil {
        return fmt.Errorf("validator: signingKey is mutually exclusive with genesisCeremony")
    }
    if v.SigningKey.Secret == nil {
        return fmt.Errorf("validator: signingKey requires a variant (secret); none set")
    }
    if v.SigningKey.Secret.SecretName == "" {
        return fmt.Errorf("validator: signingKey.secret.secretName is required")
    }
}
```

The `SigningKey ↔ GenesisCeremony` mutual-exclusion check ensures a fresh-chain validator (which generates keys via the genesis ceremony) is not also configured to import keys from a Secret. The CRD-level `XValidation: exactly-one` covers `SigningKeySource` variants but not cross-field exclusion with `GenesisCeremony` — that goes here.

The `SigningKey + Snapshot.BootstrapImage` combination is **valid and expected** — that's the migration use case (bootstrap-Job sync + signing key on production StatefulSet). No additional validation needed.

The `SigningKey + neither GenesisCeremony nor Snapshot` combination is also valid — it represents a validator joining an existing chain via block-sync from genesis (slow but correct). We do not require a snapshot mechanism alongside `SigningKey`.

## 7. Finalizer interaction

Like imported PVCs (import-volume LLD §5), Secrets referenced by `SigningKey` are managed externally and must never be deleted by the controller. The finalizer path in `internal/controller/node/controller.go` requires no changes — it currently deletes the data PVC and cleans up metrics, neither of which references the signing-key Secret. Added as an explicit code-comment in the finalizer to prevent future accidental coupling:

```go
// Note: SigningKey-referenced Secrets are managed externally (operator,
// ESO, CSI Secrets Store). The controller never reads, writes, or
// deletes these Secrets — see docs/design-seinode-validator-signing-key-lld.md §7.
```

## 8. Cutover flow (operational)

This LLD does not change the cutover *runbook* in `.tide/validator-migration.md:229-285` — it makes the *controller* support the runbook. The flow is:

```
Phase 1: Pre-sync (no SigningKey)
  kubectl apply <SeiNode without SigningKey>
    → controller: buildBootstrapPlan
    → bootstrap Job runs with halt-height
    → production StatefulSet starts as non-signing observer
    → seid block-syncs / state-syncs to chain tip
    → SeiNode.status.phase = Running
    → No priv_validator_key.json on disk (or stale-from-seid-init, ignored)
    [old EC2 validator continues signing]

Phase 2: Cutover (add SigningKey)
  [stop old EC2 validator at height H; confirm process is dead]
  [scrape priv_validator_key.json from old EC2]
  [wait for chain to advance ≥ M blocks past H — defense against re-org at cutover boundary]
  [create K8s Secret <name>-signing-key containing priv_validator_key.json]
  kubectl patch seinode <name> --patch '{"spec":{"validator":{"signingKey":{"secret":{"secretName":"<name>-signing-key"}}}}}'
    → controller observes spec change
    → planner builds NodeUpdate plan (SigningKey added → StatefulSet diff)
    → controller runs validate-signing-key
       → Secret exists, key present and well-formed → SigningKeyReady=True
    → apply-statefulset patches the StatefulSet pod template
    → pod restarts with signing-key Secret mounted at config/priv_validator_key.json
    → seid starts, finds no priv_validator_state.json on the PVC, creates a fresh one (height=0)
    → first signing opportunity is at chain tip ≫ H → seid signs (not a re-sign of any height the old validator signed)

Phase 3: Verify
  Operator checks on-chain signing_info; controller reports SigningKeyReady=True
```

The patch in Phase 2 triggers a NodeUpdate plan because the StatefulSet pod template now references new volumes — this falls out of the existing image-drift NodeUpdate machinery (CLAUDE.md "NodeUpdate plans"). Pod restart is the Phase 2 cutover event.

### Note on slashing protection during cutover

CometBFT auto-creates `priv_validator_state.json` on first start with zero state. The new validator's first signing opportunity is at chain tip, which is well past the old validator's last-signed height once the chain has advanced ≥ M blocks (M=20 is generous for Sei). The runbook's "wait M blocks before applying SigningKey" step is the operational protection against the narrow case where a chain re-org at the cutover boundary could produce a height-collision with the old validator. The controller does not inject `priv_validator_state.json` — see §11.

### Why NodeUpdate naturally handles this

The existing NodeUpdate plan path (`internal/planner/planner.go`, when `spec.image != status.currentImage` — but more generally, when the StatefulSet pod template differs from the desired template) already orchestrates `apply-statefulset → observe-image → mark-ready`. Adding signing-key volumes to the pod template is a structurally-equivalent diff. We rely on this mechanism unchanged for v1; richer "validator-promotion" plan semantics are deferred (§11).

## 9. Tests

### Unit tests

**API validation** (`api/v1alpha1/validator_types_test.go`):

- `SigningKey_SecretOnly_Valid`
- `SigningKey_NoVariant_RejectedByXValidation`
- `SigningKey_SecretNameImmutable` — apply with one name, patch with another, expect rejection
- `SigningKey_WithGenesisCeremony_RejectedByPlannerValidate`

**Pod spec generation** (`internal/noderesource/noderesource_test.go`):

- `SigningKeySet_SecretVolumePresent` — `signing-key` volume exists on the StatefulSet pod template, scoped to `priv_validator_key.json`
- `SigningKeySet_SeidContainerHasSubPathMount` — seid container has `subPath: priv_validator_key.json` mount at `/sei/config/priv_validator_key.json`
- `SigningKeySet_SidecarHasNoSigningMount` — sidecar container does not mount the signing volume
- `SigningKeyUnset_NoSigningVolume` — regression guard for the absence path
- `BootstrapJob_NeverHasSigningVolume` — even when `SigningKey` is set on the SeiNode, `task.GenerateBootstrapJob` produces a Job with no signing-related volume (the §3 invariant)

**Validate-signing-key task** (`internal/task/validate_signing_key_test.go`):

Table-driven, fake-client pattern matching `ensure_pvc_test.go`. One case per row of §4's validation table (transient + terminal variants), plus:

- `ValidSecret_Completes`
- `SecretNotFound_Transient_ThenAppears_Completes` (apply Secret between reconciles)

### Integration / e2e

- `Controller_ValidatorWithSigningKey_ReachesRunningSigning` — apply SeiNode with SigningKey, Secret pre-applied; expect Phase=Running, SigningKeyReady=True, key file present at expected mount path inside pod
- `Controller_ValidatorPhase1ThenPhase2_PodRestartsWithKeys` — apply without SigningKey, wait for Running, patch SigningKey in, observe pod restart with mounts
- `Controller_ValidatorWithMissingSecret_StuckTransient` — apply with SigningKey but no Secret; expect SigningKeyReady=False/SecretNotFound; apply Secret; expect convergence
- `Controller_ValidatorWithMalformedSecret_PlanFailed` — Secret exists but `priv_validator_key.json` is not valid JSON or wrong shape; expect plan Failed and SigningKeyReady=False with terminal Reason
- `Controller_ValidatorDeletion_PreservesSecret` — delete SeiNode, confirm referenced Secret still exists

## 10. Observability

### Metrics (`internal/controller/observability/`)

- `signingKeyValidationTotal` (counter, attrs: controller, namespace, result∈{valid,transient,terminal})
- `signingKeyTerminalTotal` (counter, attrs: controller, namespace, reason) — independent counters per terminal Reason for alerting
- `signingKeyTimeToValid` (histogram, attrs: controller, namespace) — observed at task Complete, from `SubmittedAt` → now

### Events

Emitted by the reconciler after status flush:

- `SigningKeyValidated` (Normal) on transition to True
- `SigningKeyValidationFailed` (Warning) when False-condition Reason changes

### Dashboards / alerts

Out of scope for this LLD — they live in `~/workspace/platform/clusters/prod/monitoring/`. The Reason strings in §4 and §5 are the public contract those alerts will key on.

## 11. What this LLD does NOT cover

- **Variants beyond `Secret`.** TMKMS, Horcrux, remote signer (Web3Signer / Vault / AWS-KMS-fronted), Tendermint KMS protocol over a Unix socket. Add as sibling fields under `SigningKeySource` when in-house validators need them; the union is shaped to accept them additively.
- **Automated cutover orchestration.** The cutover in §8 is operator-driven (manual stop of EC2, manual scrape of keys, manual `kubectl apply`). Automating this end-to-end is `.tide/validator-migration.md` Decision #2 — explicitly deferred until first manual cutover succeeds.
- **`priv_validator_state.json` injection.** This file is CometBFT's slashing-protection ledger. seid auto-creates it (height=0) on first start and owns it on the PVC thereafter; on pod restart it's read from the PVC, not from any external source. For the migration use case, transferring the old validator's state file is unnecessary because the cutover runbook already enforces a hard halt of the old validator and a wait for chain advance ≥ M blocks past `last_signed_height` before activating the new instance — at that point the new validator's first signing opportunity is far past anything the old validator signed. The file is also operational data, not secret material (height/round/step plus the most recent signature, all of which are public on-chain). If a future use case needs explicit state injection (e.g., automated chain-rollback tooling), add a separate ConfigMap-based source distinct from `SigningKey`.
- **Double-sign detection.** Out-of-band monitoring (slashing-info polling, sentry comparisons) is the right venue. Not a controller responsibility.
- **Sentry-node topology** (private validator behind public sentries). Architecture decision orthogonal to keying; deferred.
- **Consensus key rotation.** Rotating `priv_validator_key.json` requires an on-chain `MsgEditValidator` and is a coordinated operation, not a `kubectl edit secret`. Out of scope; runbook concern.
- **Unsetting `SigningKey` after it's been set.** Currently no field-level immutability rule prevents unset-after-set, but the workflow is unsupported (pod would restart without keys, validator would stop signing, miss blocks → jail). If a future use case requires demote-to-non-signing, add a controlled demotion plan rather than relying on unset.
- **Cross-namespace Secret references.** SecretName resolves in the SeiNode's namespace only.
- **HSM integration.** The natural variant for HSM is a remote-signer-style sibling under `SigningKeySource`, not a Secret-based shape. Defer until concrete HSM platform is selected.

## 12. Migration / rollout

**No migration required.** Existing SeiNodes serialize without `signingKey`; the controller reads it as nil and follows existing code paths unchanged. The new CRD schema is additive.

**Controller upgrade is idempotent.** Any running SeiNode whose plan completed before this version is unaffected. A SeiNode with `SigningKey` already set in spec but plan not yet started will pick up the new `validate-signing-key` task on the next reconcile; if the Secret is valid, the task completes immediately and the plan continues.

**Operator rollout:**

```bash
make manifests generate
make test
make lint
make docker-build IMG=<registry>/sei-k8s-controller:<sha>
make docker-push IMG=<registry>/sei-k8s-controller:<sha>
# update Helm/flux values; controller restart is disruption-free
```

No CRD data migration. Applying the new CRD on a cluster with old SeiNode objects is safe.

## Related work

- `.tide/validator-migration.md` — direction doc; threat model, phased migration, runbook.
- `docs/design-seinode-import-volume-lld.md` — adjacent LLD; sets in-house style for K8s API design (immutability, validation, condition lifecycle).
- `docs/design/composable-genesis.md` — sketches `register-validator` for the *new-chain validator joining* case (deferred for migration use case).
- `api/v1alpha1/validator_types.go` — new fields per §1.
- `api/v1alpha1/seinode_types.go` — new condition constant per §5.
- `internal/noderesource/noderesource.go:229-447` — pod-spec changes per §2.
- `internal/task/validate_signing_key.go` — new task per §4.
- `internal/planner/validator.go:17-36` — planner Validate additions per §6.
- `internal/planner/bootstrap.go`, `internal/planner/planner.go:411` — plan integration per §4.
