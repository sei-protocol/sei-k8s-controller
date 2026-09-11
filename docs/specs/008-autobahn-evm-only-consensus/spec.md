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
controller's startup probe, readiness probe, and `restart-seid` up-check all read,
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
- **Up-check**: the sidecar's post-restart poll in `restart-seid` that declares the restart complete. Today it polls `/status`.
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
startup and readiness probes. A `configValues` edit still completes its
`restart-seid`.

**Why this priority**: this ranks with Story 1. The README's benchmark is the
EVM-only one, and a pod the probes cannot pass is a pod the controller never
reports Ready.

**Independent Test**: Apply the network with `evmOnly: true`. Confirm every
validator pod passes readiness, `kubectl exec` shows no listener on `26657` and
a listener on `8545`, and a `configValues` edit returns every validator to Ready
without a `restart-seid` timeout.

**Acceptance Scenarios**:

1. **Given** an EVM-only validator, **When** `seid` opens its EVM-only RPC listener, **Then** the pod's startup and readiness probes pass without any request to `26657`.
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
- An EVM-only node with a `spec.configValues` entry that names an engine key or the `[rpc]` listener. The controller rejects the entry through `ConfigValuesValid` rather than racing it, because the existing merge lets user overrides win and a silent override would leave the artifact and the config disagreeing.
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
3. When an update changes `spec.consensus` on a SeiNetwork, the API server SHALL reject it with a message stating that the engine is baked into the ceremony's artifact.
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
2. When the engine is `Autobahn`, the assembler SHALL, after writing `genesis.json`, materialise one node directory per validator from the identity manifests, run `seid tendermint gen-autobahn-config <dirs> --output autobahn.json`, and upload the result to `{bucket}/{chainID}/autobahn.json` beside `genesis.json`.
3. When any identity manifest lacks a field the generator reads, the assembler SHALL fail before writing the artifact, naming the validator and the field.
4. When the engine is `Autobahn`, the `configure-genesis` task SHALL download `autobahn.json` to `config/autobahn.json` after `genesis.json`, on validators and followers alike, and SHALL fail when the object is absent.
5. The assembler SHALL invoke the generator with its defaults, so the artifact carries `max_txs_per_block` 2000, `allow_empty_blocks` false, `block_interval` 400ms, `persistent_state_dir` `data/autobahn`, and BlockDB retention 30s. Exposing these as fields is deferred.

### Requirement 3: The controller writes the engine keys

**Objective:** As an operator, I want the controller to set the two keys `seid`
reads for the engine, so that I never hand-edit `config.toml` and cannot set
them inconsistently with the artifact.

**Traces to:** User Story 1, User Story 2, User Story 3

#### Acceptance Criteria

1. When the engine is `Autobahn`, the controller SHALL write `autobahn-config-file = "<home>/config/autobahn.json"` as a top-level `config.toml` key through the controller-owned config path, beside `persistent_peers` and `freeze_height`.
2. When `evmOnly` is `true`, the controller SHALL write `evm-only = true` in `config.toml`, an empty `[rpc] laddr`, and `enable = false` under `[api]`, `[grpc]`, and `[grpc-web]` in `app.toml`.
3. When an operator's `spec.configValues` entry names an engine key, or names `rpc.laddr` on an EVM-only node, the controller SHALL report `ConfigValuesValid` false with a reason naming the key, so the overlay and the engine cannot disagree.
4. When the engine is `Tendermint`, the controller SHALL write no engine key, so an existing node's rendered config is unchanged by this spec.

### Requirement 4: The health source follows the engine and mode

**Objective:** As an operator, I want a node's startup probe, readiness probe, and
`restart-seid` up-check to read one endpoint chosen by the node's mode, so that
an EVM-only node is Ready when its RPC answers and a restart completes against
the same endpoint the probe trusts.

**Traces to:** User Story 2

#### Acceptance Criteria

