# Feature Specification: A typed consensus engine on the SeiNetwork, and a health source that follows the engine

**Feature Branch**: `008-autobahn-evm-only-consensus`

**Created**: 2026-09-11

**Status**: Draft

**Unblocks**: an Autobahn or EVM-only benchmark network declared in Git. The
`sei-chain` Autobahn README deploys a four-validator Autobahn network with
`docker compose`; the controller cannot express the same network today, for one
reason with three faces. `autobahn.json` is a ceremony artifact — `seid tendermint
gen-autobahn-config` reads every validator's public keys and addresses at once and
writes one file that every node then loads — and the controller's config substrate
(`spec.configValues`, specs 002 and 003) admits TOML files only, so no field can
carry it. EVM-only mode then removes the CometBFT RPC on `26657` that the
controller's readiness probe and `restart-seid` up-check both read,
so an EVM-only pod would never pass its probes even with the file in place. And
the README's block gas limit lives at `consensus_params.block.max_gas`, a
top-level genesis key that `spec.genesis.overrides` — an `app_state` patch —
cannot reach.

**Input**: Linear PLT-1249 and the `sei-chain` `integration_test/autobahn/README.md`
at the `sei-load v0.0.1` pin.

This spec adds a typed, create-only consensus field rather than a generic
non-TOML file substrate, and it makes the health source a function of the
node's engine and mode rather than a constant.

## Semantic Anchors

This spec names each anchor once. The body below does not restate it. Each row
states what the anchor does not reach, because that gap is the honest part.

| Anchor | Governs | Does not cover |
|---|---|---|
| EARS | acceptance criteria syntax | whether a criterion is the right one |
| RFC 2119 | normative keywords, uppercase | whether the obligation is correct |
| INVEST | whether each story is a real slice | whether the slice delivers value |
| Kubernetes API conventions | CRD field shape, defaults, status, create-only via CEL | whether the controller reconciles correctly |
| Google AIP | an enum over a boolean for the engine | whether the resource model is the right one |
| `sei-chain` Autobahn README | the values an Autobahn network needs | whether those values suit a given benchmark |

## Glossary

- **Controller**: the sei-k8s-controller reconciler that renders a node's pod, plans its tasks, and writes status.
- **Sidecar**: the per-pod `sei-sidecar` that runs the controller's tasks against the node's home directory.
- **Operator**: a person who declares a benchmark network in Git and reads its status.
- **SeiNetwork**: the CRD that owns a validator pool and runs the genesis ceremony.
- **SeiNode**: the CRD for a single node. A validator child is a SeiNode; a follower is a SeiNode an operator declares against an existing chain.
- **Engine**: the consensus engine `seid` runs. One of `Tendermint` (the default, CometBFT) or `Autobahn` (GigaRouter).
- **EVM-only**: an Autobahn mode in which `seid` replaces the Cosmos application with the disk-backed EVM-only executor, hard-codes EVM chain ID `713715`, serves a JSON-RPC subset on `8545`, and opens no CometBFT RPC.
- **Consensus field**: the typed, optional, create-only `spec.consensus` this spec adds to both CRDs, which carries the engine and the EVM-only flag.
- **Autobahn artifact**: the `autobahn.json` file `gen-autobahn-config` writes. It lists every validator's validator key, node key, P2P address, and EVM-RPC URL, plus the block-interval and transaction-limit settings.
- **Ceremony**: the genesis assembly the SeiNetwork already runs — each validator uploads its gentx and identity, one assembler collects them, writes `genesis.json`, and every node downloads it.
- **Identity manifest**: the per-validator JSON the `upload-genesis-artifacts` task uploads today. It carries the node key; this spec widens it.
- **Health source**: the endpoint the controller treats as proof that `seid` is up. Today it is one of the P2P TCP port, `/status`, or `/lag_status` on `26657`.
- **Up-check**: the sidecar's poll that says seid answers — the post-restart wait in `restart-seid` and the honesty check in `restart-seid` and `stop-seid`. Today it polls `/status`.
- **Engine keys**: the two top-level `config.toml` keys `seid` reads for this feature — `autobahn-config-file` and `evm-only`.
- **Consensus params**: the top-level `consensus_params` object in `genesis.json`, distinct from `app_state`.

