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

## 0.1 Design choice: externalize cert ownership; spec is immutable

Three coupled decisions, each enabling the next:

**a) The controller does not provision the cert.** Operator/platform tooling (cert-manager `Certificate` in GitOps, or any other source) creates the `kubernetes.io/tls` Secret. The controller references it by name and gates progression on presence + cert validity. This matches every other security-sensitive resource in the SeiNode spec (`signingKey`, `nodeKey`, `operatorKeyring`, imported `PVC`).

**b) `spec.sidecar.tls` is immutable post-creation.** Toggling TLS on an existing SeiNode is *not* a controller-orchestrated transition — it's a delete + recreate. The data PVC is retained externally (today via `spec.dataVolume.import.pvcName` pre-detach; future-cleaner via `spec.dataVolume.retainOnDelete`). At ~10 nodes for the foreseeable internal fleet, manual cycling is operationally fine and gives each transition a clean Pending → Running boundary instead of a mid-flight transport-mode flip.

**c) The cert→SeiNode contract is machine-readable, not docs-only.** The controller publishes `status.sidecarTLS.{secretName, requiredDNSNames}` whenever TLS is enabled. Platform tooling reads this directly rather than re-deriving the K8s service DNS convention from documentation. The controller validates the Secret's cert SANs against this list before allowing the pod to schedule.

What collapses under these decisions:
- The drift detector (`sidecarTLSDrift`) — gone; immutable spec means no drift on Running nodes.
- The status mirror (`status.currentSidecarTLS`) — gone; immutable spec means status = spec for TLS.
- The condition reason expansion (`TLSToggleStarted`, `UpdateAndTLSToggleStarted`) — gone; toggle isn't a NodeUpdate.
- `classifyPlan` discrimination — back to its pre-PR shape.
- The `ApplySidecarCert` task and `GenerateSidecarCertificate` resource generator — gone; cert is external.
- The `ObserveSidecarTLS` task — gone; nothing to observe on a Running node.
- The Issuer trust-anchor security surface (`SidecarTLSSpec.IssuerName`/`IssuerKind`) — gone; trust-anchor selection happens entirely in platform tooling, outside the controller's blast radius.

What stays:
- `noderesource` pod/service generation with the existing `if SidecarTLSEnabled(node) { ... add proxy }` branching.
- `ApplyRBACProxyConfig` task (the kube-rbac-proxy authz ConfigMap is controller-owned and depends on SeiNode namespace/name).

## 1. CRD schema changes

```go
// api/v1alpha1/common_types.go — SidecarConfig.TLS

// SidecarTLSSpec, if set, fronts the sidecar API with kube-rbac-proxy
// on :8443 using TLS material from a Secret in the SeiNode's
// namespace. The Secret is operator-provisioned (e.g., via a
// cert-manager Certificate in the platform GitOps repo); this
// controller does not create it.
//
// Immutable post-creation. Toggling TLS on an existing SeiNode is a
// delete + recreate operation; data persists via the SeiNode's PVC
// retention mechanism.
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
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.sidecar.tls) || (has(self.sidecar.tls) && self.sidecar.tls == oldSelf.sidecar.tls)",message="spec.sidecar.tls is immutable; delete + recreate the SeiNode to change TLS configuration"
```

(Exact CEL pending — the rule needs to handle nil sidecar gracefully. May land at the `SidecarConfig` level instead.)

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

`buildRunningPlan` reverts to its pre-PR-#254 shape: image drift + sidecar re-approval, no TLS handling. There is no TLS drift to detect.

If an operator attempts to mutate `spec.sidecar.tls`, the CRD CEL rejects the API request — no controller code runs.

If the externally-provisioned Secret rotates (cert-manager renewal): kube-rbac-proxy's existing `--tls-reload-interval=30s` flag picks up the new material from the same Secret mount; no pod restart needed; no controller action. The pre-flight validation re-runs on each reconcile and continues stamping `SidecarTLSSecretReady=True` as long as the new cert still has matching SANs.

If the Secret SAN coverage *changes* such that the contract breaks (e.g., wrong SANs after a misconfigured re-issuance): pre-flight flips the condition to `SidecarTLSSecretReady=False, Reason=SANsMismatch`. The Running node continues to serve traffic with the now-wrong cert (kube-rbac-proxy doesn't care about controller-side validation); operators get a visible signal via the condition and can fix the cert. The pod doesn't cycle; this is observability, not enforcement.

## 5. Operator workflow for "enable TLS on existing fleet"

The arctic-1 / atlantic-2 / pacific-1 use case:

1. Platform tooling pre-provisions a `kubernetes.io/tls` Secret per SeiNode (cert-manager `Certificate` resources, GitOps-managed) with SANs matching the published `status.sidecarTLS.requiredDNSNames` — readable in advance from a dry-run of the target spec or from a known template.
2. Per SeiNode (one at a time, governed by ops policy):
   a. Detach the data PVC (existing `spec.dataVolume.import.pvcName` flow, or set `spec.dataVolume.retainOnDelete: true` if that lands first).
   b. Delete the SeiNode.
   c. Create a new SeiNode with the same name, `spec.sidecar.tls.secretName` set, and `spec.dataVolume.import.pvcName` pointing at the retained PVC.
   d. Wait for `Phase=Running`.

At ~10 nodes total, this is a one-time script. No SND-level orchestration changes needed; the SND already supports its existing rollout primitives.

## 6. Out of scope (deferred or unrelated)

- **`spec.dataVolume.retainOnDelete: bool`** as a one-step PVC retention primitive (replaces the two-step pre-detach workflow). Small follow-up issue. Not blocking.
- **SND-level retain-on-delete** ("delete the SND, keep the SeiNodes") — independently useful as a "drop the orchestrator, keep the fleet" escape hatch but unrelated to this LLD. Separate follow-up if desired.
- **In-place TLS toggle on Running SeiNodes** — explicitly rejected in §0.1(b). Re-open as a separate design if the fleet scale or operational model changes such that delete + recreate is no longer feasible.
- **CA rotation observability** (cert fingerprint, SecretResourceVersion in status) — moot under the externalized-cert model; cert content is not the controller's concern.

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
