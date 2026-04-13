# Blackbox Endpoint Monitoring

**Status:** Draft
**Date:** 2026-04-12
**Scope:** Platform -- Monitoring, Alerting, PagerDuty

---

## Problem

`grafana.prod.platform.sei.io` went unreachable due to a DNS delegation issue. We had no alert for it. Our monitoring stack (kube-prometheus-stack) watches internal service health -- it does not probe the public endpoints that users actually hit. DNS resolution failures, TLS expiry, and HTTP routing misconfigurations are invisible until someone notices manually.

## Solution

Deploy `prometheus-blackbox-exporter` as a separate HelmRelease in the monitoring namespace. Use the Prometheus Operator `Probe` CRD to define target endpoints, and a `PrometheusRule` to alert when probes fail. Routes to `pagerduty-platform` via the existing `team: platform` label matcher.

---

## Architecture

```
Prometheus (kube-prometheus-stack)
    |
    | scrapes Probe targets via blackbox exporter
    |
blackbox-exporter (monitoring namespace, port 9115)
    |
    | HTTP requests to public endpoints
    |
sei-gateway (NLB, TLS termination)
    |
    +-- grafana.prod.platform.sei.io --> Grafana
    +-- grafana.pacific-1.seinetwork.io --> 301 redirect
    +-- grafana.atlantic-2.seinetwork.io --> 301 redirect
    +-- grafana.arctic-1.seinetwork.io --> 301 redirect
```

The blackbox exporter is a stateless HTTP probe runner. Prometheus scrapes it by passing the target URL as a query parameter. The `Probe` CRD automates this -- prometheus-operator translates `Probe` resources into the correct scrape config targeting the blackbox exporter's `/probe` endpoint.

No new Prometheus instance. No new ServiceMonitor plumbing. The existing kube-prometheus-stack Prometheus discovers `Probe` and `PrometheusRule` resources via label selectors already configured (it uses `release: sei-prod` or equivalent; the new resources carry matching labels).

---

## Probe Configuration

### Blackbox Exporter Modules

Configured in the HelmRelease values. Two modules are sufficient:

| Module | Purpose | Config |
|--------|---------|--------|
| `http_2xx` | Validates endpoint returns 200, TLS is valid, DNS resolves | `prober: http`, `method: GET`, `fail_if_ssl: false`, `preferred_ip_protocol: ip4` |
| `http_301` | Validates legacy domains redirect (301/302) | `prober: http`, `method: GET`, `valid_status_codes: [301, 302]`, `no_follow_redirects: true` |

The `http_2xx` module performs the full chain: DNS resolution, TCP connect, TLS handshake, HTTP request, status code validation. The `probe_dns_lookup_time_seconds`, `probe_ssl_earliest_cert_expiry`, and `probe_success` metrics come automatically.

### Probe Resources

**Primary endpoint probe:**

```yaml
apiVersion: monitoring.coreos.com/v1
kind: Probe
metadata:
  name: grafana-prod
  namespace: monitoring
  labels:
    release: sei-prod
spec:
  interval: 60s
  module: http_2xx
  prober:
    url: blackbox-exporter-prometheus-blackbox-exporter.monitoring:9115
  targets:
    staticConfig:
      static:
        - https://grafana.prod.platform.sei.io
      labels:
        probe_group: platform-endpoints
```

**Legacy redirect probe:**

```yaml
apiVersion: monitoring.coreos.com/v1
kind: Probe
metadata:
  name: grafana-legacy-redirects
  namespace: monitoring
  labels:
    release: sei-prod
spec:
  interval: 120s
  module: http_301
  prober:
    url: blackbox-exporter-prometheus-blackbox-exporter.monitoring:9115
  targets:
    staticConfig:
      static:
        - https://grafana.pacific-1.seinetwork.io
        - https://grafana.atlantic-2.seinetwork.io
        - https://grafana.arctic-1.seinetwork.io
      labels:
        probe_group: legacy-redirects
```

### Scrape Intervals

- Primary endpoints: 60s. Frequent enough to catch issues within 2-3 minutes (with a `for: 2m` alert), infrequent enough to not be noisy.
- Legacy redirects: 120s. These are lower priority -- a broken redirect is annoying but not an outage.

---

## Alert Rules

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: blackbox-endpoint-alerts
  namespace: monitoring
  labels:
    release: sei-prod
    team: platform
