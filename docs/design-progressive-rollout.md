# Design: Automated Progressive Rollout — EC2 to K8s RPC Migration

## Overview

Automated, zero-manual-intervention migration of Sei RPC traffic from EC2 to Kubernetes using Route53 weighted routing, in-cluster load generation, and a confidence-score-driven progression loop.

### Architecture (simplified from previous Istio-centric design)

```
DNS: rpc.pacific-1.sei.io
         |
   Route53 weighted record set
    /                    \
EC2 ALB                 K8s NLB
(weight: W_ec2)         (weight: W_k8s)
   |                       |
EC2 RPC nodes         Istio IngressGateway
                           |
                      K8s Service
                      (SeiNodeGroup)
```

Key simplification: Istio fronts K8s only. It does not sit in the EC2 path. Traffic splitting is done at DNS level via Route53 weighted record sets. This avoids ServiceEntry complexity, mTLS termination issues to EC2, and keeps the EC2 path completely unchanged during migration.

### Trade-offs vs. Istio-only weights

| Factor | Route53 weighted | Istio VirtualService |
|--------|-----------------|---------------------|
| EC2 path impact | None | Must route through mesh |
| Rollback speed | ~60s (DNS TTL) | ~1s (Envoy push) |
| Split granularity | Per-DNS-resolution | Per-request |
| Client caching | Some clients cache DNS | No client caching |
| Complexity | Low (aws CLI) | Medium (ServiceEntry + DestinationRule) |

For blockchain RPC clients, DNS caching is a real concern. Mitigations: (1) set TTL to 10s on the weighted records, (2) the progression holds at each step for hours, so transient DNS caching does not affect steady-state measurements. The simplicity wins.

---

## 1. Automated Weight Progression

### Tool choice: CronJob + shell script (not a custom controller)

A custom Go controller is overkill for a one-time migration. Argo Rollouts and Flagger both assume they own the rollout object (Deployment/Rollout) and are designed for in-cluster traffic splitting, not Route53 manipulation. The right tool is a Kubernetes CronJob running a shell script that:

1. Queries Prometheus for the confidence score
2. Evaluates gate conditions
3. Calls `aws route53 change-resource-record-sets` to adjust weights
4. Posts status to a Slack webhook

This runs as a CronJob with `schedule: "*/5 * * * *"` (every 5 minutes). The script is idempotent: if conditions are not met, it does nothing. If conditions are met and the current weight is below the next step, it advances.

### Implementation

Container image: Alpine + `aws-cli` + `curl` + `jq`. No custom Go code.

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: rpc-migration-controller
  namespace: sei-infra
spec:
  schedule: "*/5 * * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      backoffLimit: 0
      template:
        spec:
          serviceAccountName: rpc-migration-controller
          containers:
          - name: controller
            image: amazon/aws-cli:2.15
            command: ["/bin/bash", "/scripts/progress.sh"]
            env:
            - name: PROMETHEUS_URL
              value: "http://prometheus.monitoring:9090"
            - name: HOSTED_ZONE_ID
              value: "Z0123456789ABCDEF"
            - name: RECORD_NAME
              value: "rpc.pacific-1.sei.io"
            - name: EC2_ALB_DNS
              value: "ec2-rpc-alb-123456.us-east-1.elb.amazonaws.com"
            - name: K8S_NLB_DNS
              value: "k8s-rpc-nlb-789012.us-east-1.elb.amazonaws.com"
            - name: SLACK_WEBHOOK_URL
              valueFrom:
                secretKeyRef:
                  name: rpc-migration-secrets
                  key: slack-webhook-url
            - name: WEIGHT_STEPS
              value: "0,1,10,50,100"
            - name: MIN_HOLD_MINUTES
              value: "240"  # 4 hours at each step
            - name: ROLLBACK_THRESHOLD
              value: "40"   # confidence score below this triggers rollback
            volumeMounts:
            - name: scripts
              mountPath: /scripts
          volumes:
          - name: scripts
            configMap:
              name: rpc-migration-scripts
          restartPolicy: Never
```

### The progression script (`progress.sh`)

Core logic (pseudocode — the real script is straightforward bash):

```bash
#!/bin/bash
set -euo pipefail

