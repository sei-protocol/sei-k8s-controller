# Unified OAuth2 Authentication for Internal Platform Tools

**Status:** Accepted
**Date:** 2026-04-10
**Scope:** Platform — Istio ext_authz, OAuth2 Proxy, Gateway API, Monitoring

---

## Problem

Internal platform tools (Prometheus, AlertManager) are not externally accessible. PagerDuty alert links contain `generatorURL` pointing to the cluster-internal Prometheus address (`http://sei-prod-prometheus.monitoring:9090/graph?g0.expr=...`), which is unreachable from a browser. On-call engineers cannot click through to see the metric that triggered an alert.

Exposing these tools requires authentication. The old per-chain monitoring stack (sei-infra) had Prometheus behind ALBs without auth — we should not replicate that pattern.

## Solution

Deploy a shared **OAuth2 Proxy** as an Istio **ext_authz** extension provider. Any internal tool can opt in to Google OAuth by adding an `AuthorizationPolicy` with `action: CUSTOM`. The first consumers are Prometheus and AlertManager.

### Architecture

```
Browser → NLB → sei-gateway (TLS termination)
                      │
              AuthorizationPolicy (CUSTOM action)
                      │
              ext_authz check → OAuth2 Proxy (auth namespace)
                      │
              ┌── 202 (authenticated) → HTTPRoute → backend
              └── 302 (unauthenticated) → Google OAuth → callback → retry
```

### Auth Flow

1. User clicks PagerDuty link to `https://prometheus.prod.platform.sei.io/graph?g0.expr=...`
2. Istio Gateway matches the CUSTOM AuthorizationPolicy, sends ext_authz check to OAuth2 Proxy
3. OAuth2 Proxy checks for `_sei_platform_auth` cookie — not found
4. Returns 302 to Google OAuth with `redirect_uri=https://oauth2-proxy.prod.platform.sei.io/oauth2/callback` and `state=<original-url>`
5. User authenticates with Google (same SSO as Grafana — `@seinetwork.io`, `@sei.io`, `@seifdn.org`)
6. Google redirects to callback; OAuth2 Proxy sets cookie on `.prod.platform.sei.io`, redirects back to original URL
7. Browser retries with cookie → ext_authz passes → Prometheus graph loads

### SSO

Cookie domain `.prod.platform.sei.io` means one login covers all protected tools. Authenticating on Prometheus automatically works on AlertManager, and any future tools added under the same domain.

---

## Design Decisions

### Shared Google OAuth Client

OAuth2 Proxy reuses the existing Google OAuth client that Grafana already uses. Add the OAuth2 Proxy callback URI to the existing client's authorized redirect URIs in Google Cloud Console:

```
https://oauth2-proxy.prod.platform.sei.io/oauth2/callback
```

This avoids managing a second set of credentials. The OAuth2 Proxy deployment references the same `google-oauth` secret (client ID and client secret) plus its own `cookie-secret`.

### Dedicated `auth` Namespace

OAuth2 Proxy deploys in its own `auth` namespace rather than `monitoring`. This isolates the authentication infrastructure from the monitoring workloads it protects, limiting blast radius if either is compromised.

### Fail-Closed

`failOpen: false` in the ext_authz config. When OAuth2 Proxy is down, protected tools return 503 rather than being accessible without auth. A PodDisruptionBudget (`minAvailable: 1`) and 2 replicas mitigate downtime.

### Cookie-Based Sessions (No Redis)

Session state lives in encrypted cookies. No shared state, no Redis, perfect HA. Appropriate for a team of <100 engineers. Revisit only if cookie size exceeds browser limits (4KB), which would happen with large group claims — not applicable here.

### Grafana Excluded

Grafana keeps its own Google OAuth integration. It needs user identity for role mapping (`@seinetwork.io` → Editor, others → Viewer). The AuthorizationPolicy only targets Prometheus and AlertManager hostnames, so Grafana is never intercepted.

---

## Resources

### Istio Mesh Config — Extension Provider

Register OAuth2 Proxy as an ext_authz provider in the istiod mesh config:

```yaml
meshConfig:
  extensionProviders:
    - name: oauth2-proxy
      envoyExtAuthz:
        service: oauth2-proxy.auth.svc.cluster.local
        port: 4180
        includeRequestHeadersInCheck:
          - authorization
          - cookie
        headersToUpstreamOnAllow:
          - authorization
          - x-auth-request-user
          - x-auth-request-email
          - cookie
        headersToDownstreamOnDeny:
          - set-cookie
          - content-type
          - location
        headersToDownstreamOnAllow:
          - set-cookie
        failOpen: false
        statusOnError: "503"
```

