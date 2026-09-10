# Decision records — 001 Selectable node resources

Design decisions taken while implementing spec 001. Each records a choice the
spec's requirements do not dictate, so it is not re-litigated later. The spec
states the *what*; these state the *how* and *why*.

## DR-001 — Storage performance is selected as a named VolumeAttributesClass

**Status:** Accepted — 2026-09-09.

**Scope:** Requirement 3 (selectable storage parameters), the storage half of
Requirement 2 (typed CRD field) and Requirement 7 (precedence), and SC-003.

### Context

An engineer benchmarking a storage-bound workload must select the gp3 volume's
IOPS and throughput per node group. Kubernetes offers no way to put arbitrary
driver parameters inline on a PVC — IOPS/throughput always come from a
cluster-scoped object the PVC references: a StorageClass (at provision) or a
VolumeAttributesClass (as a QoS class the CSI driver applies). VAC reached GA in
Kubernetes 1.34, which matches the production EKS control plane; the earlier beta
(1.31) is not a usable floor here, because managed EKS does not let a user enable
a beta feature gate or the `storage.k8s.io/v1beta1` group. So the design
questions are: which object carries the parameters, who creates it, and what the
CRD field selects.

### Decision

- **VolumeAttributesClass (VAC), referenced by name.** The CRD carries a VAC
  name (plus the volume-claim size); the controller stamps it onto each node's
  PVC at provision and otherwise passes it through. It MUST NOT create VACs.
- **CRD surface.** The selection lands **under the `spec.dataVolume.storage`
  object** — that path is a field-path prefix, never a bare quantity — so
  `SeiNetwork.spec.dataVolume` inherits the same object: the storage **size**
  at `spec.dataVolume.storage.resources.requests.storage`, in the volume-claim
  shape Req 2.2/2.5 call for (PR 4 ships this as a narrow
  `VolumeClaimResources` — same JSON shape as
  `corev1.VolumeResourceRequirements` but request-only, since a volume claim has
  no limit dimension), and the VAC selection as a sibling name field at
  `spec.dataVolume.storage.volumeAttributesClassName` (mirroring the PVC field).
  The size has one home, the volume-claim field — not two. The size leaf is no
  longer provisional: PR 4 shipped it as `DataVolumeStorage.Resources` →
  `VolumeClaimResources.Requests["storage"]` (`api/v1alpha1/seinode_types.go`),
  with CEL positively requiring the `storage` key. Only the
  `volumeAttributesClassName` leaf is still open; PR 5 finalizes that one name,
  and the record fixes its shape so implementation does not settle it by default.
  Because `SeiNetwork.spec.dataVolume` is the same type, the selection inherits
  that field's spec-level create-only CEL (`seinetwork_types.go:29`), which
  rejects a change, an unset, **and a first-time set**. So at the network level
  the selection is fixed for the pool's life: a network created without one can
  never gain it, and changing a validator pool's storage means recreating the
  **whole network** — not the delete-and-recreate of a single node in the
  provision-time bullet below, which is the node-level remedy. (PR 4 rewrites
  that rule to be canonicalization-safe once a size Quantity lands under it —
  the same int-or-string fix applied to the compute footprint — without
  relaxing its create-only semantics.)
