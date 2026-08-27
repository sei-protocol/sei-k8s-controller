# Freeze node as a first-class SeiNode configuration

| | |
|---|---|
| Status | Implemented. Two items need approval before merge — see Decisions requiring approval. |
| Repository | `sei-protocol/sei-k8s-controller` |
| Depends on | `sei-config` v0.0.27 (`chain.freeze_height`), sei-chain `release/v6.6` |
| Reviewed by | `kubernetes-specialist`, `systems-engineer`, `platform-engineer` |
| Author | Brandon Chatham |
| Date | 2026-08-27 |

## Semantic Anchors

EARS · RFC 2119 · CEL `XValidation` · ADR (Nygard) · compatibility law for the
Kubernetes API · discriminated-union spec

## Problem

A frozen node runs `seid` with a freeze height. It executes blocks up to that
height, then stops, and it serves query RPC at that height indefinitely. The
platform has no way to express this.

Three routes exist to set a freeze height. None of them works today.

| Route | State |
|---|---|
| CLI flag | Closed. The controller builds a fixed argv, `{start, --home, <dataDir>}`, at `internal/noderesource/noderesource.go:940` and `:998`. No escape hatch exists. |
| CRD field | Absent before this change. |
| `spec.overrides` | Open since `sei-config` v0.0.27, and wrong. See the next section. |

## Why `spec.overrides` is not the answer

`spec.overrides` is a flat `map[string]string` that the controller forwards
without interpretation. Two consequences make it unfit.

**The controller cannot derive its own behaviour from it.** A frozen node needs a
different readiness probe (FN-8). The controller would have to parse an opaque
string map to know that. A typed field makes the intent a fact the controller can
act on.

**A user override silently outranks the controller.** `mergeOverrides` copies user
overrides last, at `internal/planner/planner.go:836`. An operator who sets
`chain.freeze_height` in `spec.overrides` therefore beats any value the
controller derives from a typed field. Only one source can stay writable.

## Scope

### In scope

- A `freeze` sub-spec on the `fullNode` and `archive` modes of `SeiNode`.
- CEL validation for create-only presence, immutable height, and the four
  combinations that seid refuses or silently degrades.
- Controller emission of `chain.freeze_height` as a controller-owned override.
- A readiness probe that suits a node whose height never advances.
- A pod label the height-based alert rules can exclude on.

### Out of scope

- Packaging `frozen-rpc-router` as a container image. It has no image today, and
  it cannot carry WebSocket. Track separately.
- The platform manifests for a frozen node, and its public exposure. See
  Platform preconditions for what that work covers.
- Routing across a fleet of frozen nodes at different heights.
- A `status` field or condition that reports the frozen height. See Open
  questions.
- A `Freeze` field on the `sdk/sei` `NodeSpec`. The SDK's two `Config`
  passthroughs reach `spec.overrides` and `spec.configOverrides`, so an SDK user
  who sets `chain.freeze_height` there now gets an API rejection with no
  SDK-level signal. The SDK cannot express a frozen node at all.

## Requirements

Each requirement uses one EARS template. A test MUST name the ID it covers.

### CRD contract

**Independent Test** — apply candidate `SeiNode` manifests against envtest with
no controller running. The API server alone accepts the valid shapes and rejects
each invalid one. This group needs no reconcile loop to verify.

**FN-1.** The `SeiNode` CRD MUST provide a `freeze` sub-spec on the `fullNode`
and `archive` mode sub-specs. The sub-spec is OPTIONAL.

**FN-2.** The `freeze` sub-spec MUST carry a `height` field of type `int64` with
a minimum of 1. The field is REQUIRED.

**FN-3.** WHEN an update to a `SeiNode` changes `freeze.height`, the API server
MUST reject the update.

**FN-3a.** WHEN an update to a `SeiNode` adds or removes the `freeze` sub-spec,
the API server MUST reject the update. A field-level transition rule cannot
express this, because CEL skips a rule that reads `oldSelf` when the path is
absent from the stored object. The rule therefore lives on the mode sub-spec,
where `has()` is observable on both sides.

**FN-4.** The `freeze` sub-spec MUST NOT appear on the `validator`, `seed`, or
`replayer` modes.

