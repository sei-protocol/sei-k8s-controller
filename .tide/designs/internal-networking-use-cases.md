# Internal Networking Use Cases for sei-k8s-controller

Design exploration: how the operator's networking primitives serve internal development, testing, and load testing workflows where the topology does not match production.

---

## Networking Primitives Available Today

The operator exposes two layers of networking:

**Per-node (SeiNode controller):**
- Headless Service (`ClusterIP: None`) with `PublishNotReadyAddresses: true`, named identically to the SeiNode. Provides stable DNS at `{node-name}-0.{node-name}.{namespace}.svc.cluster.local` for every port seid exposes. This exists unconditionally for every node.

**Per-group (SeiNodeGroup controller):**
- External Service (`{group-name}-external`) -- ClusterIP, LoadBalancer, or NodePort. Selector targets `sei.io/nodegroup: {name}` (plus `sei.io/revision` during deployments). Ports derived from node mode via `seiconfig.NodePortsForMode()`.
- HTTPRoute (Gateway API) -- routes baseDomain subdomains (`rpc.*`, `rest.*`, `grpc.*`, `evm-rpc.*`, `evm-ws.*`) to the external Service. Requires Gateway CRDs and a Gateway implementation.
- AuthorizationPolicy (Istio) -- ALLOW policy restricting which identities can reach pods. Auto-injects the controller SA.
- ServiceMonitor (Prometheus Operator) -- scrapes the `metrics` port.

**Key insight for internal use:** The headless Services and the external ClusterIP Service are plain Kubernetes networking. No Gateway, no Istio, no external DNS required. Any pod in the cluster can reach them directly via Kubernetes DNS. This is the foundation for all internal workflows.

---

## Use Case 1: Compose a Test Network

**Goal:** Spin up a complete chain (validators + RPCs + state syncers) in a single namespace for integration testing.

### Architecture

Use one SeiNodeGroup per role. The genesis ceremony group creates the chain; additional groups join it as full nodes. All groups live in the same namespace for DNS simplicity.

```
Namespace: integration-test
  |
  +-- SeiNodeGroup "testnet-validators"    (genesis ceremony, 4 validators)
  |     +-- SeiNode "testnet-validators-0"
  |     +-- SeiNode "testnet-validators-1"
  |     +-- SeiNode "testnet-validators-2"
  |     +-- SeiNode "testnet-validators-3"
  |
  +-- SeiNodeGroup "testnet-rpc"           (3 full nodes, ClusterIP service)
  |     +-- SeiNode "testnet-rpc-0"
  |     +-- SeiNode "testnet-rpc-1"
  |     +-- SeiNode "testnet-rpc-2"
  |
  +-- SeiNodeGroup "testnet-state-syncer"  (1 state syncer)
        +-- SeiNode "testnet-state-syncer-0"
```

### Manifests

**Step 1: Validator group with genesis ceremony**

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodeGroup
metadata:
  name: testnet-validators
  namespace: integration-test
spec:
  replicas: 4

  genesis:
    chainId: integration-test-1
    stakingAmount: "10000000usei"
    accountBalance: "1000000000000000000000usei,1000000000000000000000uusdc"
    # Fund test accounts for load testing
    accounts:
      - address: "sei1testaccount..."
        balance: "1000000000000000000000usei"

  template:
    metadata:
      labels:
        sei.io/role: validator
    spec:
      chainId: integration-test-1
      image: "ghcr.io/sei-protocol/sei:v6.3.0"
      entrypoint:
        command: ["seid"]
        args: ["start", "--home", "/sei"]
      sidecar:
        image: ghcr.io/sei-protocol/seictl@sha256:2cb320dd...
      validator: {}

  # No networking section -- validators do not need external traffic.
  # The per-node headless Services are sufficient for P2P communication.
```

**Step 2: RPC group that peers with the validators**

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodeGroup
metadata:
  name: testnet-rpc
  namespace: integration-test
spec:
  replicas: 3

  template:
    metadata:
      labels:
        sei.io/role: rpc
    spec:
      chainId: integration-test-1
      image: "ghcr.io/sei-protocol/sei:v6.3.0"
      entrypoint:
        command: ["seid"]
        args: ["start", "--home", "/sei"]
      sidecar:
        image: ghcr.io/sei-protocol/seictl@sha256:2cb320dd...

      # Discover validator nodes by label -- no EC2, no static IPs
      peers:
        - label:
            selector:
              sei.io/nodegroup: testnet-validators

      fullNode: {}

  networking:
    service:
      type: ClusterIP
    # No gateway -- internal only
    # No isolation -- test namespace is trusted
```

