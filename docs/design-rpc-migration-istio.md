# Design: EC2-to-K8s RPC Node Migration via Istio Traffic Mirroring

## Overview

Progressive migration of Sei blockchain RPC infrastructure from EC2 to sei-k8s-controller-managed nodes using Istio traffic mirroring and weighted routing.

## Architecture

```
DNS: rpc.pacific-1.sei.io
         |
    AWS NLB (L4)
         |
  Istio IngressGateway
    (Envoy, in-mesh)
         |
  +------+------+
  |             |
[primary]   [mirror]
  |             |
ServiceEntry  K8s Service
(EC2 ALB)    (SeiNodeDeployment)
```

Replace ALB with NLB + Istio Gateway. The gateway terminates L7 and applies VirtualService routing. EC2 is the primary backend; K8s receives mirrored (fire-and-forget) traffic.

## Istio Manifests

All at `manifests/samples/istio/pacific-1-rpc-mirror/`:

- **`service-entry.yaml`** — EC2 ALB as `ec2-rpc.pacific-1.internal`, DNS resolution, ports 26657/8545/9090
- **`destination-rule.yaml`** — mTLS disabled to EC2 (outside mesh), HTTP/2 upgrade disabled (CometBFT is HTTP/1.1), outlier detection
- **`virtual-service.yaml`** — Phase 2 mirror config: 100% to EC2, 100% mirror to K8s. WebSocket routes to EC2 only (Istio cannot mirror WebSocket)
- **`virtual-service-cutover.yaml`** — Phase 3 template: weighted routing between EC2 and K8s
- **`peer-authentication.yaml`** — STRICT mTLS on K8s RPC pods
- **`telemetry.yaml`** — Access logging for error/latency analysis

## Migration Phases

### Phase 0: Isolated Validation (Week 1-2)
- Deploy SeiNodeDeployment for RPC, sync from S3 snapshot
- Enable `ExportAndCompare` with `canonicalRpc` pointing at EC2
- Run 48h with zero app-hash divergence (Layer 0 + Layer 1)
- Validate all alerts fire correctly
- **Blast radius: zero**

### Phase 1: Synthetic Load (Week 2)
- Replay recorded RPC queries against both EC2 and K8s
- Compare responses byte-for-byte (normalize node ID, peer list)
- Deploy synthetic WebSocket client subscribing to NewBlock on both
- **Gate: 100% response parity, latency within 20%**

### Phase 2: Shadow Traffic (Week 3)
- Point DNS at NLB, VirtualService mirrors 100% to K8s
- Responses discarded — clients only see EC2
- Monitor: mirror acceptance rate, K8s error rate, latency delta, block height lag
- **Gate: 48h clean metrics**
- **Rollback: DNS back to ALB (60s)**

### Phase 3: Canary (Week 4)
- Switch from mirror to weighted routing: 1% → 5% → 10% → 25% → 50%
- Hold 4h minimum at each step, overnight at 50%
- Exclude `/broadcast_tx*` from early canary (add after 10%)
- **Gate: 24h at 50% with no degradation**
- **Rollback: Set K8s weight to 0 (seconds)**

### Phase 4: Full Cutover (Week 5)
- 100% to K8s, EC2 hot standby
- Keep EC2 syncing for 48h
- **Rollback: Set EC2 weight to 100 (seconds)**

### Phase 5: Decommission (Week 6)
- Remove ServiceEntry, comparison CronJob
- Decommission EC2 instances
- Optionally simplify ingress (remove Istio gateway if not needed long-term)

## Key Design Decisions

### Istio route weights over DNS
DNS caching by blockchain clients makes Route53 splits non-deterministic. Istio applies weights per-request at the proxy, with immediate propagation and rollback in seconds.

### WebSocket handled separately
Istio cannot mirror WebSocket (persistent bidirectional stream). HTTP RPC is mirrored; WebSocket gets weighted routing during cutover. The sidecar `ExportAndCompare` validates execution correctness independently.

### STRICT mTLS on K8s pods
EC2 traffic enters through the ingress gateway (which terminates external TLS and originates mTLS). No reason for non-mTLS traffic to reach K8s pods directly.

### ExportAndCompare is the correctness oracle
Istio mirroring provides realistic query load. But execution correctness is validated by the sidecar's block-by-block Layer 0/Layer 1 comparison, which uploads DivergenceReport artifacts to S3. Access logs and metrics are supporting signals, not source of truth.

### Do not mirror /broadcast_tx
Mirroring write endpoints would double-broadcast transactions. Mempool dedup handles it, but it wastes resources and creates confusing logs.

## Confidence Criteria

| Category | Signal | Pass |
|----------|--------|------|
| Chain correctness | App-hash agreement (L0+L1) | Zero divergences over 10k blocks |
| Chain correctness | Block height parity | Within 2 blocks of EC2 |
| Performance | RPC latency p99 | Within 20% of EC2 |
| Operations | Automated pod recovery | Recovers in < 5 min |
| Operations | Blue-green deployment | Works without manual steps |
| Data plane | Gateway healthy | ConditionRouteReady == True |

## Prerequisites Checklist

- [ ] SeiNodeDeployment for RPC deployed, all nodes synced (`catching_up: false`)
- [ ] Istio sidecar injection enabled on RPC namespace
- [ ] Gateway + ServiceEntry deployed, reachable from mesh
- [ ] ExportAndCompare running 48h with zero divergence
- [ ] Monitoring: block height lag alert, comparison divergence alert, gateway error rate alert
- [ ] Dashboard: chain health + traffic + operator health
- [ ] DNS TTL lowered to 60s
- [ ] Rollback procedure documented and rehearsed
- [ ] On-call briefed on the migration

## Timeline

6 weeks (compressible to 4). Do not compress below 4 — the canary ramp alone needs 5-7 days.