**FN-5.** IF `spec.overrides` contains the key `chain.freeze_height`, THEN the
API server MUST reject the `SeiNode`.

**FN-6.** IF `freeze` and `snapshotGeneration` are both set on the same mode
sub-spec, THEN the API server MUST reject the `SeiNode`.

**FN-10.** IF a mode sub-spec sets `freeze` and `spec.overrides` contains
`chain.halt_height` or `chain.halt_time`, THEN the API server MUST reject the
`SeiNode`. Rationale: `sei-config` refuses to resolve the combination, so the
node otherwise wedges at config-apply after a successful merge.

**FN-11.** IF `freeze` is set and `snapshot.s3.targetHeight` is at or above
`freeze.height`, THEN the API server MUST reject the `SeiNode`. Rationale: seid
refuses to start once a store has reached the freeze height, so the node enters a
permanent CrashLoopBackOff.

**FN-12.** IF `freeze` and `snapshot.stateSync` are both set, THEN the API server
MUST reject the `SeiNode`. Rationale: seid disables state sync under freeze and
falls back to block sync from genesis without failing, so an operator who asks
for a fast bootstrap silently gets a multi-week one.

### Controller behaviour

**Independent Test** — call the planner and `noderesource` builders directly with
a frozen and an unfrozen `SeiNode`, then assert the emitted config intent, the
probe shape, and the pod labels. This group needs no cluster.

**FN-7.** WHERE the operator sets `freeze`, the controller MUST emit
`chain.freeze_height` as a controller-owned override carrying `freeze.height`.

**FN-8.** WHERE the operator sets `freeze`, the controller MUST NOT configure the
`/lag_status` readiness probe. It MUST configure an HTTP probe on `/status` at
the RPC port instead.

**FN-9.** WHERE the operator omits `freeze`, the controller MUST configure the
readiness probe exactly as it does today. This requirement bounds the change.

**FN-13.** WHERE the operator sets `freeze`, the controller MUST stamp
`sei.io/frozen: "true"` on the pod template. Rationale: see the alerting entry
under Platform preconditions.

### Constraints

**Independent Test** — run `make manifests generate` in CI and assert an empty
diff, and diff the served CRD schema against the previous release.

**NFR-1.** The change MUST be additive. It MUST NOT alter the type, validation,
or meaning of any existing served field. **FN-5 and FN-10 contradict this**, and
the contradiction is deliberate — see D-4.

**NFR-2.** The author MUST regenerate `manifests/` and
`api/v1alpha1/zz_generated.deepcopy.go` with `make manifests generate`, and MUST
NOT edit either by hand.

**NFR-3.** The author MUST express every rule in FN-3, FN-3a, FN-5, FN-6, FN-10,
FN-11, and FN-12 as a `+kubebuilder:validation:XValidation` marker, not in
reconcile code alone. Rationale: the API server rejects a bad spec at admission;
reconcile code cannot.

## Interface

```go
// FreezeSpec holds a node at a block height. The node executes through
// Height-1, then stops while it continues to serve query RPC. seid refuses to
// freeze a validator, so only the non-consensus modes carry this sub-spec.
type FreezeSpec struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="height is immutable"
	Height int64 `json:"height"`
}
```

`int64` matches every other height in this CRD — `TargetHeight` at
`common_types.go:140`, `UpgradeHeight` at `seinodetask_types.go:250`. It is not
the `uint64` that `sei-config` uses. The narrower type also makes
`sei-config`'s `MaxInt64` bound unreachable from this path.

CEL rules, stated as expressions:

```
# SeiNodeSpec — FN-5
!has(self.overrides) || !('chain.freeze_height' in self.overrides)

# SeiNodeSpec — FN-10
!((has(self.fullNode) && has(self.fullNode.freeze)) || (has(self.archive) && has(self.archive.freeze)))
  || !has(self.overrides)
  || (!('chain.halt_height' in self.overrides) && !('chain.halt_time' in self.overrides))

# FullNodeSpec and ArchiveSpec — FN-6
!has(self.freeze) || !has(self.snapshotGeneration)

# FullNodeSpec and ArchiveSpec — FN-3a
has(self.freeze) == has(oldSelf.freeze)

# FullNodeSpec — FN-11
!has(self.freeze) || !has(self.snapshot) || !has(self.snapshot.s3)
  || self.snapshot.s3.targetHeight < self.freeze.height

# FullNodeSpec — FN-12
!has(self.freeze) || !has(self.snapshot) || !has(self.snapshot.stateSync)
```

