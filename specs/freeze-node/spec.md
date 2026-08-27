# Freeze node as a first-class SeiNode configuration

| | |
|---|---|
| Status | Draft — awaiting approval on the one-way doors under Decisions requiring approval |
| Repository | `sei-protocol/sei-k8s-controller` |
| Depends on | `sei-config` v0.0.27 (`chain.freeze_height`), sei-chain `release/v6.6` |
| Author | Brandon Chatham |
| Date | 2026-08-27 |

## Semantic Anchors

EARS · RFC 2119 · CEL `XValidation` · ADR (Nygard) · compatibility law for the Kubernetes API ·
discriminated-union spec

## Problem

A frozen node runs `seid` with a freeze height. It executes blocks up to that
height, then stops, and it serves query RPC at that height indefinitely. The
platform has no way to express this.

Three routes exist to set a freeze height. None of them works today.

| Route | State |
|---|---|
| CLI flag | Closed. The controller builds a fixed argv, `{start, --home, <dataDir>}`, at `internal/noderesource/noderesource.go:940` and `:998`. No escape hatch exists. |
| CRD field | Absent. No freeze field exists on any mode sub-spec. |
| `spec.overrides` | Open since `sei-config` v0.0.27, but wrong. See the next section. |

## Why `spec.overrides` is not the answer

`spec.overrides` is a flat `map[string]string` that the controller forwards
without interpretation. Two consequences make it unfit.

**The controller cannot derive its own behaviour from it.** A frozen node needs a
different readiness probe (FN-8). The controller would have to parse an
opaque string map to know that. A typed field makes the intent a fact the
controller can act on.

**A user override silently outranks the controller.** `mergeOverrides` copies
user overrides last, at `internal/planner/planner.go:836`. An operator who sets
`chain.freeze_height` in `spec.overrides` therefore beats any value the
controller derives from a typed field. Only one source can stay writable.

## Scope

### In scope

- A `freeze` sub-spec on the `fullNode` and `archive` modes of `SeiNode`.
- CEL validation for immutability and for the two contradictions in §4.
- Controller emission of `chain.freeze_height` as a controller-owned override.
- A readiness probe that suits a node whose height never advances.

### Out of scope

- Packaging `frozen-rpc-router` as a container image. It has no image today, and
  it cannot carry WebSocket. Track separately.
- Public exposure through Waterway. That is platform work, not controller work.
- Routing across a fleet of frozen nodes at different heights.
- A `status` field or condition that reports the frozen height. See Open questions.

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

**FN-4.** The `freeze` sub-spec MUST NOT appear on the `validator`, `seed`, or
`replayer` modes.

**FN-5.** IF `spec.overrides` contains the key `chain.freeze_height`, THEN the
API server MUST reject the `SeiNode`.

**FN-6.** IF `freeze` and `snapshotGeneration` are both set on the same mode
sub-spec, THEN the API server MUST reject the `SeiNode`.

### Controller behaviour

**Independent Test** — call the planner and `noderesource` builders directly with
a frozen and an unfrozen `SeiNode`, then assert the emitted config intent and the
probe shape. This group needs no cluster.

**FN-7.** WHERE the operator sets `freeze`, the controller MUST emit
`chain.freeze_height` as a controller-owned override carrying `freeze.height`.

**FN-8.** WHERE the operator sets `freeze`, the controller MUST NOT configure
the `/lag_status` HTTP readiness probe. It MUST configure a TCP socket probe on
the RPC port instead.

**FN-9.** WHERE the operator omits `freeze`, the controller MUST configure the
readiness probe exactly as it does today. This requirement bounds the change.

### Constraints

**Independent Test** — run `make manifests generate` in CI and assert an empty
diff, and diff the served CRD schema against the previous release.

**NFR-1.** The change MUST be additive. It MUST NOT alter the type, validation,
or meaning of any existing served field. Rationale: the compatibility law for the Kubernetes API.

**NFR-2.** The author MUST regenerate `manifests/` and
`api/v1alpha1/zz_generated.deepcopy.go` with `make manifests generate`, and MUST
NOT edit either by hand.

**NFR-3.** The author MUST express every rule in FN-3, FN-5, and FN-6 as a
`+kubebuilder:validation:XValidation` marker, not in reconcile code alone.
Rationale: the API server rejects a bad spec at admission; reconcile code
cannot.

## Interface

```go
// FreezeSpec holds a node at a block height. The node executes through
// Height-1, then stops while continuing to serve query RPC. seid refuses to
// freeze a validator, so this sub-spec exists only on the non-consensus modes.
type FreezeSpec struct {
	// Height is the block height at which the node stops executing; the node
	// serves blocks through Height-1. Immutable: the node has already stopped,
	// so lowering the height cannot un-execute and raising it would resume a
	// node that is read-only by contract. Replace the node to change it.
	//
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
# SeiNodeSpec (FN-5)
!has(self.overrides) || !('chain.freeze_height' in self.overrides)

# FullNodeSpec and ArchiveSpec (FN-6)
!has(self.freeze) || !has(self.snapshotGeneration)
```

## Placement rationale

The `freeze` sub-spec belongs on both `fullNode` and `archive`.