STEPS=(${WEIGHT_STEPS//,/ })
CURRENT_K8S_WEIGHT=$(get_current_route53_weight "k8s")
CONFIDENCE=$(query_prometheus_confidence_score)
LAST_CHANGE_TIME=$(get_annotation_from_configmap "last-weight-change")
MINUTES_AT_CURRENT=$(minutes_since "$LAST_CHANGE_TIME")

# Rollback check — runs before progression
if (( CONFIDENCE < ROLLBACK_THRESHOLD )) && (( CURRENT_K8S_WEIGHT > 0 )); then
    previous_step=$(find_previous_step "$CURRENT_K8S_WEIGHT")
    set_route53_weight "$previous_step"
    notify_slack ":rotating_light: ROLLBACK: confidence=$CONFIDENCE, weight $CURRENT_K8S_WEIGHT -> $previous_step"
    exit 0
fi

# Progression check
if (( MINUTES_AT_CURRENT < MIN_HOLD_MINUTES )); then
    echo "Holding at weight=$CURRENT_K8S_WEIGHT for $MINUTES_AT_CURRENT/$MIN_HOLD_MINUTES minutes"
    exit 0
fi

next_step=$(find_next_step "$CURRENT_K8S_WEIGHT")
if [[ -z "$next_step" ]]; then
    echo "Already at final weight. Migration complete."
    exit 0
fi

# Gate: confidence must be above threshold for progression
if (( CONFIDENCE >= 80 )); then
    set_route53_weight "$next_step"
    record_change_time
    notify_slack ":white_check_mark: PROGRESS: confidence=$CONFIDENCE, weight $CURRENT_K8S_WEIGHT -> $next_step"
else
    echo "Confidence=$CONFIDENCE below 80, holding at weight=$CURRENT_K8S_WEIGHT"
fi
```

State is stored in a ConfigMap (`rpc-migration-state`) with keys:
- `current-k8s-weight`: redundant with Route53 but avoids API calls for reads
- `last-weight-change`: ISO 8601 timestamp
- `rollback-count`: number of rollbacks (alarm if > 2)

### Route53 weight mechanics

Route53 weighted records use relative weights, not percentages. To achieve "1% K8s":

| Step | K8s weight | EC2 weight | Effective K8s % |
|------|-----------|-----------|----------------|
| 0%   | 0         | 100       | 0%             |
| 1%   | 1         | 99        | ~1%            |
| 10%  | 10        | 90        | ~10%           |
| 50%  | 50        | 50        | 50%            |
| 100% | 100       | 0         | 100%           |

TTL on both records: 10 seconds. This is the minimum practical TTL for Route53 and ensures DNS resolvers pick up weight changes within seconds.

### Timing and cadence

| Step | Min hold | Rationale |
|------|----------|-----------|
| 0% -> 1% | Requires passing load test (see section 2) | First real traffic |
| 1% -> 10% | 4 hours | Detect issues at low blast radius |
| 10% -> 50% | 4 hours | Significant traffic, covers edge cases |
| 50% -> 100% | 12 hours (overnight) | Full confidence before cutover |

The 4-hour hold is configurable via `MIN_HOLD_MINUTES`. The 50% -> 100% step uses a longer hold, implemented as a special case in the script (check if current step is 50, use 720 minutes).

---

## 2. Load Generation for 0% Phase

### Tool choice: k6

k6 over vegeta or custom Go:
- Native JavaScript scripting for complex RPC query patterns
- Built-in Prometheus remote write (metrics go straight to our stack)
- Thresholds that can fail the test programmatically
- Runs as a Kubernetes Job, no persistent infrastructure
- Handles ramping, stages, and per-endpoint breakdowns natively

### Deriving traffic patterns from EC2 ALB access logs

Before writing k6 scripts, extract the real query distribution:

```bash
# Enable ALB access logging to S3 if not already enabled
# Then analyze the logs:

# 1. Download recent access logs (24h sample)
aws s3 sync s3://sei-infra-alb-logs/AWSLogs/.../elasticloadbalancing/ ./alb-logs/ \
  --exclude "*" --include "*.log.gz"

# 2. Extract RPC method distribution
zcat alb-logs/*.gz | \
  awk -F'"' '{print $2}' | \        # extract request field
  grep -oP '(GET|POST) [^ ]+' | \   # method + path
  sort | uniq -c | sort -rn | head -30

# 3. For JSON-RPC POST bodies, enable ALB request logging or
#    sample from application-level logs on EC2 nodes.
#    CometBFT logs the method in its access log.
```

Expected distribution for a Sei RPC node (typical from sei-infra patterns):

| Method | Approx % | Type |
|--------|---------|------|
| `abci_query` (bank balances, wasm state) | 35% | POST JSON-RPC |
| `block` / `block_results` | 20% | GET or POST |
| `tx_search` | 15% | GET |
| `status` | 10% | GET |
| `eth_call` (EVM JSON-RPC) | 8% | POST JSON-RPC on :8545 |
| `eth_getBlockByNumber` | 5% | POST JSON-RPC on :8545 |
| `broadcast_tx_sync` | 3% | POST (skip in load test) |
| `validators` / `consensus_state` | 2% | GET |
| Other | 2% | Mixed |

### k6 load test script

```javascript
// k6-rpc-load-test.js
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';

const RPC_URL = __ENV.RPC_URL || 'http://k8s-rpc-svc.sei.svc.cluster.local:26657';
const EVM_URL = __ENV.EVM_URL || 'http://k8s-rpc-svc.sei.svc.cluster.local:8545';

const errorCount = new Counter('rpc_errors');
const blockHeightLag = new Trend('block_height_lag');

// Ramping profile: warm up over 10 min, hold at target for 1h, cool down
export const options = {
  stages: [
    { duration: '5m',  target: 50 },   // warm up
    { duration: '10m', target: 200 },   // ramp to target
    { duration: '60m', target: 200 },   // sustained load
    { duration: '5m',  target: 0 },     // cool down
  ],
  thresholds: {
    'http_req_failed': ['rate<0.01'],        // <1% errors
    'http_req_duration{method:status}': ['p(99)<500'],  // p99 < 500ms
    'http_req_duration{method:abci_query}': ['p(99)<2000'],
    'rpc_errors': ['count<50'],
  },
};

// Weighted method selection matching real traffic distribution
const methods = [
  { weight: 35, fn: abciQuery },
  { weight: 20, fn: blockQuery },
  { weight: 15, fn: txSearch },
  { weight: 10, fn: statusQuery },
  { weight: 8,  fn: ethCall },
  { weight: 5,  fn: ethGetBlock },
  { weight: 5,  fn: validatorsQuery },
  // broadcast_tx intentionally excluded
];

const totalWeight = methods.reduce((sum, m) => sum + m.weight, 0);

export default function () {
  const rand = Math.random() * totalWeight;
  let cumulative = 0;
  for (const m of methods) {
    cumulative += m.weight;
    if (rand < cumulative) {
      m.fn();
      break;
    }
  }
  sleep(0.1); // 100ms think time
}

function statusQuery() {
  const res = http.get(`${RPC_URL}/status`, { tags: { method: 'status' } });
  check(res, { 'status 200': (r) => r.status === 200 });
  if (res.status !== 200) errorCount.add(1);

  // Track block height lag vs EC2
  if (res.status === 200) {
    try {
      const height = parseInt(res.json().result.sync_info.latest_block_height);
      // EC2 height fetched once per VU iteration via setup()
      blockHeightLag.add(Math.abs(height - globalThis.ec2Height));
    } catch (e) { /* ignore parse errors */ }
  }
}