This creates `testnet-rpc-external.integration-test.svc.cluster.local` as a ClusterIP Service load-balancing across all 3 RPC nodes.

### DNS Topology

| DNS Name | Type | What it reaches |
|----------|------|-----------------|
| `testnet-rpc-external.integration-test.svc.cluster.local` | ClusterIP | Round-robin across all RPC pods |
| `testnet-rpc-0-0.testnet-rpc-0.integration-test.svc.cluster.local` | Headless | Specific RPC pod (ordinal 0) |
| `testnet-validators-0-0.testnet-validators-0.integration-test.svc.cluster.local` | Headless | Specific validator pod (ordinal 0) |

### What Works Today

- Genesis ceremony orchestration is fully automated -- the group controller handles identity generation, gentx collection, genesis assembly, and peer discovery.
- Label-based peer discovery (`peers[].label.selector`) resolves to headless DNS names via the node controller's `reconcilePeers()`. The RPC group automatically discovers validator nodes without hardcoded addresses.
- The ClusterIP external Service is created with correct ports derived from the node mode.

### What Is Missing or Could Be Improved

1. **Cross-group genesis awareness.** The RPC group has no way to know when the genesis ceremony is complete. Today you must either wait for the validators to reach `Ready` phase before applying the RPC group, or the RPC nodes will fail their `configure-genesis` task and retry for up to 30 minutes. An explicit `dependsOn` or genesis-readiness gate on the SeiNodeGroup would make this deterministic.

2. **Namespace-scoped genesis sharing.** The genesis ceremony uploads artifacts to S3. The RPC group's sidecar downloads genesis from S3 using the chain ID. This works but requires S3 access from the test cluster. For fully local test networks, an in-cluster genesis distribution mechanism (ConfigMap or PVC-based) would remove the S3 dependency.

3. **Test accounts in genesis.** The `genesis.accounts` field supports funded accounts, but there is no built-in way to generate deterministic test keys. Users must bring their own mnemonics or addresses.

---

## Use Case 2: Load Test an RPC Fleet

**Goal:** Run synthetic load against a group of RPC nodes behind a single endpoint, measure throughput and latency.

### Architecture

The existing load test pattern (from `manifests/samples/jobs/loadtest-job.yaml`) targets individual node headless Services. For fleet-level load testing, target the group's ClusterIP external Service instead -- Kubernetes distributes connections across all backends.

### Manifest: Load Test Job Targeting the External Service

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: rpc-loadtest-config
  namespace: integration-test
data:
  profile.json: |
    {
      "chainId": 713715,
      "seiChainId": "integration-test-1",
      "endpoints": [
        "http://testnet-rpc-external.integration-test.svc.cluster.local:8545"
      ],
      "accounts": {
        "count": 200,
        "newAccountRate": 0.0
      },
      "scenarios": [
        { "name": "EVMTransfer", "weight": 7 },
        { "name": "EVMContractDeploy", "weight": 1 },
        { "name": "EVMContractCall", "weight": 2 }
      ],
      "settings": {
        "workers": 200,
        "tps": 500,
        "statsInterval": "5s",
        "bufferSize": 500,
        "trackBlocks": true,
        "prewarm": true,
        "rampUp": true
      }
    }
---
apiVersion: batch/v1
kind: Job
metadata:
  name: rpc-fleet-loadtest
  namespace: integration-test
spec:
  ttlSecondsAfterFinished: 3600
  parallelism: 4
  completions: 4
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: seiload
          image: ghcr.io/sei-protocol/sei-load:latest
          args:
            - --config
            - /etc/seiload/profile.json
          resources:
            requests:
              cpu: "2"
              memory: 2Gi
          volumeMounts:
            - name: config
              mountPath: /etc/seiload
      volumes:
        - name: config
          configMap:
            name: rpc-loadtest-config