## Boundary Context

- **Sits within**: the SeiNetwork genesis ceremony, the SeiNode config overlay, the pod probes the controller renders, and the sidecar's `restart-seid` task.
- **Owns**: the consensus field on both CRDs, the Autobahn artifact's generation and distribution, the engine keys the controller writes, the health-source selection, and a typed path to top-level consensus params.
- **Does not own**: the Autobahn protocol, `gen-autobahn-config`, or the EVM-only RPC surface. `sei-chain` owns those; this spec consumes them as they are at the pinned README.
- **Does not own**: ordinary Autobahn tuning that `seid` reads from TOML — `giga_executor`, `state-store`, `state-commit`, `receipt-store`, `evm` and `api` toggles, `timeout_commit`. `spec.configValues` (specs 002 and 003) already carries those.
- **Does not own**: `seid start` command-line flags (`--inv-check-period`, `--freeze-height`). PLT-1250 owns those.
- **Does not own**: the `Producing` condition and the height a Ready network must reach. Spec 007 and PLT-1251 own those.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An operator declares an Autobahn validator pool (Priority: P1)

An operator sets the engine to `Autobahn` on a four-replica SeiNetwork, alongside
the Giga `configValues` the README lists, and commits it. The ceremony produces
`genesis.json` and the Autobahn artifact together, every validator loads both,
and the network reaches `Ready` with no manual step.

**Why this priority**: this story is the ticket. Without it every other story has
nothing to probe or tune.

**Independent Test**: Apply a SeiNetwork with `spec.consensus.engine: Autobahn`.
Confirm each validator's `config.toml` names the artifact, each validator's home
holds the same artifact with four validators in it, and the network reports
`Ready`.

**Acceptance Scenarios**:

1. **Given** a SeiNetwork with the engine set to `Autobahn`, **When** the ceremony completes, **Then** every validator's home directory holds an identical `autobahn.json` listing all `replicas` validators.
2. **Given** that network, **When** a validator pod starts, **Then** its `config.toml` carries `autobahn-config-file` pointing at that file and `evm-only = false`.
3. **Given** that network, **When** the operator later edits the engine, **Then** the API server rejects the edit.

### User Story 2 - An operator declares an EVM-only pool and the probes still work (Priority: P1)

The operator sets `evmOnly: true` on the Autobahn network. Each validator starts
with the EVM-only executor, opens `8545` and no `26657`, and still passes its
readiness probe. A `configValues` edit still completes its
`restart-seid`.

**Why this priority**: this ranks with Story 1. The README's benchmark is the
EVM-only one, and a pod the probes cannot pass is a pod the controller never
reports Ready.

**Independent Test**: Apply the network with `evmOnly: true`. Confirm every
validator pod passes readiness, `kubectl exec` shows no listener on `26657` and
a listener on `8545`, and a `configValues` edit returns every validator to Ready
without a `restart-seid` timeout.

**Acceptance Scenarios**:

1. **Given** an EVM-only validator, **When** `seid` opens its EVM-only RPC listener, **Then** the pod's readiness probe passes without any request to `26657`.
2. **Given** an EVM-only validator, **When** the operator changes a `configValues` entry, **Then** `restart-seid` completes and the node returns to Ready.
3. **Given** an EVM-only validator, **When** an operator reads its `config.toml` and `app.toml`, **Then** `evm-only = true`, the `[rpc]` listener is empty, and the `[api]`, `[grpc]`, and `[grpc-web]` servers are disabled.

### User Story 3 - A follower joins the Autobahn chain by declaration (Priority: P2)

The operator declares a follower SeiNode against the Autobahn chain the same way
they declare one against a Tendermint chain today. The follower fetches the
Autobahn artifact alongside `genesis.json` and joins as a full-node participant.

**Why this priority**: this ranks below the pool. The README runs its load against
validators; the follower is the observer an operator adds once the pool works.

**Independent Test**: Apply a SeiNode with `spec.consensus.engine: Autobahn` and
the pool's `chainId`. Confirm it downloads the artifact, its `config.toml` names
it, and the node syncs.