function abciQuery() {
  const payload = JSON.stringify({
    jsonrpc: '2.0', id: 1, method: 'abci_query',
    params: {
      path: '/cosmos.bank.v1beta1.Query/AllBalances',
      data: '',  // empty query = recent state
      height: '0', prove: false,
    },
  });
  const res = http.post(RPC_URL, payload, {
    headers: { 'Content-Type': 'application/json' },
    tags: { method: 'abci_query' },
  });
  if (res.status !== 200) errorCount.add(1);
}

function blockQuery() {
  const res = http.get(`${RPC_URL}/block`, { tags: { method: 'block' } });
  if (res.status !== 200) errorCount.add(1);
}

function txSearch() {
  // Search for recent transactions (last 100 blocks)
  const res = http.get(
    `${RPC_URL}/tx_search?query="tx.height>0"&per_page=10&page=1&order_by="desc"`,
    { tags: { method: 'tx_search' } }
  );
  if (res.status !== 200) errorCount.add(1);
}

function ethCall() {
  const payload = JSON.stringify({
    jsonrpc: '2.0', id: 1, method: 'eth_call',
    params: [{ to: '0x0000000000000000000000000000000000001002', data: '0x' }, 'latest'],
  });
  const res = http.post(EVM_URL, payload, {
    headers: { 'Content-Type': 'application/json' },
    tags: { method: 'eth_call' },
  });
  if (res.status !== 200) errorCount.add(1);
}

