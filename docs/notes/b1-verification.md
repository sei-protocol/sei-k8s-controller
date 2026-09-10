# PR 534 scoped B1 follow-up verification

Code head: 5414944. Go toolchain: `go1.26.0 linux/amd64`, selected with
`GOTOOLCHAIN=go1.26.0` and `/tmp/go/bin` on PATH. No Go 1.27 was used.

Each module ran exactly `go build ./...` and `go test ./...`. Root gates were
rerun after the final predicate/test changes; unchanged packages used Go's test
cache. The targeted planner run after those changes was:

```text
$ go test ./internal/planner
ok github.com/sei-protocol/sei-k8s-controller/internal/planner 0.375s
```

New coverage lives in `internal/planner/config_migration_regression_test.go`:
three test functions, with 15 migration mode/trigger leaf cases, five parity
cases and three phase cases. Migration expectations intentionally pin known
defect B1, including config edits/removals after restoring the migration patch.
The phase cases test the extracted, unchanged predicate directly, with a Running
positive control, as well as exercising ResolvePlan's initialization paths.

Default `go test ./...` does not select envtest-tagged admission tests. The
24-case admission fence and all API files are unchanged in this follow-up.
No API comment changed, so controller-gen/verify-generated was not required.
No B1 remedy, filename restriction, freeze/halt guard or incremental writer was
introduced. The reason constant remains local: existing cross-package reasons
live in the API, while package-local reasons also have precedent (for example
`internal/controller/seinetwork/status.go:151`); UpdateFailed has only planner
consumers and needs no new API surface.

## root (`./...` in that module)

```text
go version go1.26.0 linux/amd64
BUILD_EXIT=0
?   	github.com/sei-protocol/sei-k8s-controller/api/v1alpha1	[no test files]
?   	github.com/sei-protocol/sei-k8s-controller/cmd	[no test files]
ok  	github.com/sei-protocol/sei-k8s-controller/internal/controller/node	3.437s
ok  	github.com/sei-protocol/sei-k8s-controller/internal/controller/nodetask	(cached)
?   	github.com/sei-protocol/sei-k8s-controller/internal/controller/observability	[no test files]
ok  	github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork	(cached)
?   	github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest	[no test files]
?   	github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures	[no test files]
ok  	github.com/sei-protocol/sei-k8s-controller/internal/keygen	(cached)
ok  	github.com/sei-protocol/sei-k8s-controller/internal/noderesource	(cached)
ok  	github.com/sei-protocol/sei-k8s-controller/internal/peering	(cached)
ok  	github.com/sei-protocol/sei-k8s-controller/internal/planner	(cached)
ok  	github.com/sei-protocol/sei-k8s-controller/internal/platform	(cached)
?   	github.com/sei-protocol/sei-k8s-controller/internal/platform/platformtest	[no test files]
ok  	github.com/sei-protocol/sei-k8s-controller/internal/sidecartransport	(cached)
ok  	github.com/sei-protocol/sei-k8s-controller/internal/task	(cached)
ok  	github.com/sei-protocol/sei-k8s-controller/sdk/sei	(cached)
ok  	github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider	(cached)
ok  	github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/docker	(cached)
ok  	github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/k8s	(cached)
TEST_EXIT=0
```

## sidecarapi (`./...` in that module)

```text
go version go1.26.0 linux/amd64
BUILD_EXIT=0
?   	github.com/sei-protocol/sei-k8s-controller/sidecarapi/api	[no test files]
ok  	github.com/sei-protocol/sei-k8s-controller/sidecarapi/client	0.241s
ok  	github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch	0.040s
ok  	github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire	0.099s
TEST_EXIT=0
```

## sidecar (`./...` in that module)

```text
go version go1.26.0 linux/amd64
go: downloading github.com/urfave/cli/v3 v3.6.1
BUILD_EXIT=0
ok  	github.com/sei-protocol/sei-k8s-controller/sidecar	0.021s
ok  	github.com/sei-protocol/sei-k8s-controller/sidecar/actions	0.584s
ok  	github.com/sei-protocol/sei-k8s-controller/sidecar/engine	7.886s
ok  	github.com/sei-protocol/sei-k8s-controller/sidecar/rpc	0.010s
ok  	github.com/sei-protocol/sei-k8s-controller/sidecar/s3	0.013s
ok  	github.com/sei-protocol/sei-k8s-controller/sidecar/server	0.259s
ok  	github.com/sei-protocol/sei-k8s-controller/sidecar/shadow	0.124s
ok  	github.com/sei-protocol/sei-k8s-controller/sidecar/tasks	19.992s
?   	github.com/sei-protocol/sei-k8s-controller/sidecar/tasks/defaults	[no test files]
TEST_EXIT=0
```

## Hygiene

`make tidy-check` exited 0. Actual output below omits only dependency download
progress; no go.mod/go.sum diff was emitted in any module.

```text
==> go mod tidy -diff (.)
==> go mod tidy -diff (sidecarapi)
==> go mod tidy -diff (sidecar)
```

## Root lint

`golangci-lint` v2.12.1, command `golangci-lint run --new-from-rev=cb3f837`,
exited 0. Actual output:

```text
0 issues.
```

The first lint pass caught repeated test strings; reusing existing test constants
removed those findings. No pre-existing findings were changed.
