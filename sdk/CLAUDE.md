# sei SDK

A thin, typed, **stateless**, **multi-mode** Go-native API for SeiNetwork/SeiNode
lifecycle. Mirrors `database/sql`: a provider registers in `init()`, the consumer
blank-imports it, and `Open(ctx, mode)` selects the mode by name (or env).

The flow a harness drives: create network -> `WaitReady` -> create RPC nodes as
peers -> `WaitReady` -> run tests against the returned Go handles -> `Delete`.

## Scope — a CRUD layer, NOT an orchestrator

The SDK does clean CRUD over Go-native handles. It does **not** do cleanup, GC,
rollback, or composite orchestration — that is the caller's job. Specifically the
SDK NEVER:

- auto-deletes or cleans up on failure (the caller owns every `Delete`);
- fans out N nodes (the caller loops `CreateNode`);
- tracks provisioned resources (no registry/cache/"database" — the runtime owns
  resource state; `Get` reads it back).

## Conventions

Comments and PRs are why-not-what; names carry intent. No ticket IDs in code
(scaffolding markers excepted). Keep `go build ./sdk/...`, `go vet ./sdk/...`,
`go test ./sdk/... -race`, `golangci-lint run ./sdk/...` (0 issues), and
`gofmt -s -l sdk/` all green (GOTOOLCHAIN=go1.26.0).

## Invariants

**Stateless `Client`.** The Client and providers hold only the mode connection
(kube client, etc.) — never a cache or tracking of provisioned resources.

**Single-writer.** The k8s SSA field-owner (`sei-sdk`) is one manager, so a
`*Client` is safe for sequential use only — not goroutine-safe across calls.

**Mode selection.** `Open(ctx, mode)`: explicit `mode` wins; with `""`, exactly
one of `SEI_NODE_CLUSTER` (=> k8s), `SEI_LOCAL` (=> local), `SEI_DOCKER` (=>
docker) must be set — more than one, or none, is an error. Providers self-register
via blank import (`_ ".../sdk/sei/provider/k8s"`).

**`WaitReady` = phase + LIGHT serve-probe.** Network: phase Ready + one TM
`/status` liveness check. Node: phase Running + one serve-probe (EVM
`eth_blockNumber` if it serves EVM, else TM `/status`). A single liveness check,
NOT a `catching_up`+EVM consensus gate. Bound by the caller's `ctx` — there are no
timeout spec fields; `sei.IsTimeout(err)` reports a deadline.

**Caller owns lifecycle.** `Delete` is caller-invoked and idempotent (a not-found
is success). The SDK never auto-deletes — including on a failed `WaitReady`.

**Raw-CR escape.** `Network.Object()` / `Node.Object()` return `any`; in k8s mode
the caller type-asserts to `*v1alpha1.SeiNetwork` / `*v1alpha1.SeiNode`. Keeps the
k8s type off the mode-agnostic surface; local/docker stubs return nil.

## Source of truth

This package is the **canonical** home for the readiness serve-probe
(`provider/k8s/probe.go`) and the label / peer-wiring constants (`labels.go`).
The controller's equivalents are `internal/`-trapped and unimportable, so the SDK
authors them once.

## Modes

- **k8s** — real. SSA apply under field-owner `sei-sdk`, typed CR render, the
  canonical object labels (`sei.io/role=node`, `sei.io/seinetwork=<network>`),
  peer wiring, kubeconfig resolution (incl. SA-namespace trim), status-read
  endpoints. The node object lives at `NodeSpec.Namespace`; the peer selector
  searches `NodeSpec.NetworkNamespace` (where the genesis validators live),
  defaulting to the node's namespace when empty — the co-located common case.
- **local**, **docker** — registered STUBS. `Open` resolves them, but every verb
  returns `ErrNotImplemented` so a mode picked by env fails clearly.

## One-way doors (need sign-off to change; additive is safe)

- **Import path** `github.com/sei-protocol/sei-k8s-controller/sdk` (core pkg
  `.../sdk/sei`, providers `.../sdk/sei/provider/...`). In-module package of the
  controller — ships under the controller's tag.
- **Public Go API.** `Open`; `Client.{CreateNetwork,GetNetwork,CreateNode,
  GetNode}`; `Network`/`Node` and their methods; `NetworkSpec`/`NodeSpec`/
  `GenesisAccount`. Removing a field / changing a signature breaks every harness.
- **`provider.Provider` interface + `Register`/`Factory`.** The handle-based CRUD
  driver-registration contract.
- **Object-label keys** `sei.io/role=node`, `sei.io/seinetwork=<net>`. The
  fleet-wide selector contract shared with seictl, seitask, chaos selectors.
- **SSA FieldOwner `sei-sdk`.** A distinct field manager. Renaming it orphans
  field ownership on objects the SDK already created.
