# sei-k8s-controller

Kubernetes operator for managing Sei blockchain nodes. Single binary, two controllers: `SeiNodeGroup` (fleet orchestration, genesis ceremonies, deployments) and `SeiNode` (individual node lifecycle).

## Architecture

- **API group**: `sei.io/v1alpha1`
- **CRD types**: `SeiNodeGroup`, `SeiNode` (defined in `api/v1alpha1/`)
- **Controllers**: `internal/controller/nodegroup/`, `internal/controller/node/`
- **Entry point**: `cmd/main.go` — thin binary that creates a `manager.Manager` and registers both controllers
- **Framework**: controller-runtime v0.23.1 / kubebuilder v4.12.0

## Subagents

Always use the available subagents for relevant work:
- **kubernetes-specialist**: Use for all Kubernetes design, deployment, troubleshooting, and operational decisions. Consult before making changes to CRDs, RBAC, kustomize configs, StatefulSet specs, or any cluster-facing resource definitions.
- **platform-engineer**: Use for architectural decisions about platform composability, developer experience, CI/CD workflows, and infrastructure abstraction patterns.

## Code Standards

### Go
- Follow idiomatic Go. No unnecessary abstractions — three similar lines are better than a premature helper.
- Use `controller-runtime` patterns: every controller exports a reconciler struct with `SetupWithManager(mgr)`.
- All code must pass `golangci-lint` (config in `.golangci.yml`). Fix lint issues, don't suppress them.
- Imports must be grouped: stdlib, external, then `github.com/sei-protocol/sei-k8s-controller` (enforced by goimports).
- No `panic` in controller code. Return errors and let the reconciler retry.
- Keep reconcile loops idempotent — every reconcile should converge toward desired state regardless of current state.

### Testing
- Tests use `testing` + `gomega` for assertions.
- Test fixtures for platform config live in `internal/platform/platformtest/`.
- Run tests with `make test` before submitting changes.

### CRD Changes
- Edit types in `api/v1alpha1/` (e.g., `seinode_types.go`, `seinodegroup_types.go`, `validator_types.go`).
- After any type change, run `make manifests generate` to regenerate CRD YAML and DeepCopy methods.
- Never hand-edit files in `manifests/` or `zz_generated.deepcopy.go`.

### RBAC
- RBAC is generated from `// +kubebuilder:rbac:` markers on controller files.
- After changing markers, run `make manifests` to regenerate `manifests/role.yaml`.

## Build & Deploy

```bash
make build                    # Build the manager binary
make test                     # Run unit tests
make lint                     # Run golangci-lint
make manifests generate       # Regenerate CRDs, RBAC, DeepCopy after type changes
make docker-build IMG=<image> # Build container image
make docker-push IMG=<image>  # Push container image
```

## Key Patterns

- **SeiNodeGroup** creates and owns **SeiNode** resources. Groups orchestrate genesis ceremonies, manage deployments, and coordinate networking/monitoring.
- **SeiNode** creates StatefulSets (replicas=1), headless Services, and PVCs via server-side apply (fieldOwner: `seinode-controller`).
- **Platform config** is fully environment-driven — all fields in `platform.Config` must be set via env vars (no defaults). See `internal/platform/platform.go` for the full list.
- **Genesis resolution** is handled by the sidecar autonomously: embedded sei-config for well-known chains, S3 fallback at `{SEI_GENESIS_BUCKET}/{chainID}/genesis.json` for custom chains.
- Sidecar bootstrap progression is driven by the node controller polling the sidecar HTTP API and submitting tasks in sequence.
- Config keys in seid's `config.toml` use **hyphens** (e.g., `persistent-peers`, `trust-height`), not underscores.