**Acceptance Scenarios**:

1. **Given** a Ready Autobahn network, **When** an operator applies a follower SeiNode with the same `chainId` and engine, **Then** the follower's home holds the same artifact the validators hold.
2. **Given** a follower whose consensus field disagrees with the chain's artifact presence, **When** the sidecar configures genesis, **Then** the task fails with an error that names the mismatch.

### User Story 4 - The operator sets the block gas limit in the manifest (Priority: P2)

The operator sets `consensus_params.block.max_gas` to `35000000` on the SeiNetwork
and the assembled `genesis.json` carries it, with no `kubectl exec` patch.

**Why this priority**: this ranks with Story 3. The network runs without it; the
README's numbers do not reproduce without it.

**Independent Test**: Set the field, run the ceremony, read
`.consensus_params.block.max_gas` from a validator's `genesis.json`.

**Acceptance Scenarios**:

1. **Given** a SeiNetwork with a block gas limit in the genesis spec, **When** the ceremony assembles `genesis.json`, **Then** `.consensus_params.block.max_gas` equals the declared value.
2. **Given** a SeiNetwork with no consensus params declared, **When** the ceremony assembles `genesis.json`, **Then** `consensus_params` is what `seid init` wrote.

### Edge Cases

- A SeiNetwork with `evmOnly: true` and engine `Tendermint` or unset. The API server rejects it; EVM-only exists only under Autobahn.
- A seed SeiNode with `evmOnly: true`. `seid` rejects this at startup (`errEVMOnlySeed`); the API server rejects it first.
- An Autobahn ceremony where one validator's identity manifest lacks a validator key. The assembler fails and names the validator; it does not write a partial artifact.
- A node whose `spec.overrides` or `spec.configOverrides` names an engine key, or the `[rpc]` listener on an EVM-only node. The API server rejects the object at admission, because the existing merge lets user overrides win and a silent override would leave the artifact and the config disagreeing.
- A node whose `spec.configValues` names an engine key. The controller applies it — spec 002 criterion 7 and spec 003 forbid a key guard on that path — and records a Warning event naming the entry. The operator owns the outcome, as they already do for a config value that collides with the freeze height.
- A `replicas` count other than four. The engine imposes no count; the README's four is a choice, not a limit.
- An Autobahn network with empty blocks disabled and no load. Height stays at 0. `Ready` here means the health source answers, not that blocks flow; spec 007's `Producing` carries the block signal and PLT-1251 decides the idle-chain rule.

## Requirements *(mandatory)*

Keywords follow RFC 2119. Criteria follow EARS.

### Requirement 1: A typed, create-only consensus field on both CRDs

**Objective:** As an operator, I want one validated field that names the engine
and the EVM-only mode, so that a manifest expresses an Autobahn network without
producing a ceremony artifact by hand.

**Traces to:** User Story 1, User Story 2, User Story 3

#### Acceptance Criteria

1. The SeiNetwork and the SeiNode SHALL each carry an optional `spec.consensus` object with `engine` (enumeration `Tendermint`, `Autobahn`) and `evmOnly` (boolean, default `false`), sharing one named Go type.
2. When `spec.consensus` is absent, the controller SHALL behave as engine `Tendermint`, which is the behaviour every existing object has today.
3. When an update changes the effective engine or the effective `evmOnly` on a SeiNetwork or a SeiNode, the API server SHALL reject it with a message stating that the engine is baked into the ceremony's artifact and the node's home directory. The effective value is `Tendermint` and `false` when the field is absent, so adding an explicit `{engine: Tendermint}` to an existing object is an accepted no-op, and the rule uses the `has()`-guarded effective-value comparison the freeze height already uses, not the bare equality a required field can afford.
4. When `evmOnly` is `true` and `engine` is not `Autobahn`, the API server SHALL reject the object.
5. When `evmOnly` is `true` on a SeiNode whose mode is seed, the API server SHALL reject the object.
6. The SeiNetwork SHALL propagate `spec.consensus` to every validator child through the existing child-sync path.

### Requirement 2: The ceremony produces and distributes the Autobahn artifact

