# Implementation Plan: Kind-Based E2E Test Harness
**Ticket:** sei-k8s-controller #119  
**Date:** 2026-04-23  
**Estimated effort:** ~1.5–2 weeks after envtest harness lands

---

## Objective

Add a `kind`-based e2e test harness to `sei-k8s-controller` that covers the ~20% of scenarios `envtest` can't reach: real pod scheduling, Service DNS resolution, kubelet lifecycle, SSA field-ownership under load, and Gateway API / ServiceMonitor apply paths.

---

## Dependency

**The envtest harness (companion issue) must land first.** It provides the fake-sidecar harness that this work reuses. Do not start Phase 2 (fake sidecar) until the envtest PR is merged.

---

## Constraints and Accepted Gaps

| Constraint | Mitigation | Gap (accepted) |
|---|---|---|
| Karpenter node affinity hardcoded in generators | Label kind nodes with `karpenter.sh/nodepool=local` + matching toleration in `setup.sh` | Fragile if new affinities are added; tracked as follow-up (`PlacementProfile` abstraction) |
| AWS EBS storage classes don't exist in kind | Override via env vars (`SEI_STORAGE_CLASS_DEFAULT=standard`) | No capacity enforcement; PVC size accuracy untestable |
| Prod-shaped PVC sizes (2–25 Ti) | Override via env vars (`SEI_STORAGE_SIZE_DEFAULT=1Gi`) | Size-computation path not tested |
| Large resource requests (4 CPU / 32 Gi) | Override via env vars (`SEI_RESOURCE_CPU_DEFAULT=100m`, `SEI_RESOURCE_MEM_DEFAULT=128Mi`) | Real scheduling capacity not tested |
| No Pod Identity / IRSA | Fake sidecar (option a) by default; real sidecar with embedded genesis (option b) as opt-in | Real AWS / S3 path not tested locally |
| Real `seid` binary OOMs on laptop | `busybox` or thin Go echo binary as `spec.image` | Chain block production not in scope |
| Kind cold boot ~60–90 s | Per-run cluster; CI on `ubuntu-latest` (no Docker Desktop pain) | ~60 s added to every `make test-e2e` |
| ARM Macs (M-series) | Fake sidecar built multi-arch via `docker buildx`; busybox is multi-arch | amd64-only images need Rosetta (~3–5× slower) |

**Non-goals (explicit):** real `seid` producing blocks, MinIO/local S3, multi-cluster tests, perf/throughput on kind, nightly full-resource run (deferred).

---

## File Structure to Create

```
sei-k8s-controller/
├── test/
│   └── e2e/
│       ├── kind/
│       │   ├── setup.sh            # Cluster bootstrap
│       │   ├── teardown.sh         # Explicit cluster teardown
│       │   └── kind-config.yaml    # Kind cluster config (node count, port mappings)
│       ├── fakesidecar/
│       │   ├── main.go             # Thin HTTP server mimicking sidecar task API
│       │   └── Dockerfile          # Multi-arch build
│       ├── suite_test.go           # TestMain: envSetup + t.Cleanup teardown
│       ├── scheduling_test.go      # Real pod scheduling path
│       ├── genesis_ceremony_test.go # Multi-node genesis ceremony
│       ├── dns_test.go             # Service DNS resolution
│       ├── rollout_test.go         # NodeUpdate rolling semantics
│       └── ssa_test.go             # SSA field-ownership under kubelet pressure
├── .github/
│   └── workflows/
│       └── e2e.yml                 # Separate CI job (non-blocking initially)
└── Makefile                        # test-e2e target added
```

---

## Phase 0 — Prerequisite Verification (0.5 day)

**Goal:** Confirm the envtest harness is merged and the fake sidecar interface is stable.