function ethGetBlock() {
  const payload = JSON.stringify({
    jsonrpc: '2.0', id: 1, method: 'eth_getBlockByNumber',
    params: ['latest', false],
  });
  const res = http.post(EVM_URL, payload, {
    headers: { 'Content-Type': 'application/json' },
    tags: { method: 'eth_getBlockByNumber' },
  });
  if (res.status !== 200) errorCount.add(1);
}

function validatorsQuery() {
  const res = http.get(`${RPC_URL}/validators`, { tags: { method: 'validators' } });
  if (res.status !== 200) errorCount.add(1);
}
```

### k6 Kubernetes Job

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: rpc-load-test
  namespace: sei-infra
spec:
  backoffLimit: 0
  template:
    spec:
      containers:
      - name: k6
        image: grafana/k6:0.49.0
        command: ["k6", "run", "--out", "experimental-prometheus-rw", "/scripts/load-test.js"]
        env:
        - name: K6_PROMETHEUS_RW_SERVER_URL
          value: "http://prometheus.monitoring:9090/api/v1/write"
        - name: K6_PROMETHEUS_RW_TREND_AS_NATIVE_HISTOGRAM
          value: "true"
        - name: RPC_URL
          value: "http://sei-rpc-pacific-1.sei:26657"
        - name: EVM_URL
          value: "http://sei-rpc-pacific-1.sei:8545"
        volumeMounts:
        - name: scripts
          mountPath: /scripts
      volumes:
      - name: scripts
        configMap:
          name: k6-rpc-load-test
      restartPolicy: Never
```

### Duration and intensity

| Parameter | Value | Rationale |
|-----------|-------|-----------|
| Ramp-up | 15 minutes to 200 VUs | Avoid cold-start spike |
| Sustained | 60 minutes at 200 VUs | ~2000 req/s, matches EC2 ALB peak |
| Pass criteria | All k6 thresholds green | Automated gate |
| Runs required | 3 consecutive passes | Eliminate flakes |

The target RPS (200 VUs * ~10 req/s/VU = 2000 req/s) should match or exceed the p95 traffic level observed on the EC2 ALB. Adjust VU count based on actual ALB CloudWatch `RequestCount` metrics.

---

## 3. Observability During Rollout

### Metrics sources

| Metric | K8s source | EC2 source |
|--------|-----------|-----------|
| Request rate | Istio `istio_requests_total` | CloudWatch ALB `RequestCount` |
| Error rate | Istio `istio_requests_total{response_code=~"5.."}` | CloudWatch ALB `HTTPCode_Target_5XX_Count` |
| Latency p50/p99 | Istio `istio_request_duration_milliseconds` | CloudWatch ALB `TargetResponseTime` |
| Block height | Prometheus scraping K8s seid `:26657/status` | CloudWatch custom metric or Prometheus remote-write agent on EC2 |
| Pod health | `sei_controller_seinode_phase` | N/A |

### EC2 metrics strategy

Two options, prefer option A for simplicity:

**Option A: CloudWatch only (recommended)**

EC2 ALB already publishes metrics to CloudWatch. Use the `yet-another-cloudwatch-exporter` (YACE) running in K8s to scrape CloudWatch metrics into Prometheus. This avoids touching EC2 infrastructure.

```yaml
# yace config for EC2 ALB metrics
discovery:
  jobs:
  - type: AWS/ApplicationELB
    regions: [us-east-1]
    searchTags:
    - key: Name
      value: sei-rpc-*
    metrics:
    - name: RequestCount
      statistics: [Sum]
      period: 60
    - name: TargetResponseTime
      statistics: [p50, p99]
      period: 60
    - name: HTTPCode_Target_5XX_Count
      statistics: [Sum]
      period: 60
```

**Option B: Prometheus agent on EC2**

Run `prometheus-agent` on one EC2 node, remote-write to the K8s Prometheus. More accurate latency data but requires EC2 changes. Only do this if CloudWatch TargetResponseTime granularity is insufficient.

### Block height comparison

The existing controller already uses CometBFT `/status` to track `latest_block_height` and `catching_up`. For the migration dashboard, add a recording rule that computes lag:

```yaml
groups:
- name: rpc-migration
  rules:
  # K8s block height (from seid metrics or ServiceMonitor scrape)
  - record: sei:rpc:block_height:k8s
    expr: max(cometbft_consensus_latest_block_height{namespace="sei"})

  # EC2 block height (scraped via YACE custom metric or a simple curl probe)
  - record: sei:rpc:block_height:ec2
    expr: sei_ec2_rpc_block_height

  - record: sei:rpc:block_height_lag
    expr: abs(sei:rpc:block_height:k8s - sei:rpc:block_height:ec2)
```

For the EC2 block height, deploy a simple CronJob that curls the EC2 ALB `/status` endpoint every 15s and pushes the height to Prometheus via pushgateway or a `/metrics` endpoint:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: ec2-height-probe
  namespace: sei-infra
spec:
  schedule: "* * * * *"  # every minute (finest CronJob granularity)
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: probe
            image: curlimages/curl:8.7.1
            command:
            - /bin/sh
            - -c
            - |
              HEIGHT=$(curl -sf http://EC2_ALB_DNS:26657/status | \
                jq -r '.result.sync_info.latest_block_height')
              curl -sf -X POST "http://pushgateway.monitoring:9091/metrics/job/ec2-height-probe" \
                --data-binary "sei_ec2_rpc_block_height $HEIGHT"
          restartPolicy: Never
```

Better alternative: use a Prometheus Blackbox Exporter probe target that hits the EC2 ALB `/status` and parses height. Configure as a ServiceMonitor probe.

### Dashboard layout (Grafana)

Single dashboard: **"RPC Migration: EC2 vs K8s"**

```
Row 1: Migration Status
  [Current Weight: 10% K8s] [Confidence Score: 87] [Time at Step: 2h 15m] [Rollback Count: 0]

Row 2: Traffic Split (stacked time series)
  [Requests/sec — EC2 vs K8s, stacked area]  [Error Rate — EC2 vs K8s, line]

Row 3: Latency Comparison
  [p50 Latency — EC2 vs K8s, line]  [p99 Latency — EC2 vs K8s, line]

Row 4: Chain Health
  [Block Height — EC2 vs K8s, line]  [Block Height Lag, line with threshold at 5]

Row 5: K8s Platform Health
  [SeiNode Phase, state timeline]  [Pod Restarts, bar]  [Istio 5xx Rate, line]

Row 6: Load Test Results (only during 0% phase)
  [k6 RPS, line]  [k6 Error Rate, line]  [k6 p99 Latency, line]
```

The dashboard JSON will be stored in a ConfigMap and auto-provisioned via Grafana's sidecar provisioner.

---

## 4. The Confidence Score

### Definition

A single number from 0 to 100 representing migration readiness. Computed every 5 minutes as a Prometheus recording rule. The progression script queries this single metric.

### Formula

```
confidence = w_uptime * S_uptime
           + w_error  * S_error
           + w_latency * S_latency
           + w_height * S_height
           + w_load   * S_load