**Objective:** As an operator, I want the controller to run `gen-autobahn-config`
where it already holds every validator's identity, so that the artifact is never
a manual step.

**Traces to:** User Story 1, User Story 3

#### Acceptance Criteria

1. When the engine is `Autobahn`, the `upload-genesis-artifacts` task SHALL widen the identity manifest with the validator public key, the node public key, the P2P address `<node>-0.<node>.<namespace>.svc.cluster.local:26656`, and the EVM-RPC URL `http://<node>-0.<node>.<namespace>.svc.cluster.local:8545`.
2. When the engine is `Autobahn`, the assembler SHALL materialise one node directory per validator from the identity manifests, run `seid tendermint gen-autobahn-config <dirs> --output autobahn.json`, and upload the result to `{bucket}/{chainID}/autobahn.json` **before** it uploads `genesis.json`. `genesis.json` is the ceremony's commit point that every `configure-genesis` polls for, so the artifact is present whenever the commit point is.
3. When any identity manifest lacks a field the generator reads, the assembler SHALL fail before writing either artifact, naming the validator and the field.
4. When the engine is `Autobahn`, the `configure-genesis` task SHALL download `autobahn.json` to `config/autobahn.json` after `genesis.json`, on validators and followers alike. An absent artifact after a present `genesis.json` is a broken ceremony, not a race, and the task SHALL fail terminally naming the object key, so the plan fails on that attempt rather than at the end of the `configure-genesis` retry budget.
5. The assembler SHALL invoke the generator with its defaults, so the artifact carries `max_txs_per_block` 2000, `allow_empty_blocks` false, `block_interval` 400ms, `persistent_state_dir` `data/autobahn`, and BlockDB retention 30s. Exposing these as fields is deferred.

### Requirement 3: The controller writes the engine keys

**Objective:** As an operator, I want the controller to set the two keys `seid`
reads for the engine, so that I never hand-edit `config.toml` and cannot set
them inconsistently with the artifact.

**Traces to:** User Story 1, User Story 2, User Story 3

#### Acceptance Criteria

1. When the engine is `Autobahn`, the controller SHALL write `autobahn-config-file = "<home>/config/autobahn.json"` as a top-level `config.toml` key through the controller-owned config path, beside the peers and freeze-height overrides it already owns. That path is `sei-config` enrichment, so `sei-config` SHALL gain keys for `autobahn-config-file` and `evm-only`, and the controller SHALL pin that release.
2. When `evmOnly` is `true`, the controller SHALL write `evm-only = true` in `config.toml`, an empty `[rpc] laddr`, and `enable = false` under `[api]`, `[grpc]`, and `[grpc-web]` in `app.toml`.
3. When `evmOnly` is `true`, the controller SHALL NOT attach the `cosmos-exporter` container, which waits on seid's gRPC port and is killed by its own liveness probe when that port never binds — the same carve-out a seed has today.
4. When an operator's `spec.overrides` or `spec.configOverrides` names an engine key, or names `rpc.laddr` on an EVM-only node, the API server SHALL reject the object at admission, following the `chain.freeze_height` denial that already guards a controller-owned key on those substrates. A status condition is not enough here: user overrides outrank controller-derived ones in the merge, and `ConfigValuesValid=False` leaves children on their last good set rather than rejecting the write. Both maps are spelled in `sei-config` keys — `spec.configOverrides` is cloned verbatim into each child's `spec.overrides` — so one denylist serves both. It covers `network.rpc.listen_address` on an EVM-only node from the start, because `sei-config` exposes that key today. It covers the engine keys from the release in which `sei-config` exposes them, which this spec requires because the controller writes the engine keys through that same enrichment path (Requirement 3 item 1); until that release neither map can spell an engine key, so neither can set one inconsistently with the artifact.
5. When an operator's `spec.configValues` names an engine key or, on an EVM-only node, `rpc.laddr`, the controller SHALL apply it as spec 002 criterion 7 and spec 003 require — that path carries no key guard, and this spec does not amend them — and SHALL record a Warning event on the object, on the transition into that state only, naming the file, key, the engine-derived value it displaced, and that the pod's probes, exporter selection, and up-check keep following `spec.consensus`, so a node whose `seid` runs the displaced mode does not reach Ready. The operator owns the result, the trade-off spec 002 already records for the freeze height.
6. When the engine is `Tendermint`, the controller SHALL write no engine key and attach the exporter as today, so an existing node's rendered pod is unchanged by this spec.