## Placement rationale

The `freeze` sub-spec belongs on both `fullNode` and `archive`.

Pruning happens at commit, and a stopped node stops pruning. A frozen full node
therefore serves only the window it had retained when it stopped. That suits a
query for state at one height. It does not suit a query for an arbitrary earlier
block.

A frozen archive node retains the whole history and serves any block below the
freeze height.

Both shapes are legitimate. The mode chooses which history the node keeps.
Offering `freeze` on `fullNode` alone would give an operator the cheap shape when
they asked for the complete one.

## Why FN-3a is the load-bearing rule

Three reviewers converged on the same hole, and two reproduced it against a live
1.34 API server. Without FN-3a the CRD advertises immutability and delivers
add-and-remove.

Adding `freeze` to a Running node produces the worst state in the design:

1. Admission accepts the update.
2. `reconcileStatefulSet` runs an unconditional server-side apply on every
   reconcile (`internal/controller/node/controller.go:161`), so the pod template
   gets the frozen readiness probe immediately.
3. `chain.freeze_height` never arrives. Only `TaskConfigApply` carries a
   `ConfigIntent`, and it appears only in bootstrap programs. A Running node's
   plan carries `TaskConfigPatch`, which writes `config.toml [p2p]` keys alone.
4. `UpdateStrategy: OnDelete` leaves the live pod untouched, so nothing looks
   wrong.
5. Weeks later a routine image bump or a Karpenter consolidation replaces the
   pod. It comes back as an ordinary chain-following RPC node **with lag-based
   readiness removed**, and it reports Ready however far behind it falls.

Removing `freeze` is the mirror failure: `app.toml` keeps `freeze-height` while
the probe reverts to `/lag_status`, so the node goes NotReady permanently at the
next pod replacement. The operator's obvious remedy for a mistake is the action
that breaks it.

FN-3a makes both states unreachable at admission.

## Success Criteria

The work closes when all of the following hold.

| # | Outcome | How to measure |
|---|---|---|
| SC-1 | An operator declares a frozen node in one manifest, with no `spec.overrides` entry and no manual edit on the volume. | The manifest carries only `freeze.height`. |
| SC-2 | The API server rejects every invalid shape in FN-3 through FN-12. | The envtest cases for those IDs pass. |
| SC-3 | A frozen node reaches Ready and stays Ready for at least 24 hours past its freeze height. | Pod readiness on a real cluster. |
| SC-4 | The rendered `app.toml` on a frozen node carries the expected `freeze-height`. | Read the file on the running pod. |
| SC-5 | No node without `freeze` changes behaviour. | A rendered-StatefulSet diff against the previous commit is empty for every unfrozen shape. |
| SC-6 | `make manifests generate` leaves no diff. | CI. |
| SC-7 | `NodeFellBehind` does not fire for a frozen node. | The alert rule excludes `sei.io/frozen`. |

SC-3 needs a cluster. SC-5 was measured: a reviewer rendered
`GenerateStatefulSet` for seven node shapes at both commits and the diff is
empty.

## Decisions requiring approval

**D-1. A struct, not a bare `freezeHeight int64`.** Approved. A struct leaves
room for a retention policy or a routing hint.