```

Where each sub-score is 0-100 and weights sum to 1.0:

| Component | Weight | Score function | 100 (perfect) | 0 (fail) |
|-----------|--------|---------------|----------------|----------|
| `S_uptime` | 0.20 | K8s pod uptime over last 1h | 100% uptime | <95% uptime |
| `S_error` | 0.30 | Error rate comparison | K8s error rate <= EC2 | K8s error rate > 5x EC2 |
| `S_latency` | 0.25 | p99 latency parity | K8s p99 <= 1.1x EC2 | K8s p99 > 2x EC2 |
| `S_height` | 0.15 | Block height parity | Lag <= 1 block | Lag > 10 blocks |
| `S_load` | 0.10 | Load test pass (0% phase only) | All thresholds pass | Any threshold fail |

Error rate gets the highest weight because RPC correctness is the primary concern. Latency is second because blockchain clients are sensitive to query timeouts.

### Prometheus recording rules

```yaml
groups:
- name: rpc-migration-confidence
  interval: 60s
  rules:
  # Sub-score: uptime (fraction of time pods were Ready in last 1h)
  - record: sei:migration:score:uptime
    expr: |
      clamp(
        100 * (
          1 - (sum(rate(kube_pod_status_ready{namespace="sei",condition="false"}[1h]))
               / max(sum(kube_pod_status_ready{namespace="sei"}), 1))
        ),
        0, 100
      )

  # Sub-score: error rate
  # 100 when K8s error rate <= EC2, linear decay to 0 when K8s >= 5x EC2
  - record: sei:migration:score:error_rate
    expr: |
      clamp(
        100 * (1 - clamp_min(
          (
            sum(rate(istio_requests_total{reporter="destination",namespace="sei",response_code=~"5.."}[10m]))
            / clamp_min(sum(rate(istio_requests_total{reporter="destination",namespace="sei"}[10m])), 0.001)
          )
          /
          clamp_min(
            (sei:ec2:error_rate_5xx OR on() vector(0.001)),
            0.001
          )
          - 1, 0
        ) / 4),
        0, 100
      )

  # Sub-score: latency parity
  # 100 when K8s p99 <= 1.1x EC2, linear decay to 0 when >= 2x EC2
  - record: sei:migration:score:latency
    expr: |
      clamp(
        100 * (1 - clamp_min(
          histogram_quantile(0.99, sum(rate(istio_request_duration_milliseconds_bucket{reporter="destination",namespace="sei"}[10m])) by (le))
          / clamp_min(sei:ec2:latency_p99_ms, 1)
          - 1.1, 0
        ) / 0.9),
        0, 100
      )

  # Sub-score: block height parity
  # 100 when lag <= 1, linear decay to 0 when lag >= 10
  - record: sei:migration:score:block_height
    expr: |
      clamp(
        100 * (1 - clamp_min(sei:rpc:block_height_lag - 1, 0) / 9),
        0, 100
      )

  # Sub-score: load test (set by k6 job completion)
  # Stored in pushgateway: 100 if last test passed, 0 if failed
  - record: sei:migration:score:load_test
    expr: sei_migration_load_test_score OR on() vector(0)

  # Composite confidence score
  - record: sei:migration:confidence_score
    expr: |
      0.20 * sei:migration:score:uptime
      + 0.30 * sei:migration:score:error_rate
      + 0.25 * sei:migration:score:latency
      + 0.15 * sei:migration:score:block_height
      + 0.10 * sei:migration:score:load_test