### Requirement 4: The health source follows the engine and mode

**Objective:** As an operator, I want a node's readiness probe and the sidecar's
`restart-seid` and `stop-seid` up-checks to read one endpoint chosen by the
node's mode, so that an EVM-only node is Ready when its RPC answers and a
restart completes against the same endpoint the probe trusts.

**Traces to:** User Story 2

#### Acceptance Criteria

1. The controller SHALL select the up-check by one function of the SeiNode spec, in this order: seed → TCP on `26656`; `evmOnly` → HTTP GET `/` on `8545`; otherwise HTTP GET `/status` on `26657`.
2. The controller SHALL render the readiness probe from that up-check, with one refinement: an unfrozen node that serves CometBFT RPC probes `/lag_status` on the up-check's port instead of `/status`, as today. The startup probe on the seid container SHALL stay on the sidecar's `/v0/healthz` gate, which parks seid during a workflow hold; it reads no seid listener and needs no change.
3. The `restart-seid` and `stop-seid` tasks SHALL take the up-check as a task parameter the planner fills from the same function, and SHALL poll it in place of the fixed `/status` — `restart-seid` for the post-restart wait, both for the honesty check that refuses to report a stop when `/proc` shows nothing but the listener still answers. The sidecar SHALL validate the parameter (scheme `tcp` or `http`, port in range, path only for `http`) and dial loopback only. When the parameter is absent the sidecar SHALL keep today's `/status` poll, so a controller that predates this spec still drives the sidecar; a sidecar image that predates it ignores the parameter and polls `/status`, so the sidecar image lands before the controller, the deploy order the repository already requires.
4. When the health source is the EVM-only listener, the controller SHALL document in the CRD field description that Ready proves the listener answers, not sync distance, as `/status` already does for a frozen node.
5. When the engine is `Tendermint` and `evmOnly` is `false`, every rendered probe SHALL equal today's, and every up-check on an RPC-serving node SHALL equal today's. The one existing node this requirement changes is the seed: today its up-check polls `/status` on `26657`, which a seed never binds, so its honesty check always reads down and a `stop-seid` against an invisible seid reports a no-op success; item 1 moves it to TCP `26656`, which answers, and that is a fix this spec owns.

### Requirement 5: A typed path to top-level consensus params

**Objective:** As an operator, I want to set `consensus_params` in the manifest,
so that the README's block gas limit is a declared value and not a patch.

**Traces to:** User Story 4

#### Acceptance Criteria

1. The SeiNetwork SHALL carry an optional `spec.genesis.consensusParams` field holding one nested JSON object shaped like `consensus_params` itself (`{"block": {"max_gas": "35000000"}}`), not the flat dotted-path map `spec.genesis.overrides` uses. It shares `spec.genesis`'s create-only rule because it lives inside `spec.genesis`.
2. When `consensusParams` is set, the assembler SHALL deep-merge that object into the top-level `consensus_params` of the assembled `genesis.json` after `app_state` overrides, with the same `null`-rejection the overrides carry.
3. When `consensusParams` is absent, the assembler SHALL leave `consensus_params` as `seid init` wrote it.

### Key Entities

- **`ConsensusSpec`**: `{engine: Tendermint|Autobahn, evmOnly: bool}`. One Go type in the shared API file, the `DeletionPolicy` and `NodeIsolation` precedent. Create-only on both Kinds by an effective-value CEL rule.
- **Autobahn artifact**: `autobahn.json` at `{bucket}/{chainID}/autobahn.json`, the same prefix as `genesis.json`. Written once by the assembler; read by every node's `configure-genesis`.
- **Identity manifest**: `{bucket}/{chainID}/nodes/<node>/identity.json`, widened with `validator_pubkey`, `node_pubkey`, `autobahn_address`, `evmrpc_url`.
- **Up-check**: `wire.UpCheck{scheme: tcp|http, port, path}` chosen by `noderesource.UpCheckForNode(node)`, consumed by the readiness-probe renderer and carried as the `upCheck` parameter of `restart-seid` and `stop-seid`. The sidecar validates it and dials loopback only, so a SeiNodeTask author can at most point the wait at a different local port.
- **`spec.genesis.consensusParams`**: one nested JSON object, merged into top-level `consensus_params`.

