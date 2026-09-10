# PLT-1210 Piece 1 verification

Go: `go version go1.27.1 linux/amd64`. Commands ran serially with:

```sh
export PATH=/home/omnigent/go-toolchain/bin:$PATH
export GOMAXPROCS=2 GOFLAGS=-p=1 GOMEMLIMIT=1500MiB
```

Root means `~/wt/plt-1210`. No API fields changed; no regeneration required.

## go build ./...

Directory: `/home/omnigent/wt/plt-1210`. Exit 0.

```text
go: downloading go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.43.0
go: downloading go.opentelemetry.io/otel/exporters/prometheus v0.65.0
go: downloading go.opentelemetry.io/otel/sdk/metric v1.43.0
go: downloading go.opentelemetry.io/otel/sdk v1.43.0
go: downloading sigs.k8s.io/gateway-api v1.5.1
go: downloading github.com/btcsuite/btcd/btcec/v2 v2.3.5
go: downloading github.com/cosmos/go-bip39 v1.0.0
go: downloading golang.org/x/crypto v0.49.0
go: downloading github.com/aws/aws-sdk-go-v2/service/ec2 v1.293.0
go: downloading go.opentelemetry.io/proto/otlp v1.10.0
go: downloading google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9
go: downloading google.golang.org/grpc v1.80.0
go: downloading github.com/prometheus/otlptranslator v1.0.0
go: downloading github.com/go-logr/zapr v1.3.0
go: downloading go.uber.org/zap v1.27.0
go: downloading k8s.io/apiserver v0.35.0
go: downloading github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.0
go: downloading github.com/cenkalti/backoff/v5 v5.0.3
go: downloading github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0
go: downloading go.uber.org/multierr v1.11.0
go: downloading k8s.io/component-base v0.35.0
go: downloading google.golang.org/genproto/googleapis/api v0.0.0-20260401024825-9d38bb4040a9
go: downloading github.com/google/cel-go v0.26.0
go: downloading github.com/blang/semver/v4 v4.0.0
go: downloading cel.dev/expr v0.25.1
go: downloading github.com/stoewer/go-strcase v1.3.1
go: downloading github.com/antlr4-go/antlr/v4 v4.13.0
go: downloading sigs.k8s.io/apiserver-network-proxy/konnectivity-client v0.31.2
go: downloading go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.62.0
go: downloading go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.37.0
go: downloading go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.37.0
go: downloading github.com/spf13/cobra v1.10.2
go: downloading golang.org/x/exp v0.0.0-20260112195511-716be5621a96
go: downloading github.com/felixge/httpsnoop v1.0.4
```

## go test ./...

Directory: `/home/omnigent/wt/plt-1210`. Exit 0.

```text
?   	github.com/sei-protocol/sei-k8s-controller/api/v1alpha1	[no test files]
?   	github.com/sei-protocol/sei-k8s-controller/cmd	[no test files]
ok  	github.com/sei-protocol/sei-k8s-controller/internal/controller/node	1.614s
ok  	github.com/sei-protocol/sei-k8s-controller/internal/controller/nodetask	0.254s
?   	github.com/sei-protocol/sei-k8s-controller/internal/controller/observability	[no test files]
ok  	github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork	0.153s
?   	github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest	[no test files]
?   	github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures	[no test files]
ok  	github.com/sei-protocol/sei-k8s-controller/internal/keygen	0.010s
ok  	github.com/sei-protocol/sei-k8s-controller/internal/noderesource	0.155s
ok  	github.com/sei-protocol/sei-k8s-controller/internal/peering	0.107s
ok  	github.com/sei-protocol/sei-k8s-controller/internal/planner	0.114s
ok  	github.com/sei-protocol/sei-k8s-controller/internal/platform	0.007s
?   	github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest	[no test files]
ok  	github.com/sei-protocol/sei-k8s-controller/internal/sidecartransport	0.003s
ok  	github.com/sei-protocol/sei-k8s-controller/internal/task	0.304s
ok  	github.com/sei-protocol/sei-k8s-controller/sdk/sei	0.249s
ok  	github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider	0.008s
ok  	github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/docker	0.002s
ok  	github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/k8s	0.460s
```

## go vet ./...

Directory: `/home/omnigent/wt/plt-1210`. Exit 0.

```text
(no output)
```

## go test ./...

Directory: `/home/omnigent/wt/plt-1210/sidecarapi`. Exit 0.