```

### Score interpretation

| Range | Meaning | Automation action |
|-------|---------|-------------------|
| 80-100 | Green: all signals nominal | Advance to next weight step |
| 60-79 | Yellow: minor degradation | Hold at current step, alert to Slack |
| 40-59 | Orange: significant issues | Hold, page on-call |
| 0-39 | Red: rollback conditions | Automatic rollback to previous step |

### Rollback conditions (any one triggers)

These are evaluated independently from the composite score as hard circuit breakers:

1. **Error rate spike**: K8s 5xx rate > 5% for 5 consecutive minutes
2. **Latency regression**: K8s p99 > 3x EC2 p99 for 10 consecutive minutes
3. **Block height stall**: K8s block height not advancing for 2 minutes
4. **Pod failure**: SeiNodeGroup phase == Degraded or Failed

The progression script checks these independently:

```bash
check_circuit_breakers() {
    # Hard error rate check
    error_rate=$(promql 'sum(rate(istio_requests_total{namespace="sei",response_code=~"5.."}[5m])) / sum(rate(istio_requests_total{namespace="sei"}[5m]))')
    if (( $(echo "$error_rate > 0.05" | bc -l) )); then
        echo "BREAKER: error_rate=$error_rate > 5%"
        return 1
    fi

    # Block height stall
    height_change=$(promql 'changes(cometbft_consensus_latest_block_height{namespace="sei"}[2m])')
    if (( $(echo "$height_change < 1" | bc -l) )); then
        echo "BREAKER: block height stalled"
        return 1
    fi

    # SeiNodeGroup health
    group_phase=$(kubectl get sng -n sei -o jsonpath='{.items[0].status.phase}')
    if [[ "$group_phase" == "Failed" || "$group_phase" == "Degraded" ]]; then
        echo "BREAKER: SeiNodeGroup phase=$group_phase"
        return 1
    fi

    return 0
}
```

---

## 5. Timeline

### Prerequisites (before clock starts)

- SeiNodeGroup deployed, all nodes synced (`catching_up: false`)
- Istio sidecar injection on, Gateway + HTTPRoute working
- ServiceMonitor scraping K8s nodes
- YACE exporting EC2 ALB CloudWatch metrics
- EC2 block height probe running
- Grafana dashboard provisioned
- Confidence score recording rules deployed
- CronJob + scripts deployed (weight at 0%)

### Automated progression timeline

| Day | Phase | K8s Weight | Activity |
|-----|-------|-----------|----------|
| 0 | Load test | 0% | k6 Job runs 3x against K8s-internal endpoint. Automated: passes or fails. |
| 0-1 | Soak at 0% | 0% | Confidence score baking. ExportAndCompare running. Score must reach 80+. |
| 1 | First traffic | 1% | Automation advances when score >= 80 and load tests pass. Hold 4h. |
| 1-2 | Low canary | 10% | Automation advances. Hold 4h. |
| 2-3 | Mid canary | 50% | Hold overnight (12h minimum). |
| 3-4 | Full cutover | 100% | EC2 weight = 0. EC2 stays running as hot standby. |
| 4-7 | Hot standby | 100% | EC2 keeps syncing. Manual decommission after 72h of 100% K8s. |
| 7 | Decommission | 100% | Remove Route53 weighted records, switch to simple alias. Terminate EC2. |

**Total: 7 days from "nodes synced" to "EC2 decommissioned".**

This compresses the original 6-week plan to 1 week because:
1. No manual gate reviews (automation drives progression)
2. No shadow traffic phase (k6 load test replaces Istio mirroring)
3. Confidence score eliminates waiting for human judgment
4. Route53 is operationally simpler than Istio VirtualService weight management

The 7-day timeline can extend automatically if the confidence score drops below threshold at any step. The automation holds until conditions improve, with no human intervention needed.

### When human intervention IS required

The system pages on-call (PagerDuty) for:
1. More than 2 rollbacks at the same weight step (potential systemic issue)
2. Confidence score below 40 for more than 1 hour
3. Complete K8s cluster failure (all pods down)

These scenarios indicate problems that a weight-shifting automation cannot fix.

---

## 6. File Layout

```
manifests/samples/migration/
  cronjob.yaml                    # Migration controller CronJob
  configmap-scripts.yaml          # progress.sh and helper functions
  configmap-state.yaml            # Mutable state (current weight, timestamps)
  rbac.yaml                       # ServiceAccount + IAM role for Route53
  k6-load-test-job.yaml           # Load test Job template
  k6-load-test-configmap.yaml     # k6 script
  ec2-height-probe.yaml           # Block height comparison CronJob
  yace-config.yaml                # CloudWatch exporter for EC2 ALB metrics
  prometheus-rules.yaml           # Recording rules for confidence score
  grafana-dashboard-configmap.yaml # Migration dashboard JSON
```

No custom Go code. No new CRDs. No Argo/Flagger dependencies. The entire system is:
- 1 CronJob (progression controller)
- 1 Job (k6 load test, run on demand)
- 1 CronJob (EC2 height probe)
- 1 Deployment (YACE, likely already running)
- Recording rules + dashboard (declarative config)

---

## 7. Relation to Existing Infrastructure

### What changes in sei-k8s-controller: nothing

The controller already provisions:
- `SeiNodeGroup` with `networking.service` (type: LoadBalancer) -- this creates the K8s NLB
- `networking.gateway` with HTTPRoute -- Istio routes internal mesh traffic
- `monitoring.serviceMonitor` -- Prometheus scrapes K8s nodes
- `NetworkingStatus.LoadBalancerIngress` -- reports the NLB address

The migration automation reads these outputs but does not modify the controller. The Route53 records are managed externally by the CronJob, pointing at the NLB address from `NetworkingStatus.LoadBalancerIngress`.

### What changes in EC2: nothing

EC2 ALB continues serving traffic at its current DNS name. Route53 weighted records sit above both the ALB and NLB. EC2 infrastructure is untouched until decommission day.

### What changes in DNS

Before migration:
```
rpc.pacific-1.sei.io  ->  ALIAS  ->  EC2 ALB
```

During migration:
```
rpc.pacific-1.sei.io  ->  WEIGHTED (TTL=10s)
                            ├── SetId=ec2, Weight=W, ALIAS -> EC2 ALB
                            └── SetId=k8s, Weight=W, ALIAS -> K8s NLB
```

After migration:
```
rpc.pacific-1.sei.io  ->  ALIAS  ->  K8s NLB
```