1. The controller SHALL select the health source by one function of the SeiNode spec, in this order: seed → TCP on `26656`; `evmOnly` → HTTP GET `/` on `8545`; frozen → HTTP GET `/status` on `26657`; otherwise HTTP GET `/lag_status` on `26657`.
2. The controller SHALL render the startup probe and the readiness probe from that function; today's genesis-mode startup probe on TCP `26657` SHALL move to the same source, because an EVM-only node never opens `26657`.
3. The `restart-seid` task SHALL take the health source as a task parameter the planner fills from the same function, and SHALL poll it for the up-check in place of the fixed `/status`.
4. When the health source is the EVM-only listener, the controller SHALL document in the CRD field description that Ready proves the listener answers, not sync distance, as `/status` already does for a frozen node.
5. When the engine is `Tendermint` and `evmOnly` is `false`, every rendered probe and up-check SHALL equal today's, so this requirement changes no existing node.

### Requirement 5: A typed path to top-level consensus params

**Objective:** As an operator, I want to set `consensus_params` in the manifest,
so that the README's block gas limit is a declared value and not a patch.

**Traces to:** User Story 4

#### Acceptance Criteria

1. The SeiNetwork SHALL carry an optional `spec.genesis.consensusParams` object of JSON values, sharing `spec.genesis`'s create-only rule.
2. When `consensusParams` is set, the assembler SHALL deep-merge it into the top-level `consensus_params` of the assembled `genesis.json` after `app_state` overrides, with the same `null`-rejection the overrides carry.
3. When `consensusParams` is absent, the assembler SHALL leave `consensus_params` as `seid init` wrote it.

### Key Entities

- **`ConsensusSpec`**: `{engine: Tendermint|Autobahn, evmOnly: bool}`. One Go type in the shared API file, the `DeletionPolicy` and `NodeIsolation` precedent. Create-only on the SeiNetwork by CEL.
- **Autobahn artifact**: `autobahn.json` at `{bucket}/{chainID}/autobahn.json`, the same prefix as `genesis.json`. Written once by the assembler; read by every node's `configure-genesis`.
- **Identity manifest**: `{bucket}/{chainID}/nodes/<node>/identity.json`, widened with `validator_pubkey`, `node_pubkey`, `autobahn_address`, `evmrpc_url`.
- **Health source**: `(scheme, port, path)` chosen by `healthSourceForNode(node)`, consumed by the probe renderer and the `restart-seid` parameter.
- **`spec.genesis.consensusParams`**: `map[string]JSON`, merged into top-level `consensus_params`.

## Success Criteria *(mandatory)*

- **SC-001**: A SeiNetwork with engine `Autobahn` produces one `autobahn.json` in S3 listing every validator, and every validator and follower downloads it.
  *Verifier:* judgement — a platform engineer runs the network on the harbor dev cluster, reads `{bucket}/{chainID}/autobahn.json`, and compares it against `config/autobahn.json` in each pod.
- **SC-002**: The API server rejects an engine edit on a SeiNetwork, `evmOnly` without `Autobahn`, and `evmOnly` on a seed.
  *Verifier:* judgement — a platform engineer applies each of the three manifests against the installed CRD and records the rejection message.
- **SC-003**: The rendered `config.toml` and `app.toml` of an EVM-only validator match Requirement 3 item 2, and a `configValues` entry naming an engine key sets `ConfigValuesValid` false.
  *Verifier:* judgement — a platform engineer reads the files from a pod and applies the conflicting `configValues` entry.
- **SC-004**: `healthSourceForNode` returns today's probe for every existing fixture and the `8545` source for an EVM-only fixture, and the probe renderer and the `restart-seid` parameter both call it.
  *Verifier:* judgement — a platform engineer runs the `internal/noderesource` probe tests and the `sidecar/tasks` restart tests after the change and confirms no existing expectation moved.
- **SC-005**: An EVM-only validator pool reaches Ready and survives a `configValues` edit without a `restart-seid` timeout.
  *Verifier:* judgement — a platform engineer runs the README's four-validator EVM-only topology on harbor, edits one `configValues` entry, and confirms every validator returns to Ready.
- **SC-006**: A declared `consensusParams.block.max_gas` appears in the assembled `genesis.json`.
  *Verifier:* judgement — a platform engineer reads `.consensus_params.block.max_gas` from a validator's `genesis.json`.
- **SC-007**: A `sei-load` EVM-only profile against the pool's `8545` endpoints submits transactions and reads receipts.
  *Verifier:* not built — this needs the harbor `sei-load` Job and PLT-1251's idle-chain rule to land; until then a platform engineer runs the README's `sei-load` step by hand.

## Assumptions