spec:
  groups:
    - name: blackbox-endpoints
      rules:
        - alert: EndpointDown
          expr: probe_success{probe_group="platform-endpoints"} == 0
          for: 2m
          labels:
            severity: critical
            team: platform
          annotations:
            summary: "{{ $labels.instance }} is unreachable"
            description: >-
              Blackbox probe to {{ $labels.instance }} has been failing for 2 minutes.
              This could indicate DNS, TLS, or HTTP routing failure.
            runbook_url: https://wiki.sei.io/platform/runbooks/endpoint-down

        - alert: EndpointSSLCertExpiringSoon
          expr: probe_ssl_earliest_cert_expiry{probe_group="platform-endpoints"} - time() < 7 * 24 * 3600
          for: 10m
          labels:
            severity: warning
            team: platform
          annotations:
            summary: "TLS cert for {{ $labels.instance }} expires in {{ $value | humanizeDuration }}"
            description: >-
              The TLS certificate for {{ $labels.instance }} expires within 7 days.
              cert-manager should have renewed it automatically -- investigate why it did not.

        - alert: EndpointHighLatency
          expr: probe_duration_seconds{probe_group="platform-endpoints"} > 5
          for: 5m
          labels:
            severity: warning
            team: platform
          annotations:
            summary: "{{ $labels.instance }} probe latency > 5s"
            description: >-
              Blackbox probe to {{ $labels.instance }} is taking over 5 seconds.
              Check DNS resolution time (probe_dns_lookup_time_seconds) and
              HTTP response time for the slow component.

        - alert: LegacyRedirectDown
          expr: probe_success{probe_group="legacy-redirects"} == 0
          for: 5m
          labels:
            severity: warning
            team: platform
          annotations:
            summary: "Legacy redirect {{ $labels.instance }} is failing"
            description: >-
              Legacy Grafana redirect at {{ $labels.instance }} has been failing for 5 minutes.
              Expected a 301/302 response.
```

### Alert Routing

All rules carry `team: platform`. The existing Alertmanager config matches `team: platform` to the `pagerduty-platform` receiver. No Alertmanager changes needed.

Severity mapping:
- `critical` (EndpointDown) -- pages on-call immediately via PagerDuty
- `warning` (SSL expiry, latency, legacy redirects) -- PagerDuty low-urgency or Slack, depending on existing routing rules

---

## File Layout

All files live in the platform repo. Base manifests with kustomize overlays per environment.

```
clusters/prod/monitoring/
  kustomization.yaml                    # add blackbox-exporter.yaml, probes, alerts
  blackbox-exporter.yaml                # HelmRelease
  probe-grafana-prod.yaml               # Probe CRD for primary endpoint
  probe-grafana-legacy-redirects.yaml   # Probe CRD for legacy redirects
  prometheusrule-blackbox.yaml          # PrometheusRule with alert definitions

clusters/dev/monitoring/
  kustomization.yaml                    # add blackbox-exporter.yaml (no probes -- see below)
  blackbox-exporter.yaml                # HelmRelease (same chart, minimal config)
```

### Why No Base

There are only 4 files and the probe targets differ between environments. A `manifests/base/monitoring/blackbox/` with kustomize patches would add indirection without reducing duplication. If we add a third environment or probe targets grow significantly, extract a base then.

### Dev Environment

Dev does not expose Grafana publicly -- it is accessed via port-forward. There are no public endpoints to probe. The blackbox exporter HelmRelease is deployed in dev for consistency (so the chart upgrade path is tested in dev before prod), but no `Probe` or `PrometheusRule` resources are created. When dev gains public endpoints, add probes then.

---

## Deployment

### HelmRelease

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: blackbox-exporter
  namespace: monitoring
spec:
  interval: 1h
  chart:
    spec:
      chart: prometheus-blackbox-exporter
      version: "9.x"     # pin to latest 9.x
      sourceRef:
        kind: HelmRepository
        name: prometheus-community
        namespace: flux-system
  values:
    config:
      modules:
        http_2xx:
          prober: http
          timeout: 10s
          http:
            method: GET
            preferred_ip_protocol: ip4
            follow_redirects: true
            fail_if_ssl: false
        http_301:
          prober: http
          timeout: 10s
          http:
            method: GET
            preferred_ip_protocol: ip4
            no_follow_redirects: true
            valid_status_codes:
              - 301
              - 302
    replicas: 1
    resources:
      requests:
        cpu: 10m
        memory: 32Mi
      limits:
        memory: 64Mi
    serviceMonitor:
      enabled: true
      labels:
        release: sei-prod
```