## Success Criteria *(mandatory)*

- **SC-001**: A SeiNetwork with engine `Autobahn` produces one `autobahn.json` in S3 listing every validator, and every validator and follower downloads it.
  *Verifier:* judgement — a platform engineer runs the network on the harbor dev cluster, reads `{bucket}/{chainID}/autobahn.json`, and compares it against `config/autobahn.json` in each pod.
- **SC-002**: The API server rejects an engine edit on a SeiNetwork, `evmOnly` without `Autobahn`, and `evmOnly` on a seed.
  *Verifier:* judgement — a platform engineer applies each of the three manifests against the installed CRD and records the rejection message.
- **SC-003**: The rendered `config.toml` and `app.toml` of an EVM-only validator match Requirement 3 item 2, its pod carries no `cosmos-exporter` container, a `spec.configOverrides` entry naming `network.rpc.listen_address` is rejected at admission, and a `spec.configValues` entry naming an engine key is applied and produces one Warning event.
  *Verifier:* judgement — a platform engineer reads the files from a pod, applies the conflicting `configOverrides` entry, then the conflicting `configValues` entry.
- **SC-004**: `UpCheckForNode` returns today's listener for every existing fixture and the `8545` source for an EVM-only fixture, and the readiness-probe renderer and the `restart-seid` parameter both call it.
  *Verifier:* judgement — a platform engineer runs the `internal/noderesource` probe tests and the `sidecar/tasks` restart tests after the change and confirms no existing expectation moved.
- **SC-005**: An EVM-only validator pool reaches Ready and survives a `configValues` edit without a `restart-seid` timeout.
  *Verifier:* judgement — a platform engineer runs the README's four-validator EVM-only topology on harbor, edits one `configValues` entry, and confirms every validator returns to Ready.
- **SC-006**: A declared `consensusParams.block.max_gas` appears in the assembled `genesis.json`.
  *Verifier:* judgement — a platform engineer reads `.consensus_params.block.max_gas` from a validator's `genesis.json`.
- **SC-007**: A `sei-load` EVM-only profile against the pool's `8545` endpoints submits transactions and reads receipts.
  *Verifier:* not built — this needs the harbor `sei-load` Job and PLT-1251's idle-chain rule to land; until then a platform engineer runs the README's `sei-load` step by hand.

## Assumptions