- The engine is an enumeration, not a boolean, because a third engine or mode is plausible and the AIP guidance prefers a name over a flag. `evmOnly` stays a boolean because it is a mode of one engine, not a peer of it.
- The consensus field is create-only on the SeiNetwork through the same CEL shape `spec.genesis` uses, because the artifact and the executor are baked at the ceremony. On a SeiNode it is a plain field; a follower can be recreated.
- The Autobahn artifact rides the S3 genesis prefix rather than a ConfigMap. The ceremony already publishes `genesis.json` there and every node's `configure-genesis` already reads it by `chainId`, so a follower needs no reference to a ConfigMap in another namespace and no new RBAC. A `SeiNode.spec.autobahnConfigRef` is not needed: the follower's `spec.consensus.engine` is the declaration that it should fetch.
- The `docker/rpcnode` script in `sei-chain` regenerates the artifact on the follower from the validator directories. On Harbor the follower downloads the assembler's copy instead, because a follower never holds the validators' identity material and the generator's output is deterministic for the same inputs.
- The generator's `address` field accepts a hostname; `tcp.ParseHostPort` splits host and port and resolves the hostname at dial time. The per-pod Service DNS the peer-collection task already uses therefore serves as the Autobahn address.
- The identity manifest currently carries `node_key.json` whole. Widening it with the two public keys and two addresses is additive; the assembler ignores unknown fields on a Tendermint network.
- The validator public key text (`validator:<base64>`) and node public key text the generator reads are derivable from `priv_validator_key.json` and `node_key.json`; the sidecar computes them, or invokes `seid` to print them, at upload time. Which of the two is an implementation choice, not an API one.
- The controller's own overrides travel as `sei-config` enrichment keys (`network.p2p.persistent_peers`, `chain.freeze_height`). `sei-config` exposes no key for `autobahn-config-file` or `evm-only` today, so the engine keys need either new `sei-config` keys or the raw-TOML `config-patch` path the `configValues` restart plan already uses. The choice is an implementation one; the CRD contract is the same either way.
- EVM-only leaves `[rpc] laddr` empty and disables `api`, `grpc`, and `grpc-web`, following the `docker/localnode` `step4_config_override.sh` sequence in `sei-chain`. Non-EVM-only Autobahn keeps the CometBFT RPC, as that script does, so its health source is unchanged.
- The EVM-only listener returns `200` to a bare GET on `/` with no body: the `go-ethereum` `rpc.Server.ServeHTTP` health-check path, in the fork `sei-chain` pins. An `httpGet` probe therefore needs no `exec`, no POST, and no `sei-chain` change. The listener serves only `eth_sendRawTransaction` and `eth_getTransactionReceipt`, so no `eth_blockNumber` probe is available; that bounds what Ready can mean here.
- The EVM chain ID `713715` is a compile-time constant in `sei-chain` for EVM-only. `spec.genesis.chainId` still names the Cosmos chain and still keys the S3 prefix; the two do not conflict.
- The `restart-seid` process match on `seid start` is unchanged; EVM-only launches through the same subcommand with the engine keys in `config.toml`, not extra flags.
- The README starts `seid` with `--inv-check-period 0 --freeze-height 0`. Whether the controller's defaults are equivalent is not established here; PLT-1250 owns the answer, and a difference there does not block this spec.
- Autobahn disables empty blocks by default, so an idle network stays at height 0. Spec 007's `Producing` needs a height source the EVM-only node does not serve on `26657`; PLT-1251 decides the rule, and this spec keeps Ready as a listener-up signal for EVM-only until it does.
- `consensusParams` is a JSON map merged into one known top-level key rather than a widening of `overrides` to arbitrary top-level keys, so `overrides` keeps its `app_state` meaning and neither field can reach `validators` or `initial_height`.

## Out of scope

- Exposing the generator's tuning (`max_txs_per_block`, `block_interval`, `allow_empty_blocks`, BlockDB retention) as fields. Deferred until a benchmark needs a value the default does not give.
- `seid start` flags. PLT-1250.
- The `Producing` condition and the idle-chain Ready rule. Spec 007 and PLT-1251.
- A generic non-TOML file substrate. The originator's instruction chose the typed path.
- `seictl` flags and the `harbor-dev` recipe for these fields. They follow this spec's landing.
