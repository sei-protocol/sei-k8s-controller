# B1: giga-store migration keys after Running-path regeneration

Measured at head 75157e7 with Go 1.26.0 and sei-config v0.0.28.

**For the only legitimate workflow target (full/RPC), regeneration reverts
rocksdb to pebbledb but does not disable SS or SC.** The enabling flags survive
by default coincidence. Validator and seed regeneration disables SS, but neither
mode can legitimately receive this workflow. SC stays enabled in every mode.

| Node sub-spec | Workflow reachable? | ss-enable | evm-ss-split | ss-backend | sc-enable |
| --- | --- | --- | --- | --- | --- |
| full | Yes | true | true | pebbledb | true |
| archive | No | true | absent | pebbledb | true |
| validator | No | false | absent | pebbledb | true |
| seed | No | false | absent | pebbledb | true |
| replayer | No | true | true | pebbledb | true |

All rows started after the real migration patch with on-disk values
`true / true / rocksdb / true`, in the same column order. `absent` means the
writer omitted the key; it is not a measured TOML false value. Thus the key diff
is backend only for full/replayer; backend plus removal of evm-ss-split for
archive; and those changes plus ss-enable true -> false for validator/seed.

## Reachability and intent evidence

- `internal/planner/workflow.go:43-47` selects the StateSync recipe;
  `:53-65` rejects every target without `spec.fullNode`.
- `internal/controller/node/workflow.go:252-253` checks adoption eligibility;
  `:604-633` explicitly excludes archive, validator, seed and replayer.
- `internal/planner/workflow.go:189-206` writes the four migration keys, with
  the backend supplied by the workflow's GigaStoreMigration.
- `internal/planner/config_update.go:32-54` builds Running intents from the
  node spec, without workflow migration state. Replayer intentionally uses
  ModeFull, matching both `Mode()` and init intent in
  `internal/planner/replay.go:18,45-47`; its overrides tune SC retention/buffers.

## Empirical method

A temporary Go test was injected using Go's `-overlay` facility (no repository
source edits). For each of the five sub-specs it created a Running node with
no user overrides, set HOME to a fresh temporary directory, called the actual
`runningConfigIntent`, `seiconfig.ResolveIntent` and `WriteConfigToDir`, then
applied `gigaStoreConfigPatch(Backend: "rocksdb")` via `tomlpatch.ReadTOML`,
`Merge` and `WriteTOML`. It read the four on-disk keys, built the real mode
planner's first-observation image-update plan, decoded its config-apply and
config-patch task payloads, and executed the same writers/merger against that
HOME. It reread the on-disk TOML and compared the keys. Non-full rows deliberately
bypass workflow adoption solely to measure otherwise unreachable regeneration.

Command:

```text
PATH=/tmp/go/bin:$PATH GOTOOLCHAIN=go1.26.0 go test -overlay=/tmp/b1-overlay.json ./internal/planner -run '^TestB1Probe$' -v
```

Actual output (one test function, five mode subtests):

```text
full:      before=[true true rocksdb true] after=[true true pebbledb true]
archive:   before=[true true rocksdb true] after=[true <nil> pebbledb true]
validator: before=[true true rocksdb true] after=[false <nil> pebbledb true]
seed:      before=[true true rocksdb true] after=[false <nil> pebbledb true]
replayer:  before=[true true rocksdb true] after=[true true pebbledb true]
PASS
ok github.com/sei-protocol/sei-k8s-controller/internal/planner 0.110s
```

B1 remains unresolved: regeneration also runs on every subsequent configValues
edit, so backend loss can recur after an operator restores the migration keys.
No remedy is selected or implemented by this note.
