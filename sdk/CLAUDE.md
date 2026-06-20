# sei-sdk

Typed, provider-agnostic Go surface a CICD chaos harness programs against to
provision SeiNetwork/SeiNode and read endpoints in-process. Mirrors
`database/sql`: a provider registers in `init()`, the consumer blank-imports it,
`Open` selects the flavor by name. Re-packages the controller's already-shipped,
xreview-clean orchestration behind a Go interface — the only genuinely-new
surface is the canonical readiness probe + label constants.

## Conventions

Comments and PRs are why-not-what; names carry intent. No ticket IDs in code
(scaffolding markers excepted). Keep `go build ./... && go vet ./... && go test
./...` green (GOTOOLCHAIN=go1.26.0).

## Invariants

**Single-writer `Client`.** The SSA field-owner (`sei-sdk`) is one manager, so a
`*Client` is safe for sequential use only — not goroutine-safe across
provisioning calls. The provider is single-writer by construction.

**`Open` env precedence.** `Open(ctx, name)` owns flavor resolution: explicit
`name` -> `SEI_PROVIDER` -> presence detection (`SEI_NODE_CLUSTER` => k8s,
`SEI_LOCAL` => local; both set is an ambiguous ClassUsage error). Callers pass
`""` and let the SDK resolve — reading an env var at the call site
short-circuits the chain and drops presence detection.

**Peer-wiring namespace.** `ProvisionFleet` wires followers' peer discovery at
the genesis network's *actual* namespace (`NetworkHandle.Namespace()`), not the
provider default. Followers may provision into a different namespace than the
network; wiring to the wrong namespace strands them from the genesis validators
and surfaces as a misleading ClassTimeout.

## Source of truth

This package is the **canonical** home for the readiness probe (`provider/k8s/
probe.go`) and the label / peer-wiring constants (`labels.go`). The controller's
equivalents are `internal/`-trapped and unimportable, so the SDK authors them
once. seitask now lives in the SAME module as the SDK, so seitask->SDK
convergence (deleting its probe/label copies and calling the SDK) is a plain
in-module import — no leaf split needed. #175 (the `api/` leaf-module split) only
matters for EXTERNAL importers (seictl, harnesses): importing the SDK pulls the
full controller module graph, since `api/v1alpha1` shares the controller's root
go.mod. That heavier external import is the accepted tradeoff of landing the SDK
in an existing repo (Brandon's "existing repo" choice); #175 is the follow-up
that slims the external-consumer graph, not a prerequisite of this MVP.

## One-way doors (D1-D7, need sign-off to change; additive is safe)

- **D1 — import path** `github.com/sei-protocol/sei-k8s-controller/sdk`
  (core pkg `.../sdk/sei`, providers `.../sdk/sei/provider/...`). The SDK is an
  in-module package of the controller, not a standalone module — it ships under
  the controller's tag, not its own. Every consumer's import path; moving the
  `sdk/` subtree or renaming the controller module breaks all importers.
- **D2 — public Go API.** `Open`, `Client`, `Network`/`Fleet`,
  `NetworkSpec`/`FleetSpec`, `Endpoints`/`FleetEndpoints` (two distinct
  node-leaf types). Removing a field / changing a signature breaks every
  harness. `Open` is driver-name-only (dsn dropped, re-addable additively).
- **D3 — `provider.Provider` interface + `Register`/`Factory`.** The
  driver-registration contract a third-party or `local` provider compiles
  against. `database/sql`-shaped; locked.
- **D4 — `Class` error enum.** Consumers `switch`/`IsTimeout`/`IsFailed` on it.
  Additive-only; prefer the sentinels over the raw enum.
- **D5 — object-label keys** `sei.io/role=node`, `sei.io/seinetwork=<net>`. The
  fleet-wide selector contract shared with seictl, seitask, and chaos selectors.
  Exact values; no new keys.
- **D6 — SSA FieldOwner `sei-sdk`.** A distinct field manager from seictl /
  seitask-provision-node. Once a cluster holds `sei-sdk`-owned objects, renaming
  the manager orphans that field ownership.
- **D7 — endpoint ordering.** `[0]=aggregate, [1:]=per-pod`; per-pod order
  stable in ordinal order. WS-C nightly hard-depends on it. The typed split
  preserves it; any re-serialization (e.g. `cmd/sei` JSON) MUST keep the order.