- The engine is an enumeration, not a boolean, because a third engine or mode is plausible and the AIP guidance prefers a name over a flag. `evmOnly` stays a boolean because it is a mode of one engine, not a peer of it.
- The consensus field is create-only on both Kinds. On the SeiNetwork the artifact and the executor are baked at the ceremony; on a SeiNode `configure-genesis` runs once and drift detection is image- and config-value-driven, so a later edit would be inert or leave `config.toml` naming an artifact the home does not hold. The field is optional, so the rule compares effective values under `has()` guards, the freeze-height shape, rather than the bare `self.x == oldSelf.x` a required field like `spec.genesis` can use; a bare equality on an absent optional field errors and would reject every update to every existing object.
- The Autobahn artifact rides the S3 genesis prefix rather than a ConfigMap. The ceremony already publishes `genesis.json` there and every node's `configure-genesis` already reads it by `chainId`, so a follower needs no reference to a ConfigMap in another namespace and no new RBAC. A `SeiNode.spec.autobahnConfigRef` is not needed: the follower's `spec.consensus.engine` is the declaration that it should fetch.
- The `docker/rpcnode` script in `sei-chain` regenerates the artifact on the follower from the validator directories. On Harbor the follower downloads the assembler's copy instead, because a follower never holds the validators' identity material and the generator's output is deterministic for the same inputs.
- The generator's `address` field accepts a hostname; `tcp.ParseHostPort` splits host and port and resolves the hostname at dial time. The per-pod Service DNS the peer-collection task already uses therefore serves as the Autobahn address.
- The identity manifest currently carries `node_key.json` whole. Widening it with the two public keys and two addresses is additive; the assembler ignores unknown fields on a Tendermint network.
- The generator reads public material only: `validator_pubkey.txt`, `node_pubkey.txt`, `autobahn_address.txt`, and `evmrpc_url.txt` per node directory, per `gen_autobahn_config.go` in `sei-tendermint`. The assembler can therefore synthesise those directories from the identity manifests without any private key leaving its validator.
- The validator public key text (`validator:<base64>`) and node public key text the generator reads are derivable from `priv_validator_key.json` and `node_key.json`; the sidecar computes them, or invokes `seid` to print them, at upload time. Which of the two is an implementation choice, not an API one.
- The controller's own overrides travel as `sei-config` enrichment keys (`network.p2p.persistent_peers`, `chain.freeze_height`). `sei-config` exposes no key for `autobahn-config-file` or `evm-only` today; Requirement 3 item 1 adds them there rather than using the raw-TOML `config-patch` path, because only a `sei-config` key can be denied on the override maps (item 4).
- EVM-only leaves `[rpc] laddr` empty and disables `api`, `grpc`, and `grpc-web`, following the `docker/localnode` `step4_config_override.sh` sequence in `sei-chain`. Non-EVM-only Autobahn keeps the CometBFT RPC, as that script does, so its health source is unchanged.
- The EVM-only listener returns `200` to a bare GET on `/` with no body: the `go-ethereum` `rpc.Server.ServeHTTP` health-check path, in the fork `sei-chain` pins. An `httpGet` probe therefore needs no `exec`, no POST, and no `sei-chain` change. The listener serves only `eth_sendRawTransaction` and `eth_getTransactionReceipt`, so no `eth_blockNumber` probe is available; that bounds what Ready can mean here.
- The EVM chain ID `713715` is a compile-time constant in `sei-chain` for EVM-only. `spec.genesis.chainId` still names the Cosmos chain and still keys the S3 prefix; the two do not conflict.
- The `restart-seid` process match on `seid start` is unchanged; EVM-only launches through the same subcommand with the engine keys in `config.toml`, not extra flags.
- The README starts `seid` with `--inv-check-period 0 --freeze-height 0`. Whether the controller's defaults are equivalent is not established here; PLT-1250 owns the answer, and a difference there does not block this spec.
- Autobahn disables empty blocks by default, so an idle network stays at height 0. Spec 007's `Producing` needs a height source the EVM-only node does not serve on `26657`; PLT-1251 decides the rule, and this spec keeps Ready as a listener-up signal for EVM-only until it does.
- `consensusParams` is a nested JSON object merged into one known top-level key rather than a widening of `overrides` to arbitrary top-level keys, so `overrides` keeps its `app_state` meaning and neither field can reach `validators` or `initial_height`.
- `status.internalService.ports` advertises `Rpc` `26657` and `Rest` `1317` on every SeiNetwork today, the per-pod status advertises `EvmWs` `8546`, and the Service objects carry the same ports; on an EVM-only network none of those bind, and the EVM-only JSON-RPC subset serves no WebSocket. The typed port fields are part of the public status contract, so this spec leaves them as they are and documents on the field that an EVM-only network serves only `EvmHttp`; a typed way to advertise an absent port is deferred.

## Out of scope

- Exposing the generator's tuning (`max_txs_per_block`, `block_interval`, `allow_empty_blocks`, BlockDB retention) as fields. Deferred until a benchmark needs a value the default does not give.
- `seid start` flags. PLT-1250.
- The `Producing` condition and the idle-chain Ready rule. Spec 007 and PLT-1251.
- A generic non-TOML file substrate. The typed field keeps the ceremony coupling inside the controller and preserves the TOML-only guarantee of `configValues`.
- `seictl` flags and the `harbor-dev` recipe for these fields. They follow this spec's landing.