### Flux Reconciliation

No ordering dependencies. The blackbox exporter HelmRelease, Probe CRDs, and PrometheusRule can all be applied in any order:

- The HelmRelease deploys the exporter pod and service.
- The Probe CRDs are picked up by the existing Prometheus instance on its next reconciliation cycle (typically within 60s).
- If the exporter pod is not yet running when Prometheus first scrapes, the probe returns `probe_success=0`, which is the correct initial state. The `for: 2m` duration on the alert prevents a false page during rollout.

Add all four files to `clusters/prod/monitoring/kustomization.yaml`:

```yaml
resources:
  # ... existing resources
  - blackbox-exporter.yaml
  - probe-grafana-prod.yaml
  - probe-grafana-legacy-redirects.yaml
  - prometheusrule-blackbox.yaml
```

---

## Limitations

### In-Cluster DNS Resolution (Critical Tradeoff)

The blackbox exporter runs inside the cluster. Its DNS queries go through CoreDNS, which forwards external names to upstream resolvers (typically the VPC DNS resolver in AWS). This means:

**What it catches:**
- TLS certificate expiry or misconfiguration
- HTTP routing errors (wrong backend, 502/503 from Gateway)
- Application-level failures (Grafana itself is down)
- DNS record deletion or misconfiguration (CNAME/A record pointing to wrong target)
- Gateway or NLB failures

**What it does NOT catch:**
- DNS delegation failures between public resolvers and Route53 (the exact incident that prompted this work). The VPC resolver can resolve Route53-hosted zones directly without traversing the public delegation chain.
- Public DNS propagation delays
- ISP-level DNS issues

This is a real gap. The incident that motivated this design -- a DNS delegation issue -- would likely not have been caught by an in-cluster probe.

### Mitigation Options

1. **Accept the limitation for now.** The in-cluster probe still catches 4 out of 5 failure modes (TLS, routing, application, NLB). DNS delegation changes are rare events (this was the first one). Ship this, then evaluate whether an external probe is worth the operational cost.

2. **Force public DNS resolution.** Configure the blackbox exporter to use a public DNS resolver (e.g., `8.8.8.8`) instead of CoreDNS. This is possible via a custom `resolv.conf` on the pod (`dnsPolicy: None`, `dnsConfig.nameservers: [8.8.8.8]`). However, this breaks resolution for any in-cluster targets we might add later, and it adds an external dependency (Google DNS) to our monitoring path.

3. **External synthetic monitoring.** Use a SaaS probe (Datadog Synthetic, Uptime Robot, AWS Route53 Health Checks, Grafana Cloud Synthetic Monitoring). These run from outside the cluster and exercise the full public DNS chain. This is the correct long-term answer but adds a vendor dependency and is out of scope for this design.

**Recommendation:** Option 1 (ship it with the known limitation) plus a Route53 Health Check for `grafana.prod.platform.sei.io` as a cheap external signal. Route53 Health Checks cost ~$0.75/month per endpoint, run from multiple AWS regions, and can trigger CloudWatch alarms that feed into PagerDuty. This gives us external DNS validation without a full synthetic monitoring vendor. The Route53 Health Check is a separate task -- it lives in Terraform, not in the platform repo's monitoring stack.

### Other Limitations

- **Probing from a single location.** The exporter runs in one cluster in one region. It cannot detect region-specific routing issues. Acceptable for our single-cluster setup.
- **No content validation.** The probe checks HTTP status codes, not response body content. A Grafana login page returning 200 with an error message would not trigger an alert. Blackbox exporter supports `fail_if_body_not_matches_regexp`, but this is fragile and not worth the maintenance burden for a login page.
- **No WebSocket probing.** The blackbox exporter does not support WebSocket health checks. If we add EVM WebSocket endpoints to the probe list later, we would need a TCP probe (which validates connectivity but not protocol correctness).

---

## Future Expansion

When `platform.sei.io` public RPC endpoints go live (per the public DNS design), add probes for:

```
https://pacific-1-rpc-rpc.pacific-1.platform.sei.io/status
https://pacific-1-rpc-evm.pacific-1.platform.sei.io  (POST with eth_blockNumber)
```

These would use a new `http_2xx_post` module for JSON-RPC endpoints. Keep them in a separate `Probe` resource with a `probe_group: rpc-endpoints` label for distinct alerting rules (these are protocol-team endpoints, not platform-team).