```

### Load Test Targeting Strategy Comparison

| Strategy | Endpoint | When to use |
|----------|----------|-------------|
| **Fleet (ClusterIP)** | `testnet-rpc-external:8545` | Measure aggregate fleet throughput, realistic production-like routing |
| **Per-node (headless)** | `testnet-rpc-0-0.testnet-rpc-0:8545` | Stress a single node, identify per-node bottlenecks |
| **Fan-out (all headless)** | List all node endpoints | Saturate every node simultaneously, find the weakest link |

The existing load test sample uses the fan-out strategy (listing every `genesis-test-N-0.genesis-test-N` headless address). For production-representative load testing, the fleet strategy via ClusterIP is preferred -- it exercises the same connection distribution that real clients experience behind a Gateway.

### Cross-Namespace Load Testing

If the load test infrastructure lives in a shared `loadtest` namespace while the RPC fleet is in `integration-test`:

```yaml
# Endpoint in the load test config:
"endpoints": [
  "http://testnet-rpc-external.integration-test.svc.cluster.local:8545"
]
```

Kubernetes DNS resolves cross-namespace service names with the full FQDN. No special configuration required. The only blocker would be Istio AuthorizationPolicy -- but for test environments, omit the `isolation` section entirely.

### What Is Missing or Could Be Improved

1. **No Service-level metrics from the operator.** The external Service distributes load, but there is no built-in way to see per-node request distribution, error rates, or latency percentiles from the operator's perspective. The ServiceMonitor scrapes seid Prometheus metrics (block height, consensus state), not HTTP request metrics. For load testing visibility, you need either Istio telemetry (if in the mesh) or a sidecar metrics proxy.

2. **No built-in load test integration.** The load test job is a standalone manifest. A `loadTest` field on SeiNodeGroup (or a separate `SeiLoadTest` CRD) could automate the lifecycle: wait for Ready, run load, collect results, tear down.

3. **Connection pooling awareness.** ClusterIP uses iptables/IPVS round-robin per new connection. For HTTP/2 or long-lived connections, a single connection pins to one backend. The operator does not configure session affinity or connection balancing -- users must ensure their load generator opens many short-lived connections or uses HTTP/1.1 for even distribution.

---

## Use Case 3: Test Upgrade Flows (HardFork Deployments)

**Goal:** Validate a HardFork deployment in staging before running it in production.

### Architecture

Create a genesis network, let it produce blocks, then apply a HardFork update strategy with a new image. The operator orchestrates the halt-height signaling, entrant node creation, binary switch, and teardown.

### Manifests

**Initial deployment:**

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNodeGroup
metadata:
  name: upgrade-test
  namespace: staging
spec:
  replicas: 4

  genesis:
    chainId: upgrade-test-1
    stakingAmount: "10000000usei"
    accountBalance: "1000000000000000000000usei"
    overrides:
      # Set a low halt height so the upgrade triggers quickly
      # (in practice, use a realistic height for staging)

  template:
    metadata:
      labels:
        sei.io/role: validator
    spec:
      chainId: upgrade-test-1
      image: "ghcr.io/sei-protocol/sei:v6.3.0"
      entrypoint:
        command: ["seid"]
        args: ["start", "--home", "/sei"]
      sidecar:
        image: ghcr.io/sei-protocol/seictl@sha256:2cb320dd...
      validator: {}

  updateStrategy:
    type: HardFork

  networking:
    service:
      type: ClusterIP
```

**Trigger the upgrade (edit the image and set halt height):**

```yaml
spec:
  template:
    spec:
      image: "ghcr.io/sei-protocol/sei:v7.0.0"
  updateStrategy:
    type: HardFork
    hardFork:
      haltHeight: 1000
```

The operator detects the templateHash change, creates entrant nodes with the new image, signals the incumbent nodes to halt at the specified height via `await-condition` with SIGTERM, waits for the entrants to catch up, switches the external Service selector to the new revision, and tears down the incumbents.

### Networking During Upgrades

During a HardFork deployment, the external Service selector adds `sei.io/revision: {incumbentRevision}` to pin traffic to the active set. After the switch, the selector updates to the entrant revision. For internal testing, this means:

- Any pod targeting the ClusterIP external Service sees zero-downtime if the halt and switch complete within the Service's readiness probe window.
- You can observe both sets simultaneously by targeting headless Services directly:
  - Incumbent: `upgrade-test-0-0.upgrade-test-0.staging.svc.cluster.local`
  - Entrant: `upgrade-test-g2-0-0.upgrade-test-g2-0.staging.svc.cluster.local`

### What Is Missing or Could Be Improved

1. **No staging-specific halt height automation.** In production, the halt height is coordinated across the network. In staging, you want the chain to produce enough blocks to exercise the upgrade handler, then halt. There is no built-in "halt after N blocks" semantic -- you must calculate and set the height manually.

2. **No upgrade dry-run mode.** A mode that creates entrant nodes, verifies they sync past the upgrade height, but does NOT switch traffic or tear down incumbents would let teams validate upgrade compatibility without committing to the switch.

3. **No rollback.** HardFork deployments are one-way. If the new binary fails, the plan enters Failed state. The incumbents are already halted. Recovery requires manual intervention (new group, restore from snapshot). For staging this is acceptable, but documenting the recovery path would help.