Tasks:
- [ ] Confirm envtest PR is merged; read fake sidecar's task API contract (endpoints, request/response shapes)
- [ ] Confirm `platform.Config` env var overrides for storage class, storage size, CPU, and memory are wired up (or file a follow-up if they aren't)
- [ ] Document the exact env var names used by the generator code in a `test/e2e/README.md` stub

---

## Phase 1 — Kind Cluster Infrastructure (2–3 days)

**Goal:** Reproducible, hermetic kind cluster that the controller can actually schedule pods into.

### 1a. `test/e2e/kind/kind-config.yaml`

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
  - role: worker
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = ""
```

Three workers: enough to test multi-node genesis ceremony and pod anti-affinity. Control-plane is untainted (kind default) so it can also schedule if needed.

### 1b. `test/e2e/kind/setup.sh`

Steps in order:
1. `kind create cluster --name sei-k8s-e2e-${USER} --config kind-config.yaml` (idempotent: skip if exists)
2. Label all worker nodes: `kubectl label node "$node" karpenter.sh/nodepool=local --overwrite`
3. Taint all worker nodes: `kubectl taint node "$node" ${SEI_TOLERATION_KEY}=true:NoSchedule --overwrite 2>/dev/null || true`
4. Install `local-path-provisioner` (if not present): `kubectl apply -f https://raw.githubusercontent.com/rancher/local-path-provisioner/.../local-path-storage.yaml`
5. Patch `local-path` as default StorageClass
6. Install Gateway API CRDs (schemas only, no controller): `kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.1.0/standard-install.yaml`
7. Install prometheus-operator CRDs (schemas only): `kubectl apply -f https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/.../bundle.yaml --server-side`
8. `kind load docker-image sei-k8s-fakesidecar:test --name sei-k8s-e2e-${USER}`
9. `make deploy` (or `kubectl apply -k config/default`) to install the controller

Env vars respected by setup.sh:
- `KIND_CLUSTER_NAME` (default: `sei-k8s-e2e-${USER}`)
- `SEI_TOLERATION_KEY` (default: `karpenter.sh/disruption`)
- `SKIP_CONTROLLER_DEPLOY` (default: unset; set to skip step 9 when testing setup in isolation)

### 1c. `test/e2e/kind/teardown.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-sei-k8s-e2e-${USER}}"
kind delete cluster --name "${KIND_CLUSTER_NAME}"
```

One job: delete the cluster. Registered from `t.Cleanup` in Go tests AND as a CI `if: always()` step.

Tasks:
- [ ] Write `kind-config.yaml`
- [ ] Write `setup.sh` with all 9 steps; test on macOS Docker Desktop and Linux
- [ ] Write `teardown.sh`
- [ ] Verify 0 orphan clusters after `setup.sh && teardown.sh` on a cold machine
- [ ] Verify 0 orphan clusters after a forced `kill -9` mid-run (simulate CI SIGKILL)

---

## Phase 2 — Fake Sidecar Image (1–2 days)

**Goal:** Build and load a fake sidecar image that satisfies the controller's sidecar contract without AWS.

### `test/e2e/fakesidecar/main.go`

- Thin HTTP server on the port the controller expects
- Implements task endpoints the controller polls: `GET /tasks`, `POST /tasks/{id}/complete`, `GET /genesis`, etc.
- Returns canned responses; does not touch S3 or any external service
- Exits cleanly on SIGTERM

### `test/e2e/fakesidecar/Dockerfile`

```dockerfile
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder
ARG TARGETOS TARGETARCH
WORKDIR /app
COPY . .
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o fakesidecar .

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app/fakesidecar /fakesidecar
ENTRYPOINT ["/fakesidecar"]
```

Build and load:
```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t sei-k8s-fakesidecar:test \
  --load test/e2e/fakesidecar/
kind load docker-image sei-k8s-fakesidecar:test --name sei-k8s-e2e-${USER}
```

Tasks:
- [ ] Read envtest harness fake sidecar; extract shared interface into a `test/e2e/fakesidecar/` package (or copy if the envtest harness doesn't parameterize it)
- [ ] Write multi-arch Dockerfile
- [ ] Add `make build-fakesidecar` target
- [ ] Verify image loads into kind and is pullable by a test pod (`imagePullPolicy: Never`)

---

## Phase 3 — Go Test Suite (4–5 days)

**Goal:** Four test scenarios that exercise the behaviors `envtest` cannot reach.

### `test/e2e/suite_test.go` — Test suite bootstrap

```go
func TestMain(m *testing.M) {
    // 1. Connect to kind cluster (uses KUBECONFIG or kind get kubeconfig)
    // 2. Set up k8s client
    // 3. Register t.Cleanup: teardown.sh (belt-and-suspenders)
    // 4. os.Exit(m.Run())
}
```

Each test:
- Creates a unique namespace (e.g., `test-<uuid>`)
- Defers `kubectl delete namespace` for cleanup
- Uses `gomega`/`ginkgo` or plain `testing` + retry helpers (match existing project convention)

Env vars consumed by tests:
```
KIND_CLUSTER_NAME          (default: sei-k8s-e2e-${USER})
SEI_STORAGE_CLASS_DEFAULT  (default: standard)
SEI_STORAGE_SIZE_DEFAULT   (default: 1Gi)
SEI_RESOURCE_CPU_DEFAULT   (default: 100m)
SEI_RESOURCE_MEM_DEFAULT   (default: 128Mi)
TEST_TIMEOUT               (default: 5m per test)
```

### Test 1: `scheduling_test.go` — SND → SeiNode → StatefulSet → Pod real scheduling

1. Apply a `SeiNodeDeployment` CR with `spec.replicas: 1`, `spec.image: busybox`
2. Wait (up to 3 min) for controller to create `SeiNode` → `StatefulSet` → `Pod`
3. Assert `Pod.status.phase == Running`
4. Assert pod is on a node labeled `karpenter.sh/nodepool=local`
5. Assert `PVC` is `Bound`

**What this proves:** The full reconcile chain runs against a real scheduler, real kubelet, and real local-path provisioner.

### Test 2: `genesis_ceremony_test.go` — Multi-node genesis ceremony

1. Apply a `SeiNodeDeployment` with `spec.replicas: 3`
2. Wait for all 3 pods to be `Running`
3. Assert exactly one pod has the ceremony-leader annotation/label set by the controller
4. Assert all 3 pods reach the controller-defined "genesis complete" state (check status condition or annotation)
5. Assert `Endpoints` for the headless service lists all 3 pod IPs

**What this proves:** Genesis leader selection is correct with real `Endpoints` updates (not simulated ones).

### Test 3: `dns_test.go` — Service DNS resolution

1. Apply `SeiNodeDeployment` with `spec.replicas: 2`
2. Wait for pods `Running`
3. `kubectl exec` into pod-0, run `nslookup ${pod-1}.${headless-svc}.${ns}.svc.cluster.local`
4. Assert the resolved IP matches pod-1's `status.podIP`

**What this proves:** Headless service DNS works end-to-end; requires real CoreDNS (not present in envtest).

### Test 4: `rollout_test.go` — NodeUpdate rolling semantics

1. Apply `SeiNodeDeployment` with `spec.replicas: 3`, record current image
2. Patch `spec.image` to a new value (`busybox:latest` → `busybox:1.36`)
3. Wait for StatefulSet rolling update to complete (`UpdatedReplicas == Replicas`)
4. Assert each pod was restarted (check `pod.status.containerStatuses[0].restartCount` or pod UID changed)
5. Assert no more than 1 pod is unavailable at any point during the rollout (sample status during rollout)

**What this proves:** Controller correctly drives StatefulSet rolling updates under real kubelet pressure.

### Test 5 (stretch): `ssa_test.go` — SSA field-ownership under load

1. Apply a `SeiNodeDeployment`, wait for pod `Running`
2. Concurrently: controller applies its managed fields; test directly patches the same pod's labels via a different field manager
3. Assert: no field-manager conflict error surfaces to the controller's status conditions
4. Assert: controller's fields are not overwritten; test's patch fields are preserved

**What this proves:** SSA conflict-then-force-apply paths work correctly with real API server (not envtest's simplified SSA).

Tasks:
- [ ] Write `suite_test.go` with TestMain, namespace helper, retry/wait helpers
- [ ] Write `scheduling_test.go` (Test 1)
- [ ] Write `genesis_ceremony_test.go` (Test 2)
- [ ] Write `dns_test.go` (Test 3)
- [ ] Write `rollout_test.go` (Test 4)
- [ ] Write `ssa_test.go` (Test 5, stretch)
- [ ] All tests pass locally on macOS (Docker Desktop) with `make test-e2e`
- [ ] Suite completes in < 5 min on a 16-core laptop (time it)

---

## Phase 4 — Makefile Target (0.5 day)

**Goal:** `make test-e2e` is the single entry point; not part of the default `make test` run.

```makefile
.PHONY: test-e2e
test-e2e: build-fakesidecar
    @if [ "$(SETUP)" = "true" ]; then bash test/e2e/kind/setup.sh; fi
    go test ./test/e2e/... \
      -v \
      -timeout 10m \
      -count=1 \
      $(TEST_FLAGS)

.PHONY: test-e2e-full
test-e2e-full: build-fakesidecar
    bash test/e2e/kind/setup.sh
    go test ./test/e2e/... -v -timeout 10m -count=1
    bash test/e2e/kind/teardown.sh

.PHONY: build-fakesidecar
build-fakesidecar:
    docker buildx build --platform linux/amd64,linux/arm64 \
      -t sei-k8s-fakesidecar:test \
      --load test/e2e/fakesidecar/
```

`make test-e2e SETUP=true` provisions cluster + runs tests (useful for CI and fresh machines).  
`make test-e2e` alone assumes cluster exists (useful for iterating locally).  
`make test-e2e-full` does setup + test + teardown in one shot.

Tasks:
- [ ] Add targets to Makefile
- [ ] Verify `make test-e2e-full` works end-to-end on a clean machine (no pre-existing kind cluster)
- [ ] Verify default `make test` does NOT run e2e tests

---

## Phase 5 — CI Integration (1 day)

**Goal:** E2E suite runs on every PR in a separate, non-blocking job.

### `.github/workflows/e2e.yml`

```yaml
name: E2E Tests (kind)

on:
  pull_request:
  push:
    branches: [main]

jobs:
  e2e:
    runs-on: ubuntu-latest
    # Non-blocking initially — remove continue-on-error once suite is stable
    continue-on-error: true
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true

      - name: Install kind
        uses: helm/kind-action@v1.10.0
        with:
          install_only: true
          version: v0.23.0

      - name: Build fake sidecar
        run: make build-fakesidecar

      - name: Setup kind cluster
        run: bash test/e2e/kind/setup.sh
        env:
          KIND_CLUSTER_NAME: sei-k8s-e2e-ci

      - name: Run e2e tests
        run: make test-e2e
        env:
          KIND_CLUSTER_NAME: sei-k8s-e2e-ci
          SEI_STORAGE_CLASS_DEFAULT: standard
          SEI_STORAGE_SIZE_DEFAULT: 1Gi
          SEI_RESOURCE_CPU_DEFAULT: 100m
          SEI_RESOURCE_MEM_DEFAULT: 128Mi

      - name: Teardown kind cluster
        if: always()
        run: bash test/e2e/kind/teardown.sh
        env:
          KIND_CLUSTER_NAME: sei-k8s-e2e-ci
```

Tasks:
- [ ] Write `.github/workflows/e2e.yml`
- [ ] Verify workflow triggers on PR and passes on `ubuntu-latest`
- [ ] Confirm teardown step runs even when tests fail (`if: always()`)
- [ ] Once suite is stable for 2+ weeks, remove `continue-on-error: true`

---

## Phase 6 — Documentation (0.5 day)

### `test/e2e/README.md`

Cover:
- Prerequisites: `kind >= 0.20`, `kubectl`, `docker` in PATH; `docker buildx` for multi-arch fake sidecar
- Quick start: `make test-e2e-full`
- Iterating locally: `make test-e2e SETUP=true` first time; `make test-e2e` after that
- Emergency cleanup: `kind delete clusters --all`
- Env vars reference table (all overrides, their defaults, and what they affect)
- Known limitations (from the Constraints table above)
- Adding a new test: namespace helper, retry pattern, fake sidecar usage
- Real sidecar opt-in path (option b: embedded genesis for `pacific-1`/`atlantic-2`; manual `kind load`)

Tasks:
- [ ] Write `test/e2e/README.md`
- [ ] Add one-liner to top-level `CONTRIBUTING.md` or `README.md` pointing to it

---

## Done Criteria

- [ ] `make test-e2e-full` provisions a kind cluster, runs all 4 tests (5 if SSA stretch is complete), tears down the cluster, exits 0 — in < 5 min on a 16-core laptop
- [ ] Zero orphan kind clusters after a full run, including when tests fail midway
- [ ] Zero orphan clusters after a simulated SIGKILL (run `kill -9` on the test process mid-run)
- [ ] CI workflow passes on `ubuntu-latest` (GH Actions) with no manual intervention
- [ ] Works on macOS (Docker Desktop M-series) and Linux
- [ ] Default `make test` does NOT run e2e tests
- [ ] `test/e2e/README.md` lets a new engineer run `make test-e2e-full` successfully without asking for help

---

## Open Questions (Deferred)

| Question | Recommended default | When to revisit |
|---|---|---|
| Long-lived cluster vs. per-run | Per-run (cleaner isolation) | If `make test-e2e` > 3 min due to boot time; promote to long-lived + namespace reset |
| Helm chart for controller in kind | `make deploy` via kustomize (simpler now) | Once the harness is in use; Helm makes version-pin story cleaner |
| Nightly "big kind" run with real seid on beefy runner | Defer | After harness ships; decide based on whether lightweight kind misses real regressions |
| `PlacementProfile` abstraction to decouple Karpenter affinity | Follow-up issue | When a new affinity/toleration is added and breaks setup.sh |

---

## Risk Register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Docker Desktop flakes on macOS (VM pressure, network namespace races) | Medium | Medium | `t.Eventually` with generous timeouts; CI on native Linux avoids this |
| Karpenter label/toleration changes in generator code break setup.sh | Medium | High | Unit test in `setup.sh` that verifies all node affinities on created pods are satisfiable |
| Fake sidecar task API drifts from real sidecar | Low | High | Integration test in envtest harness that exercises both; shared interface type |
| kind boot time creep (>90s on CI) | Low | Low | Monitor CI step timing; switch to pre-built kind images if needed |
| Gateway API / prometheus CRD schema versions change upstream | Low | Low | Pin CRD install URLs by version tag; bump intentionally |
