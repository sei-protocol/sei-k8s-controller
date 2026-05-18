# Design: SeiNode — Sidecar TLS via Externally-Provisioned Secret (LLD)

**Status:** Draft / LLD
**Date:** 2026-05-16
**Tracks:** prod TLS rollout (arctic-1, atlantic-2, pacific-1)
**Related:** sei-protocol/platform#545 (per-chain internal CA issuers), seictl#165 (init-only TLS gap)
**Tracking issue:** #255
**Supersedes:** closed PR #254 (in-place toggle approach) and the prior LLD revision that lived on that branch.

## 0. Background

`spec.sidecar.tls` provisions a kube-rbac-proxy fronting the sei-sidecar API on `:8443`. Today the controller owns the entire cert lifecycle: it generates a cert-manager `Certificate` from `spec.sidecar.tls.{IssuerName, IssuerKind}`, waits for cert-manager to mint the Secret, mounts it. This couples a per-SeiNode long-lived resource (the Certificate) to the controller's plan-task flow and makes the controller's surface area grow disproportionately to its actual concerns.

The first attempt at supporting TLS toggle on Running nodes (PR #254) added a drift detector, a status mirror (`status.currentSidecarTLS`), a new condition reason, an observer task, and a cert-manager-Ready terminal-failure path inside a wait task. Cross-review caught two blockers (cert-manager stuck-forever path; Issuer trust-anchor is unvalidated attacker-controllable input) and several non-blockers. While the in-PR fixups addressed the cert-manager blocker, the Issuer-trust-anchor concern stayed deferred because the proper fix needed admission-policy work and per-chain CA decisions outside the controller's scope. The deferred surface plus the planner-discrimination complexity (classifyPlan task-list-sniffing, condition reason expansion, plan-flag composition) signaled that the design itself was layering complexity rather than removing it.

This revision drops that approach entirely. The cert is no longer the controller's concern; it becomes platform-provisioned, mirroring how `spec.validator.signingKey`, `spec.validator.nodeKey`, `spec.validator.operatorKeyring`, and `spec.dataVolume.import.pvcName` already work.

## 0.1 Design choice: externalize cert ownership; spec is set-once

Three coupled decisions:

**a) The controller does not provision the cert.** Operator/platform tooling (cert-manager `Certificate` in GitOps, or any other source) creates the `kubernetes.io/tls` Secret. The controller references it by name and gates progression on presence + cert validity. This matches every other security-sensitive resource in the SeiNode spec (`signingKey`, `nodeKey`, `operatorKeyring`, imported `PVC`).

**b) `spec.sidecar.tls` is set-once.** Once set, the value is immutable: disable (`set → nil`) and swap (`A → B`) are rejected at admission via CEL. The one allowed transition is **enable post-creation** (`nil → set`), which triggers a NodeUpdate-style plan that cycles the pod with the proxy mount. This decision evolved across two PRs: #258 made the spec fully immutable on the rationale that delete + recreate at ~10 nodes was operationally fine; #262 un-deferred the enable case after operator validation that per-node delete + recreate is more friction than warranted, but kept disable and swap blocked because they remain genuinely irreversible (swap in particular implies a new trust anchor, which warrants a clean Pending → Running boundary). The transport-mode-flip-during-cycle concern is addressed by sourcing transport selection from `status.currentSidecarTLSSecretName` (observed) rather than spec.

**c) The cert→SeiNode contract is machine-readable, not docs-only.** The controller publishes `status.sidecarTLS.{secretName, requiredDNSNames}` whenever TLS is enabled. Platform tooling reads this directly rather than re-deriving the K8s service DNS convention from documentation. The controller validates the Secret's cert SANs against this list before allowing the pod to schedule.

**d) Status is the source of truth for transport selection.** `SidecarURLForNode` reads `status.currentSidecarTLSSecretName` (observed), not `spec.sidecar.tls` (desired). The controller's sidecar client transport (cmd/main.go `newSidecarClient`) keys off the same observed field. Mid-rollout, the controller talks HTTP to the still-HTTP pod; after `ObserveSidecarTLS` stamps the mirror, both URL and transport flip together. This is the load-bearing fix for the windowing bug that bit the original PR #254 attempt.

**Load-bearing trust assumption:** the controller's `SidecarURLForNode` consumes `status.currentSidecarTLSSecretName`. Any actor with `seinodes/status` patch permission could flip the controller's transport selection (DoS on the reconcile loop, not data exfil — the sidecar still enforces its own auth). The existing RBAC (`manifests/role.yaml`) scopes status-write to the controller's own ServiceAccount only. This is canonical for controller-runtime managers but is now load-bearing; an admission-time check (ValidatingAdmissionPolicy) preventing non-controller writes to this field would be defense in depth.