---

## Use Case 4: Debug Individual Node Behavior

**Goal:** Connect directly to a specific node in a group for debugging -- inspect RPC responses, check sync status, query state.

### How It Works Today

Every SeiNode gets a headless Service with all ports exposed and `PublishNotReadyAddresses: true`. This means the node is DNS-reachable even during initialization, before it passes readiness probes.

**Direct node access:**

```bash
# From any pod in the cluster (or via kubectl port-forward from your workstation):

# CometBFT RPC (status, net_info, consensus_state)
curl http://testnet-rpc-0-0.testnet-rpc-0.integration-test.svc.cluster.local:26657/status

# EVM JSON-RPC
curl -X POST http://testnet-rpc-0-0.testnet-rpc-0.integration-test.svc.cluster.local:8545 \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'

# gRPC reflection
grpcurl -plaintext testnet-rpc-0-0.testnet-rpc-0.integration-test.svc.cluster.local:9090 list

# REST API
curl http://testnet-rpc-0-0.testnet-rpc-0.integration-test.svc.cluster.local:1317/cosmos/base/tendermint/v1beta1/syncing

# Sidecar API (task status, health, diagnostics)
curl http://testnet-rpc-0-0.testnet-rpc-0.integration-test.svc.cluster.local:7777/v0/healthz
curl http://testnet-rpc-0-0.testnet-rpc-0.integration-test.svc.cluster.local:7777/v0/tasks
```

**From your workstation via port-forward:**

```bash
# Forward a specific node's RPC port
kubectl port-forward -n integration-test svc/testnet-rpc-0 26657:26657

# Or forward the sidecar for task diagnostics
kubectl port-forward -n integration-test svc/testnet-rpc-0 7777:7777
```

### Comparing Nodes Side-by-Side

Because headless Services give per-node addressability, you can compare responses across nodes in a group:

```bash
# Check sync status across all nodes in a group
for i in 0 1 2; do
  echo "--- testnet-rpc-$i ---"
  curl -s "http://testnet-rpc-$i-0.testnet-rpc-$i.integration-test.svc.cluster.local:26657/status" \
    | jq '.result.sync_info.latest_block_height'
done
```

### What Is Missing or Could Be Improved

1. **No debug port aggregation.** To inspect all nodes in a group, you must iterate over headless Services manually. A debug endpoint on the external Service that fans out queries to all backends and aggregates responses would simplify multi-node debugging.

2. **No built-in exec/attach shortcut.** Debugging often requires `kubectl exec` into the seid or sidecar container. The operator could provide a `kubectl sei debug <node-name>` plugin that resolves the pod and container automatically.

3. **Sidecar task history is ephemeral.** The sidecar's `/v0/tasks` endpoint shows current task state, but task history is lost on pod restart. For post-mortem debugging, task history should be persisted (in the SeiNode status or an external store).

---

## Use Case 5: Mirror Production Traffic

**Goal:** Replay production RPC traffic against a test fleet to validate correctness before cutover.

### How It Works Today

The `manifests/samples/istio/pacific-1-rpc-mirror/` directory contains a complete Istio traffic mirroring setup:

1. **ServiceEntry** registers the EC2 RPC ALB as `ec2-rpc.pacific-1.internal` (mesh-external).
2. **DestinationRule** disables mTLS for the EC2 backend, prevents HTTP/2 upgrade (CometBFT is HTTP/1.1), configures outlier detection.
3. **VirtualService** routes 100% of HTTP RPC traffic to EC2 with 100% mirror to `pacific-1-rpc-external.default.svc.cluster.local`. WebSocket traffic goes to EC2 only (Istio cannot mirror WebSocket).
4. **Telemetry** logs requests with response code >= 400 or latency > 5s. Mirrored requests arrive with Host header suffixed `-shadow`, making them filterable.
5. **PeerAuthentication** enforces STRICT mTLS on the K8s pods.

The cutover VirtualService (`virtual-service-cutover.yaml`) shows the progressive weight shifting from EC2 to K8s.

### Networking Configuration for Mirroring

The K8s RPC fleet needs:
- An external ClusterIP Service (created by the SeiNodeGroup networking config)
- Istio sidecar injection on the pods (the operator does not inject this -- the namespace must have `istio-injection: enabled`)
- The VirtualService, ServiceEntry, DestinationRule, and PeerAuthentication are manually managed (not created by the operator)