```text
go: downloading github.com/oapi-codegen/runtime v1.2.0
go: downloading github.com/leanovate/gopter v0.2.11
?   	github.com/sei-protocol/sei-k8s-controller/sidecarapi/api	[no test files]
ok  	github.com/sei-protocol/sei-k8s-controller/sidecarapi/client	0.021s
ok  	github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch	0.002s
ok  	github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire	0.002s
```

## gofmt -l .

Directory: `/home/omnigent/wt/plt-1210`. Exit 0.

```text
(no output)
```

## go test ./internal/planner -run TestConfigValues -v

Directory: root. Exit 0.

```text
=== RUN   TestConfigValuesTypedTOMLFile
    config_overlay_test.go:83: SC-008 actual TOML file:
        untouched = 'base'
        
        [arbitrary]
        [arbitrary.deep]
        count = 42
        enabled = true
        ratio = 1.25
--- PASS: TestConfigValuesTypedTOMLFile (0.01s)
=== RUN   TestConfigValuesInitOrdering
=== RUN   TestConfigValuesInitOrdering/base
=== RUN   TestConfigValuesInitOrdering/state-sync
=== RUN   TestConfigValuesInitOrdering/bootstrap
=== RUN   TestConfigValuesInitOrdering/ceremony
--- PASS: TestConfigValuesInitOrdering (0.00s)
    --- PASS: TestConfigValuesInitOrdering/base (0.00s)
    --- PASS: TestConfigValuesInitOrdering/state-sync (0.00s)
    --- PASS: TestConfigValuesInitOrdering/bootstrap (0.00s)
    --- PASS: TestConfigValuesInitOrdering/ceremony (0.00s)
=== RUN   TestConfigValuesInvalidJSON
--- PASS: TestConfigValuesInvalidJSON (0.00s)
=== RUN   TestConfigValuesAllModePlanners
=== RUN   TestConfigValuesAllModePlanners/full
=== RUN   TestConfigValuesAllModePlanners/archive
=== RUN   TestConfigValuesAllModePlanners/validator
=== RUN   TestConfigValuesAllModePlanners/seed
=== RUN   TestConfigValuesAllModePlanners/replayer
--- PASS: TestConfigValuesAllModePlanners (0.00s)
    --- PASS: TestConfigValuesAllModePlanners/full (0.00s)
    --- PASS: TestConfigValuesAllModePlanners/archive (0.00s)
    --- PASS: TestConfigValuesAllModePlanners/validator (0.00s)
    --- PASS: TestConfigValuesAllModePlanners/seed (0.00s)
    --- PASS: TestConfigValuesAllModePlanners/replayer (0.00s)
PASS
ok  	github.com/sei-protocol/sei-k8s-controller/internal/planner	0.024s
```

## go test tasks/config.go tasks/config_overlay_test.go -v

Directory: sidecar. Exit 0.

```text
=== RUN   TestConfigPatchHandlerTypedOverlay
time=2026-09-10T03:37:40.740Z level=INFO msg="files patched" logger=seictl/task/config-patch count=1
    config_overlay_test.go:49: SC-008 handler output:
        untouched = 'base'
        
        [arbitrary]
        count = 42
        enabled = true
        label = 'true'
        ratio = 1.25
--- PASS: TestConfigPatchHandlerTypedOverlay (0.01s)
PASS
ok  	command-line-arguments	0.014s
```

## Corrections during verification

The first focused invocation failed on a missing TOML dependency checksum.
A build overlapping edits then failed with `could not import bytes`.
The next focused test caught JSON numbers being emitted as quoted strings;
`SetMarshalJsonNumbers(true)` fixed that, and the recorded focused run passed.
The initial sidecar file selection included `config_test.go`, whose unrelated
peer/state-sync tests require more source files, and failed to compile. The new
handler acceptance test was made self-contained; the exact passing file set is
above. A command initially run from the wrong directory made no edits and also
failed; it was corrected before the passing handler run.

The full sidecar module suite was not run. Root and sidecarapi suites passed.
After the final sidecar test-only edit, `gofmt -l .` and `git diff --check` were
rerun and produced no output.

Piece 2 remains separate: no Running-node drift trigger or restart emission was
added. The six legacy mergeOverrides sites remain unchanged; shared base,
bootstrap, and genesis builders insert the independent patch before validation,
after config-apply and any intervening state-sync/genesis peer writes.