Pruning happens at commit, and a stopped node stops pruning. A frozen full node
therefore serves only the window it had retained when it stopped. That suits a query for
state at one height. It does not suit a query for an arbitrary earlier block.

A frozen archive node retains the whole history and serves any block below the
freeze height.

Both shapes are legitimate. The mode chooses which history the node keeps.
Offering `freeze` on `fullNode` alone would give an operator the cheap shape when
they asked for the complete one.

## Success Criteria

The work closes when all of the following hold.

| # | Outcome | How to measure |
|---|---|---|
| SC-1 | An operator declares a frozen node in one manifest, with no `spec.overrides` entry and no manual edit on the volume. | The manifest in `platform` carries only `freeze.height`. |
| SC-2 | The API server rejects every invalid shape in FN-3 through FN-6. | The envtest cases for those IDs pass. |
| SC-3 | A frozen node reaches Ready and stays Ready for at least 24 hours past its freeze height. | Pod readiness on a real cluster. |
| SC-4 | The rendered `app.toml` on a frozen node carries the expected `freeze-height`. | Read the file on the running pod. |
| SC-5 | No node without `freeze` changes behaviour. | The probe test for FN-9, plus an unchanged rollout on an existing cell. |
| SC-6 | `make manifests generate` leaves no diff. | CI. |

SC-3 is the one criterion that needs a cluster. It is also the criterion that
settles Q-1.

## Decisions requiring approval

Each item below is a one-way door once an operator depends on it.

**D-1. A struct, not a bare `freezeHeight int64`.** A struct leaves room for a
retention policy or a routing hint. A bare field does not. Recommendation:
struct.

**D-2. `height` is immutable.** A frozen node cannot honour a changed height
without a rebuild. Relaxing this later is a safety regression. Recommendation:
immutable.

**D-3. Placement on `fullNode` and `archive`.** See Placement rationale. This is broader than the
original proposal of full and RPC nodes only.

**D-4. A CEL guard on `spec.overrides` rather than a change to merge order.**
Changing `mergeOverrides` precedence would alter behaviour for every existing
key. The guard is narrow and fails at admission. Recommendation: guard.

## Open questions

**Q-1. Does `/lag_status` actually fail on a frozen node?** This is not settled,
and the honest answer changes the framing of FN-8 from a fix to a hardening.

The following points hold:

- `readinessProbeForNode` at `noderesource.go:953` gives every non-seed node an
  HTTP readiness probe on `/lag_status`.
- `LagStatus` fails WHEN lag exceeds `LagThreshold`
  (`sei-tendermint/internal/rpc/core/lag_status.go:30`). The default threshold
  holds 300 (`sei-tendermint/config/config.go:559`).
- Lag derives from `GetMaxPeerBlockHeight()`, which returns 0 when the block-sync
  reactor holds no syncer.
- On the giga path the code builds the reactor with `utils.None[SyncerConfig]()`
  (`sei-tendermint/node/node.go:454`). `GetMaxPeerBlockHeight()` therefore returns 0,
  lag stays at 0, and the probe passes regardless of threshold.
- On the non-giga path the reactor receives a freeze-aware `SyncerConfig`
  (`node.go:369-379`), and the pool is never cleared.

What is not confirmed: whether the non-giga pool keeps observing rising peer
heights after the freeze stops block sync. If it does, lag grows without bound
and the pod goes NotReady about 30 seconds after the threshold trips. If peer
tracking stops with the sync routines, lag stays near zero and the probe passes.

FN-8 is worth implementing either way. It makes readiness deterministic instead
of dependent on giga mode and on pool teardown internals. Do not describe it as a fix for a
proven outage.

**Q-2. Should `status` report the freeze?** An operator reading
`kubectl get seinode` cannot see that a node is frozen. Conditions in this repo
MUST be always-present, so adding one is its own contract addition. Not designed
in.

## Rejected alternative

**Raise `network.rpc.lag_threshold` by override.** The key is in the schema, so
this works today without code. I reject it as the durable answer for three reasons.

`lag_threshold` has no value that disables the check. Zero is the strictest
setting, because any lag above zero then fails. `sei-tendermint` rejects a negative
value at `config.go:605`. The only way to pass is therefore a
threshold above every reachable lag, which means a large arbitrary number in a
manifest.

It also disables lag-based readiness for that node without saying so, and it
leaves the next operator to rediscover why the number is there.

FN-8 removes the need for it.

## Traceability

| ID | Covered by |
|---|---|
| FN-1, FN-2, FN-4 | CRD schema test asserting the field exists on the two modes and is absent on the other three |
| FN-3 | envtest update rejecting a changed `freeze.height` |
| FN-5 | envtest rejecting `chain.freeze_height` in `spec.overrides` |
| FN-6 | envtest rejecting `freeze` with `snapshotGeneration` |
| FN-7 | planner test asserting the emitted config intent carries `chain.freeze_height` |
| FN-8, FN-9 | `noderesource` test asserting probe shape with and without `freeze` |
| NFR-1 | Review, plus the existing CRD compatibility tests |
| NFR-2 | `make manifests generate` leaves no diff in CI |
| NFR-3 | Review of the `XValidation` markers |
