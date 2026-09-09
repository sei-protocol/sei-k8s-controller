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
VolumeAttributesClass (as a QoS class the CSI driver applies, EBS CSI on
Kubernetes ≥ 1.31 / GA-track by 1.34, matching the production EKS control
plane). So the design questions are: which object carries the parameters, who
creates it, and what the CRD field selects.

### Decision

- **VolumeAttributesClass (VAC), referenced by name.** The CRD carries a VAC
  name (plus the volume-claim size); the controller stamps it onto each node's
  PVC and otherwise passes it through. The controller MAY read-only pre-flight
  the referenced VAC so a missing/typo'd name surfaces as a named node
  condition rather than a silently-Pending pod. It MUST NOT create VACs.
- **The platform owns the VAC catalog** (GitOps), exactly as it owns the
  StorageClasses today. Adding a new (IOPS, throughput) offering is a
  platform/GitOps change, not a controller change or a CRD change.
- **The harness owns the IOPS/throughput menu.** It exposes the supported set
  (Req 3.1/3.4/3.5), prompts for the parameters (Req 3.2), and maps a selection
  to a supported VAC name, which is what lands on the rendered CRD.

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
- **The controller takes on no cluster-scoped write.** Referencing a VAC needs
  at most a cluster-scoped *read* (`volumeattributesclasses: get;list;watch`) for
  the pre-flight — no new write privilege on a namespaced-workload controller.

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
- The controller change is minimal: a VAC-name field on the CRD, passthrough to
  the PVC, and an optional read pre-flight condition.

### Open — verify before PR 5

- Confirm the harbor EKS runs an `aws-ebs-csi-driver` recent enough to support
  VolumeAttributesClass, that the `VolumeAttributesClass` feature gate is
  enabled, and that the driver's IAM role holds `ec2:ModifyVolume`. If the
  plumbing is absent, fall back to the curated-StorageClass-by-name alternative
  above — same CRD field shape, no re-spec.