**D-2. `height` is immutable, and presence is create-only.** Approved in
substance; FN-3a delivers what D-2 already said ("Replace the node to change
it"). The first implementation under-delivered it.

**D-3. Placement on `fullNode` and `archive`.** Approved.

**D-4. A CEL guard on `spec.overrides` rather than a change to merge order.**
Approved, and it needs a precondition. **This still needs sign-off**, because
FN-5 and FN-10 narrow validation on an existing served field, which NFR-1
forbids. A reviewer reproduced the consequence: a stored `SeiNode` that already
carries `chain.freeze_height` in `spec.overrides` becomes un-updatable for any
spec edit, and `SeiNetworkReconciler` performs full updates on children, so such
an object would error every loop.

Run this before the CRD upgrade reaches any cell:

```sh
kubectl get seinodes -A -o json | jq -r '.items[]
  | select(.spec.overrides["chain.freeze_height"] != null
        or .spec.overrides["chain.halt_height"]  != null
        or .spec.overrides["chain.halt_time"]    != null)
  | "\(.metadata.namespace)/\(.metadata.name)"'

kubectl get seinetworks -A -o json | jq -r '.items[]
  | select(.spec.configOverrides["chain.freeze_height"] != null)
  | "\(.metadata.namespace)/\(.metadata.name)"'
```

Empty output on every cell retires the concern. Non-empty output is a blocker.

**D-5. `SeiNetwork.spec.configOverrides` is left unguarded.** Not yet decided.
`SeiNetworkReconciler` copies that map verbatim into every child `SeiNode`. The
API server would accept a freeze key at the network, then reject it on every
child update. That is a permanent error loop, not a clean rejection where the
operator edited. SeiNetwork children are validators only, so the key is never
legitimate there. The mirror rule is cheap. It is out of this change only because
the scan above decides whether it is urgent.

## Platform preconditions

None of these live in this repository. All of them gate a working frozen node,
and the reviewers found each one by reading the platform repository.

**A Flux `force: enabled` annotation turns a rejected edit into a deletion.**
Every prod `SeiNode` carries `kustomize.toolkit.fluxcd.io/force: enabled`.
Flux's `IsImmutableError` returns true for any `422 Invalid`, which is what every
CEL rejection returns, and the force annotation then makes it delete and
recreate the object. Nothing enforces
`sei.io/deletion-protected: "true"` — no policy, no webhook, and no controller
reads it. An operator who edits
`freeze.height` in git therefore triggers a delete, and `deleteNodeDataPVC`
removes the data PVC unless the operator imported the volume. The frozen state is the
product, so this destroys it.

The frozen node's manifest MUST omit `force: enabled` and SHOULD set
`kustomize.toolkit.fluxcd.io/ssa: IfNotPresent`. This is the single most
important precondition in this document.

**The controller's own Service bypasses readiness.** The per-node headless
Service sets `PublishNotReadyAddresses: true`
(`internal/noderesource/noderesource.go:625`). Routing public traffic at it
discards everything FN-8 achieves. The frozen node MUST get its own ClusterIP
Service selecting `sei.io/nodedeployment`, matching the existing
`*-external.yaml` pattern.

**No cell can accept `chain.freeze_height` today.** Every cell's
`images.sidecar` predates the sidecar move into this repository. Bumping that
cell-global value rolls every node in the cell — 31 in prod, including
`pacific-1` mainnet. The escape is `spec.sidecar.image` on the frozen node
alone: a new node has no `status.currentSidecarImage`, so no other node drifts.

**The alert rules need the FN-13 label.** `NodeFellBehind` selects on
`sei.io/role` and fires once the chain advances past its threshold beyond the
freeze height. It never resolves, because the condition never clears. The rule
MUST exclude `sei.io/frozen`.

**The frozen node MUST import its data volume.** Three reasons, each
independent. The CRD carries no per-node storage class or size, so a
controller-provisioned PVC is 40Ti or 2000Gi for a node that never grows. The
controller deletes the data PVC on node deletion unless the operator imported
the volume. The imported volume MUST also sit at or below the freeze height,
because freeze cannot un-execute.

## Open questions

**Q-1. Settled: `/lag_status` does fail on a frozen node, and my original model
was wrong.** Lag is a **constant**, not a growing quantity. Block sync hands off
at the freeze height (`frozenHandoff`), which cancels the scope holding
`requestRoutine` and `processPeerUpdates`. The node stops soliciting peer status
and stops pruning peers, but `s.pool` is never cleared, so
`GetMaxPeerBlockHeight()` keeps returning the tip it last observed. Lag is
therefore `tip_at_freeze − (freeze.height − 1)` forever.

For a historical freeze height that constant sits far above the default
threshold of 300, so `/lag_status` returns HTTP 417 permanently and the pod never
becomes Ready. Nothing decays, so it never recovers. FN-8 is a fix for a real
first-bootstrap outage, not a hardening.

Two corrections to the earlier draft. Strike the giga bullet: freeze and
Autobahn cannot coexist at all, because `makeNode` refuses the combination
(`sei-tendermint/node/node.go:164`), and `gigaEnabled` is exactly
`AutobahnConfigFile != ""`. One reviewer also argues that a pod restart
resets the constant to zero, because the handoff fires before any peer status
lands. That reading is plausible and rests on a sub-millisecond race. Treat the
first-bootstrap case as the proven one.

**Q-2. `/status` does not prove the node has REACHED its freeze height.** It
answers throughout the initial block sync, so a frozen node reads Ready while it
is still catching up. On a public endpoint that means serving wrong-height
answers for as long as the sync takes. The deterministic signal is an `exec`
probe comparing `/status`'s `latest_block_height` against `freeze.height - 1`.
Deferred because `/status` is already strictly better than the TCP probe it
replaced, at the same cost. **Un-defer** when a frozen node is first bootstrapped
from a height far below its freeze height, or before the endpoint becomes public.

**Q-3. Should `status` report the freeze?** An operator reading
`kubectl get seinode` cannot see that a node is frozen. FN-13's pod label covers
the alerting need. A condition remains the better operator signal, and FN-3a
removes the silent-failure state that made it urgent.

## Rejected alternative

**Raise `network.rpc.lag_threshold` by override.** The key is in the schema, so
this works today without code. I reject it as the durable answer for three
reasons.

`lag_threshold` has no value that disables the check. Zero is the strictest
setting, because any lag above zero then fails. `sei-tendermint` rejects a
negative value at `config.go:605`. The only way to pass is therefore a threshold
above every reachable lag, which means a large arbitrary number in a manifest.

It also disables lag-based readiness for that node without saying so, and it
leaves the next operator to rediscover why the number is there.

FN-8 removes the need for it.

## Traceability

| ID | Covered by |
|---|---|
| FN-1, FN-2 | `TestFreeze_OnFullNodeAndArchive_Accepted`, `TestFreeze_HeightZero_Rejected` |
| FN-3 | `TestFreeze_HeightImmutable` (raise and lower) |
| FN-3a | `TestFreeze_PresenceIsCreateOnly` (add, remove, archive, and an unrelated edit) |
| FN-4 | `TestSpecFreeze_NilForNonRPCModes`, plus the field's structural absence from the other three mode structs |
| FN-5 | `TestFreeze_HeightInOverrides_Rejected`, `TestFreeze_OtherOverrides_Accepted` |
| FN-6 | `TestFreeze_WithSnapshotGeneration_Rejected`, `TestFreeze_SnapshotGenerationWithoutFreeze_Accepted` |
| FN-7 | `TestControllerOverrides_CarryFreezeHeight`, `TestFreezeOverrides_EmitsHeight`, `TestFreezeHeightKeyResolvesInSeiConfig` |
| FN-8 | `TestReadinessProbe_FrozenNode_TargetsStatus` |
| FN-9 | `TestReadinessProbe_UnfrozenNode_KeepsLagStatus`, `TestReadinessProbe_Seed_TargetsP2PListener`, `TestControllerOverrides_OmitFreezeWhenUnfrozen`, `TestReadinessProbe_FrozenAndUnfrozen_ShareTimings` |
| FN-10 | `TestFreeze_WithHaltKeys_Rejected`, `TestFreeze_HaltKeysWithoutFreeze_Accepted` |
| FN-11 | `TestFreeze_SnapshotTargetAtOrAboveHeight_Rejected`, `TestFreeze_SnapshotTargetBelowHeight_Accepted` |
| FN-12 | `TestFreeze_WithStateSync_Rejected` |
| FN-13 | `TestResourceLabels_MarkFrozenNodes` |
| NFR-1 | Review, plus the fleet scan under D-4 |
| NFR-2 | `make manifests generate` leaves no diff in CI |
| NFR-3 | `TestFreezeHeightKeyMatchesCELGuard`, plus review of the markers |