```yaml
# The SeiNodeGroup for the mirror target -- same as production
apiVersion: sei.io/v1alpha1
kind: SeiNodeGroup
metadata:
  name: pacific-1-rpc
  namespace: default
spec:
  replicas: 3
  template:
    metadata:
      labels:
        sei.io/role: rpc
    spec:
      chainId: pacific-1
      image: "ghcr.io/sei-protocol/sei:v6.3.0"
      # ... (same as production RPC spec)
      fullNode:
        snapshot:
          s3:
            targetHeight: 198740000
          trustPeriod: "9999h0m0s"

  networking:
    service:
      type: ClusterIP

    # AuthorizationPolicy allows the Istio gateway SA
    isolation:
      authorizationPolicy:
        allowedSources:
          - principals:
              - "cluster.local/ns/istio-system/sa/sei-gateway-istio"
          - namespaces:
              - default
```

The mirror VirtualService references `pacific-1-rpc-external.default.svc.cluster.local` -- the external Service name is deterministic (`{group-name}-external`).

### What Is Missing or Could Be Improved

1. **No operator-managed mirroring.** The Istio resources (VirtualService, ServiceEntry, DestinationRule) are manually applied. The operator could support a `networking.mirror` field that generates these resources, or at least a `networking.istio` section for VirtualService management.

2. **No result comparison integration.** The mirrored responses are discarded by Envoy. The comment in the VirtualService references a "result-compare task" on the sidecar, but this relies on the replayer node type. There is no built-in way to compare mirrored responses to the primary responses at the fleet level.

3. **WebSocket gap.** Istio cannot mirror WebSocket connections. During the mirror phase, WebSocket clients only hit the EC2 backend. The cutover VirtualService handles this with weighted routing, but there is no intermediate validation step for WebSocket traffic.

4. **No traffic recording/replay.** True replay (deterministic request replay from recorded production traffic) would require a request capture layer. The current mirroring is live -- it only works while production traffic is flowing.

---

## Summary: CRD Fields Mapped to Internal Use Cases

| CRD Field | Production Use | Internal Use |
|-----------|---------------|--------------|
| `networking.service.type: ClusterIP` | Backend for Gateway HTTPRoute | **Direct endpoint for load test jobs, integration tests, cross-namespace access** |
| `networking.service.type: LoadBalancer` | External IP for production traffic | Not needed internally -- ClusterIP suffices |
| `networking.gateway.baseDomain` | External DNS routing | **Omit entirely** -- no Gateway needed for internal workflows |
| `networking.isolation.authorizationPolicy` | Lock down pod access to Gateway SA | **Omit entirely** -- test namespaces are trusted |
| `monitoring.serviceMonitor` | Production Prometheus scraping | **Keep** -- useful for load test observability |
| Headless Service (per-node, automatic) | P2P networking, sidecar communication | **Direct node debugging, per-node load testing, side-by-side comparison** |
| `peers[].label.selector` | Cross-group peer discovery | **Compose multi-role test networks without hardcoded addresses** |
| `genesis` | N/A (production chains have existing genesis) | **Bootstrap private test chains from scratch** |
| `updateStrategy.type: HardFork` | Coordinate binary upgrades across production fleet | **Validate upgrade handlers in staging before production** |

## Summary: What Is Missing for Internal Developer Workflows

| Gap | Impact | Suggested Direction |
|-----|--------|-------------------|
| Cross-group dependency ordering | RPC groups applied before genesis completes must retry for up to 30 min | `dependsOn` field on SeiNodeGroup, or a Condition-based readiness gate |
| In-cluster genesis distribution | Test networks require S3 access for genesis sharing | ConfigMap or PVC-based genesis source as alternative to S3 |
| Halt-after-N-blocks for staging | Must manually calculate halt height for upgrade testing | `updateStrategy.hardFork.haltAfterBlocks` relative offset |
| Upgrade dry-run mode | Cannot validate upgrade compatibility without committing to the switch | `updateStrategy.dryRun: true` that creates entrants but skips switch |
| Service-level request metrics | No HTTP request metrics from the operator for load test analysis | Istio telemetry in the mesh, or a metrics sidecar on the external Service |
| Operator-managed Istio resources | VirtualService/DestinationRule for mirroring are manually applied | `networking.istio` section or separate CRD for traffic management |
| Load test lifecycle | Load test jobs are standalone, no coordination with group readiness | `SeiLoadTest` CRD or `loadTest` field that gates on group Ready |
| Debug aggregation | Must iterate headless Services manually to compare nodes | Debug endpoint or CLI plugin for multi-node queries |