### OAuth2 Proxy (auth namespace)

Deployed via Helm chart `oauth2-proxy/oauth2-proxy` from `https://oauth2-proxy.github.io/manifests`.

Key configuration:
- `provider = "google"`
- `upstreams = ["static://202"]` — ext_authz mode, not reverse-proxy mode
- `email_domains = ["seinetwork.io", "sei.io", "seifdn.org"]`
- `cookie_name = "_sei_platform_auth"`
- `cookie_domains = [".prod.platform.sei.io"]`
- `cookie_expire = "12h"`, `cookie_refresh = "1h"`
- `session_store_type = "cookie"`
- `reverse_proxy = true` — trusts X-Forwarded-* from Gateway
- `set_xauthrequest = true` — forwards user identity headers to backends

Secrets (SOPS-encrypted): Google client ID, client secret, cookie secret (32-byte random).

### HTTPRoutes

| Hostname | Backend | Namespace |
|---|---|---|
| `prometheus.prod.platform.sei.io` | `sei-prod-prometheus:9090` | monitoring |
| `alertmanager.prod.platform.sei.io` | `sei-prod-alertmanager:9093` | monitoring |
| `oauth2-proxy.prod.platform.sei.io` | `oauth2-proxy:4180` | auth |

All covered by the existing wildcard cert `*.prod.platform.sei.io`. External-DNS auto-creates DNS records from HTTPRoute hostnames.

### AuthorizationPolicy

```yaml
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: platform-tools-ext-authz
  namespace: gateway
spec:
  targetRefs:
    - group: gateway.networking.k8s.io
      kind: Gateway
      name: sei-gateway
  action: CUSTOM
  provider:
    name: oauth2-proxy
  rules:
    - to:
        - operation:
            hosts:
              - prometheus.prod.platform.sei.io
              - alertmanager.prod.platform.sei.io
```

Scoped to specific hostnames. All other gateway traffic (RPC endpoints, Grafana, etc.) is unaffected.

### Prometheus Helm Changes

```yaml
prometheus:
  prometheusSpec:
    externalUrl: https://prometheus.prod.platform.sei.io

alertmanager:
  alertmanagerSpec:
    externalUrl: https://alertmanager.prod.platform.sei.io
```

This fixes the broken PagerDuty `generatorURL` links.

---

## Interaction with Existing AuthorizationPolicies

Istio evaluates authorization in order: **CUSTOM → DENY → ALLOW**.

- SeiNode AuthorizationPolicies use `action: ALLOW` with pod selectors — they operate at the sidecar level on SeiNode pods, not at the gateway.
- The new CUSTOM policy targets the Gateway workload and only fires for matching hostnames.
- No conflict, no interference.

---

## Adding Future Services

To protect a new tool (e.g., Jaeger):

1. Add an HTTPRoute for `jaeger.prod.platform.sei.io`
2. Add the hostname to the AuthorizationPolicy `hosts` list

No OAuth2 Proxy changes needed — the cookie domain covers all `*.prod.platform.sei.io` subdomains.

---

## Rollout Order

1. Register ext_authz provider in Istio mesh config (safe — nothing references it yet)
2. Deploy OAuth2 Proxy (Helm, Secret, Service, PDB) in `auth` namespace
3. Create OAuth2 callback HTTPRoute, verify callback endpoint responds
4. Create Prometheus + AlertManager HTTPRoutes
5. Apply AuthorizationPolicy (auth now enforced)
6. Update Prometheus/AlertManager `externalUrl` in Helm values
7. Verify: click PagerDuty alert link → Google OAuth → Prometheus graph

---

## File Layout

```
clusters/prod/auth/
  kustomization.yaml
  namespace.yaml
  oauth2-proxy.yaml           # HelmRepository + HelmRelease
  secret.enc.yaml             # SOPS: client-id, client-secret, cookie-secret
  httproute.yaml              # oauth2-proxy.prod.platform.sei.io
  authz-policy.yaml           # CUSTOM AuthorizationPolicy (in gateway namespace)

clusters/prod/monitoring/
  httproute-prometheus.yaml   # prometheus.prod.platform.sei.io
  httproute-alertmanager.yaml # alertmanager.prod.platform.sei.io
  prometheus-operator.yaml    # externalUrl changes

clusters/prod/istio-system/
  mesh-config patch            # ext_authz extension provider
```

---

## Observability

OAuth2 Proxy exposes Prometheus metrics on port 44180. A ServiceMonitor scrapes these, and a PrometheusRule alerts on:
- `OAuth2ProxyHighErrorRate` (>5% 5xx for 5m) — warning
- `OAuth2ProxyDown` (metrics unreachable for 3m) — critical