What lives in the controller under these decisions:
- Drift detector `sidecarTLSEnableDrift` — narrow: spec set + status empty + preflight `SidecarTLSSecretReady=True`. Disable and swap aren't detector-handled because CEL blocks them at admission.
- Status mirror `status.currentSidecarTLSSecretName` — drives transport selection; stamped only on rollout convergence (`StatefulSet.Status.ReadyReplicas >= *Replicas`) so a crashlooping proxy doesn't flip transport prematurely.
- `ObserveSidecarTLS` task — polls StatefulSet rollout and stamps the mirror; injected before any sidecar HTTP task in every plan that brings up a TLS-enabled pod.
- `ApplyRBACProxyConfig` task — controller-owned (the ConfigMap's SAR depends on namespace/name).

What stays externalized:
- The `kubernetes.io/tls` Secret containing cert + key.
- Trust-anchor selection (Issuer / ClusterIssuer reference) — happens entirely in whatever produces the Secret.

## 1. CRD schema changes

```go
// api/v1alpha1/common_types.go — SidecarConfig.TLS

// SidecarTLSSpec, if set, fronts the sidecar API with kube-rbac-proxy
// on :8443 using TLS material from a Secret in the SeiNode's
// namespace. Operator-provisioned; the controller does not create it.
// Set-once: enabling on an existing SeiNode is allowed (triggers a
// NodeUpdate-style pod cycle); disable and swap are CEL-rejected.
//
// +optional
TLS *SidecarTLSSpec `json:"tls,omitempty"`
```

```go
// SidecarTLSSpec references an externally-provisioned TLS Secret.
type SidecarTLSSpec struct {
    // SecretName is a kubernetes.io/tls Secret in the SeiNode's
    // namespace. The cert SANs must include the DNS names published
    // in status.sidecarTLS.requiredDNSNames; the controller validates
    // this before allowing the pod to schedule.
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:MinLength=1
    SecretName string `json:"secretName"`
}
```

CRD-level immutability:

```go
// SeiNodeSpec
// +kubebuilder:validation:XValidation:rule="(!has(oldSelf.sidecar) || !has(oldSelf.sidecar.tls)) ? (!has(self.sidecar) || !has(self.sidecar.tls)) : (has(self.sidecar) && has(self.sidecar.tls) && self.sidecar.tls == oldSelf.sidecar.tls)",message="spec.sidecar.tls is immutable; delete + recreate the SeiNode to change TLS configuration"
```

Status additions:

```go
// SeiNodeStatus
//
// SidecarTLS is set whenever spec.sidecar.tls is non-nil. Publishes
// the contract platform tooling must satisfy when provisioning the
// TLS Secret. Machine-readable replacement for naming-convention docs.
// +optional
SidecarTLS *SidecarTLSStatus `json:"sidecarTLS,omitempty"`
```

```go
// SidecarTLSStatus declares the controller's expectations of the
// referenced TLS Secret.
type SidecarTLSStatus struct {
    // SecretName mirrors spec.sidecar.tls.secretName for visibility.
    SecretName string `json:"secretName"`

    // RequiredDNSNames is the SAN list the cert in SecretName must
    // include. Derived from the SeiNode's headless service DNS.
    RequiredDNSNames []string `json:"requiredDNSNames"`
}
```

New condition:

```go
// ConditionSidecarTLSSecretReady indicates whether the externally-
// provisioned TLS Secret referenced by spec.sidecar.tls.secretName is
// present, well-formed, and has SANs matching the required DNS names.
// Mirrors SigningKeyReady / NodeKeyReady / OperatorKeyringReady.
ConditionSidecarTLSSecretReady = "SidecarTLSSecretReady"
```

Reasons: `Ready`, `NotFound`, `Malformed`, `SANsMismatch`.

## 2. Pre-flight validation (steady-state reconcile)

A reconcile branch parallel to the existing `validateSigningKey`/`validateNodeKey`/`validateOperatorKeyring` pre-flight checks:

```go
// internal/controller/node/preflight.go (sketch)

func (r *SeiNodeReconciler) reconcileSidecarTLSReady(ctx context.Context, node *seiv1alpha1.SeiNode) {
    if !noderesource.SidecarTLSEnabled(node) {
        meta.RemoveStatusCondition(&node.Status.Conditions, seiv1alpha1.ConditionSidecarTLSSecretReady)
        node.Status.SidecarTLS = nil
        return
    }

    required := requiredDNSNames(node)
    node.Status.SidecarTLS = &seiv1alpha1.SidecarTLSStatus{
        SecretName:       node.Spec.Sidecar.TLS.SecretName,
        RequiredDNSNames: required,
    }

    reason, msg := validateTLSSecret(ctx, r.APIReader, node, required)
    status := metav1.ConditionFalse
    if reason == seiv1alpha1.ReasonTLSSecretReady {
        status = metav1.ConditionTrue
    }
    meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
        Type: seiv1alpha1.ConditionSidecarTLSSecretReady,
        Status: status, Reason: reason, Message: msg,
        ObservedGeneration: node.Generation,
    })
}
```

`validateTLSSecret` performs:
1. Get the Secret named `spec.sidecar.tls.secretName` in `node.Namespace`.
2. Check Secret type is `kubernetes.io/tls`.
3. Check `tls.crt` and `tls.key` are non-empty.
4. `x509.ParseCertificate(tls.crt)` — must parse cleanly.
5. Verify `cert.DNSNames` is a superset of `required`.

Outcomes:
- All checks pass → `Reason=Ready`.
- Secret missing → `Reason=NotFound`.
- Secret present but `tls.crt`/`tls.key` empty or PEM-unparseable → `Reason=Malformed`.
- Cert parses but SANs don't include required names → `Reason=SANsMismatch`.

`requiredDNSNames(node)` returns:
```
{name}.{namespace}.svc.cluster.local
{name}-0.{name}.{namespace}.svc.cluster.local
```
(Mirrors the DNS list `GenerateSidecarCertificate` used under the prior design.)

## 3. Init plan changes

`ResolvePlan` gates init-plan creation on `ConditionSidecarTLSSecretReady=True` when TLS is enabled — same pattern as `SigningKeyReady` already gates init for nodes with referenced signing keys (`api/v1alpha1/seinode_types.go`). The SeiNode stays `Pending` with the condition false until the operator/platform provisions a valid Secret. No init plan runs; no resources get created until the trust contract is satisfied.

`buildBasePlan` keeps its TLS branch but emits one task instead of two:

```go
// Before:
if noderesource.SidecarTLSEnabled(node) {
    prog = append(prog, task.TaskTypeApplySidecarCert, task.TaskTypeApplyRBACProxyConfig)
}

// After:
if noderesource.SidecarTLSEnabled(node) {
    prog = append(prog, task.TaskTypeApplyRBACProxyConfig)
}
```

`ApplyRBACProxyConfig` stays (controller-owned). `ApplySidecarCert` is deleted.

There is no `WaitForSidecarTLSSecret` task in the plan — its job is absorbed into pre-flight validation (§2). If §2 reports `SidecarTLSSecretReady=True`, the Secret is already proven present and valid; the plan can proceed directly to `ApplyStatefulSet`.

## 4. Running-state behavior

`buildRunningPlan` detects two drifts: image drift (existing) and TLS-enable drift (added via #262). Both compose into one NodeUpdate plan when they co-fire; a single pod cycle covers both.

TLS-enable drift fires only on the `nil → set` transition (operator added `spec.sidecar.tls` to an existing SeiNode that previously had none). Disable and swap are CEL-rejected at admission; the detector never sees them. The plan composes `ApplyRBACProxyConfig → ApplyStatefulSet → ApplyService → ReplacePod → [ObserveImage if image drift] → ObserveSidecarTLS → MarkReady`. `ObserveSidecarTLS` polls StatefulSet rollout convergence (requires `ReadyReplicas >= *Replicas` so a crashlooping proxy doesn't flip transport prematurely), then stamps `status.currentSidecarTLSSecretName`.

If the externally-provisioned Secret rotates in place (cert-manager renewal with the same SANs): kube-rbac-proxy's existing `--tls-reload-interval=30s` flag picks up the new material from the same Secret mount; no pod restart needed; no controller action. Pre-flight re-runs each reconcile and continues stamping `SidecarTLSSecretReady=True`.

If the Secret SAN coverage breaks (wrong SANs after a misconfigured re-issuance, Secret deleted, etc.): pre-flight flips the condition to `SidecarTLSSecretReady=False`. Plan creation is gated for any TLS-required SeiNode whose condition is not Ready. The Running node keeps serving on whatever cert kube-rbac-proxy is already bound to (no controller action can force a reload-from-bad-Secret), so the user-visible effect is "running pod stays as-is, no new rollouts until the operator fixes the Secret."

**Mid-rollout transport invariant:** `SidecarURLForNode` reads `status.currentSidecarTLSSecretName`; cmd/main.go's `newSidecarClient` keys its HTTP-doer choice on the same field. The two move in lockstep. During the enable rollout, both stay on HTTP while the old pod still listens on `:7777`; once `ObserveSidecarTLS` stamps the mirror, both flip to HTTPS for the new pod.

## 5. Operator workflow

**Enable TLS at SeiNode creation:** apply a SeiNode with `spec.sidecar.tls.secretName` referencing a pre-provisioned `kubernetes.io/tls` Secret. SeiNode stays `Pending` with `SidecarTLSSecretReady=False/NotFound` until the Secret resolves; init plan runs once the condition flips to True.

**Enable TLS on an existing Running SeiNode** (the arctic-1 / atlantic-2 / pacific-1 fleet path):

1. Platform tooling provisions a `kubernetes.io/tls` Secret with SANs matching `status.sidecarTLS.requiredDNSNames` (pre-computable as `{name}.{ns}.svc.cluster.local` + `{name}-0.{name}.{ns}.svc.cluster.local`).
2. Operator patches `spec.sidecar.tls.secretName` on the running SeiNode.
3. Preflight validates the Secret on the next reconcile; condition flips to True.
4. `sidecarTLSEnableDrift` fires; planner builds a NodeUpdate plan composing the proxy `ConfigMap` apply + StatefulSet/Service regen + pod cycle + observer + MarkReady.
5. SeiNode stays in `Phase=Running` throughout; transport selection lags spec until `ObserveSidecarTLS` confirms `ReadyReplicas`.

**Disable or swap:** delete + recreate the SeiNode. PVC retention via existing `spec.dataVolume.import.pvcName` (pre-detach) or future `spec.dataVolume.retainOnDelete`. Disable and swap remain CEL-rejected on existing objects because they require a clean Pending → Running boundary that mid-flight orchestration cannot provide.

## 6. Out of scope (deferred or unrelated)

- **`spec.dataVolume.retainOnDelete: bool`** as a one-step PVC retention primitive (replaces the two-step pre-detach workflow). Small follow-up issue. Not blocking.
- **SND-level retain-on-delete** ("delete the SND, keep the SeiNodes") — independently useful as a "drop the orchestrator, keep the fleet" escape hatch but unrelated to this LLD. Separate follow-up if desired.
- **In-place TLS disable or swap on Running SeiNodes** — CEL-rejected. Re-open as a separate design only if a coherent rollout path exists; today neither has one (disable would leave the proxy in place mid-flight serving stale state; swap implies a trust-anchor change that warrants Pending → Running).
- **CA rotation observability** (cert fingerprint, SecretResourceVersion in status) — moot under the externalized-cert model; cert content is not the controller's concern.
- **Admission-policy hardening of `status.currentSidecarTLSSecretName` writes.** The current trust assumption is that only the controller's ServiceAccount has `seinodes/status` patch permission. A ValidatingAdmissionPolicy preventing non-controller writes to this specific field would be defense in depth. Tracked separately.

## 7. Cross-cutting (operational notes)

- **Secret naming.** The `spec.sidecar.tls.secretName` field is explicit (no conventional default) so platform tooling can co-locate certs across SeiNodes if desired (e.g., wildcard certs, though unusual). Convention-by-default would couple operator tooling to the SeiNode name.
- **PVC retention.** Existing `spec.dataVolume.import.pvcName` is the supported retention escape hatch. `retainOnDelete` is a future ergonomic improvement, not a correctness requirement.
- **kube-rbac-proxy authz config.** Still controller-owned via `ApplyRBACProxyConfig` — the SAR contents depend on SeiNode namespace/name, which the operator can't reasonably template externally without re-deriving the same convention.

## 8. Verification

- Unit: `validateTLSSecret` matrix — Secret missing, wrong type, empty `tls.crt`/`tls.key`, unparseable cert, parseable cert with matching SANs, parseable cert with missing SANs.
- Unit: `requiredDNSNames` returns the expected DNS list for various namespace/name pairs.
- Unit: `reconcileSidecarTLSReady` sets/clears the condition + status struct correctly across TLS-enabled/disabled transitions on the same in-memory node object (immutability is enforced at CRD level, but the reconcile branch must handle nil-spec gracefully on first observation of a new SeiNode).
- Unit: CRD CEL — attempt to mutate `spec.sidecar.tls` on a Running SeiNode is rejected at API server.
- envtest: SeiNode with TLS but missing Secret stays Pending; provisioning the Secret transitions to Running.
- envtest: SeiNode with TLS and Secret-with-wrong-SANs stays Pending with `Reason=SANsMismatch`; correcting the Secret transitions to Running.
- End-to-end on harbor: create a SeiNode with TLS referencing a manually-applied Secret; verify the pod serves on `:8443` and the controller-side sidecar client connects via TLS. Delete + recreate with TLS removed; verify the new pod serves plain HTTP on the legacy port.