- **The controller SHALL pre-flight the referenced VAC (read-only)** and surface
  the result as an always-present node condition (a `Ready`-family type with a
  stable `CamelCase` reason, per the repo's Conditions standard) — a missing or
  mistyped name reports `False/<reason>` instead of leaving a silently-Pending
  pod. This is committed, not optional: the named-failure path in Consequences
  depends on it. The condition is present even when no VAC is selected — `True`
  in that steady state, since the mode-default storage is used — never absence,
  per the Conditions standard's rule against expressing "not configured" as a
  missing condition.
- **The selection is provision-time only in this iteration.** `ensure-data-pvc`
  is Get-then-Create with no update path
  (the `executeCreate` path in `internal/task/ensure_pvc.go`), so the VAC name and size bind when the
  PVC is first created. Nothing in the planner replaces a node on storage drift
  either — NodeUpdate plans are built on `spec.image != status.currentImage` — so
  changing storage on a running node group is an **operator act: delete and
  recreate the node.** This is not symmetric with a compute change, which rolls
  the pod in place (`apply-statefulset`/`replace-pod`) with the volume intact: a
  controller-provisioned PVC carries an `ownerReference` to the SeiNode
  (`ctrl.SetControllerReference` in `executeCreate`), so deleting the node garbage-collects its data volume.
  For a benchmark node that data loss is usually acceptable, but it is real and
  the operator should expect it. Live `ModifyVolume`-driven retuning of a bound
  volume is deferred until an update path exists — so the live-modification
  capability cited under Rationale is a property of the mechanism, not something
  this iteration exercises.
- **Storage selection resolves in the same precedence ladder as compute
  (Req 7).** The ladder is specifically about the **VAC name**: a selection on
  the CRD wins; with none set, the PVC carries **no `volumeAttributesClassName`
  at all**, and the mode-default StorageClass supplies the baseline performance.
  This is a distinct PVC field from `storageClassName`, which `GenerateDataPVC`
  sets unconditionally from `noderesource.DefaultStorageForMode`
  (`internal/noderesource/noderesource.go:706-712`) today, selection or not.
  There is no app-config middle rung for the VAC name in this iteration. The
  **size** has a fuller ladder, because an app-config rung already exists
  (`storage.sizeDefault`/`sizeArchive`/`sizeSeed`, `platform.go:62-72`, resolved
  through `DefaultStorageForMode`): a CRD-set `resources.requests.storage`
  **overrides** the platform per-mode size, and an unset size falls through to
  that existing per-mode default. This record fixes that override direction so
  PR 5 does not settle it by default.
- **The selection covers controller-provisioned volumes only.** An imported PVC
  (`spec.dataVolume.import`) keeps the importer's class and parameters — the
  controller validates but never mutates it (`seinode_types.go:167`) — so
  `dataVolume.storage` and `dataVolume.import` are mutually exclusive, and
  Req 3.3's "every node in the group" is scoped to nodes whose volume the
  controller provisions.
- **The platform owns the VAC catalog** (GitOps), exactly as it owns the
  StorageClasses today. Adding a new (IOPS, throughput) offering is a
  platform/GitOps change, not a controller change or a CRD change.
- **The harness owns the IOPS/throughput menu.** It exposes the supported set
  (Req 3.1/3.4/3.5), prompts for the parameters (Req 3.2), and maps a selection
  to a supported VAC name, which is what lands on the rendered CRD.

For gp3, the StorageClass fixes the base volume *type* at provision while the VAC
carries the tunable *performance* parameters (IOPS, throughput); the supported
set the harness exposes is the set of VACs. "Storage type" in Req 3.1/3.4/3.5 is
the base type the class provisions — gp3 for this iteration — so the two objects
do not compete for authority over it.

Under this split, two runs that differ only in throughput render CRDs that
differ only in the `volumeAttributesClassName` field — SC-003, read at the
selector rather than at a raw parameter field.

### Rationale

- **It is how mature workload operators select storage.** CloudNativePG,
  Zalando postgres-operator, Strimzi, ECK, and the Prometheus Operator all take
  a StorageClass/VAC *name* and pass it to the PVC; none introspect parameters.
  The class name is the interface boundary between workload and storage, and the
  workload stays ignorant of driver-specific parameter semantics. This also
  matches the existing house style: `noderesource.DefaultStorageForMode` already
  resolves a mode to a class *name*.
- **VAC is the purpose-built mechanism** for per-volume IOPS/throughput on EBS,
  and the EBS CSI driver does the actual volume work (provision-time attributes,
  and `ModifyVolume` for live changes). The controller does not touch AWS.
- **The controller takes on no cluster-scoped write.** The committed pre-flight
  requires a cluster-scoped *read* (`volumeattributesclasses: get;list;watch`) —
  and nothing more; no write privilege on a namespaced-workload controller.

### Alternatives rejected

- **Controller mints StorageClasses or VACs on demand.** This is the literal
  "make one available for any requested value," but both are cluster-scoped
  objects, so it requires a cluster-scoped `create` driven by a namespaced
  workload spec — a privilege escalation — and a namespaced object cannot own a
  cluster-scoped one, so the created objects have no `ownerReference` and no
  garbage collection (they leak or orphan). No mature workload operator does
  this; storage/CSI operators (Rook, OpenEBS, the CSI driver operators) own
  class lifecycle because storage is their entire job. Rejected for this
  controller.
- **Curated StorageClass matrix, referenced by name.** Viable and RBAC-free, but
  it bakes IOPS/throughput into the StorageClass name, so SC-003's "differ only
  in throughput" would surface as a differing class-name string with the
  parameters implicit. VAC keeps the parameters in the semantically-correct
  object. (If VAC plumbing proves unavailable — see the open item — this is the
  fallback, and the CRD field shape is unaffected: a name is a name.)
- **CRD accepts raw IOPS/throughput numbers; the controller resolves them to a
  VAC.** Rejected as less conventional: it adds a `volumeattributesclasses: list`
  read and couples the controller to EBS parameter semantics, for a mapping that
  belongs in the harness (which already owns the prompt and the supported set).

### Consequences

- Engineers select from a platform-managed catalog. A novel (IOPS, throughput)
  point is a quick GitOps PR; the controller's named-failure condition tells the
  operator exactly which VAC to add. Truly arbitrary, zero-GitOps values would
  require a dedicated storage-catalog controller that owns minting with proper
  RBAC and GC — deliberately **deferred**; revisit only if the friction proves
  real after the event.
- The controller change is minimal for a fresh render: a VAC-name field on the
  CRD, passthrough to the PVC at provision, and the read pre-flight condition.
  Changing the VAC on an already-provisioned volume is deliberately out of scope
  this iteration (see the provision-time note in Decision).

### Open — verify before PR 5

Confirm the VAC plumbing on the harbor EKS. Split by which path each item serves
— this iteration applies a VAC only at provision, so only the first pair blocks
PR 5:

**Blocks PR 5 (provision-time VAC apply):**

- the `aws-ebs-csi-driver` addon (and its external-provisioner) is recent enough
  to support VolumeAttributesClass — the provisioner carries the VAC's mutable
  parameters into `CreateVolume` at provision;
- the usable API is the 1.34 GA `storage.k8s.io/v1` group — do not rely on the
  1.31 beta gate, which managed EKS does not expose.

**Blocks only the deferred live-retune follow-up (not PR 5):**

- the addon's `external-resizer` sidecar is running with VAC support and volume
  modification enabled — it handles `ModifyVolume` when the VAC changes on an
  existing PVC, which this iteration never does;
- the driver's IAM role holds `ec2:ModifyVolume`.

If the plumbing is absent, fall back to the curated-StorageClass-by-name
alternative above. The CRD field shape is unaffected (a name is a name), but the
VAC-specific wording this decision added to the spec — the glossary term
(`spec.md:44`), Req 3.2/3.3/3.6, SC-003, and the assumptions paragraph — would
need rewording to a generic "named storage class."
