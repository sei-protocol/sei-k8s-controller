# Public DNS: platform.sei.io

**Status:** Draft
**Date:** 2026-04-10
**Scope:** Platform — DNS, TLS, Gateway, External-DNS, Controller

---

## Problem

Public Sei node endpoints are currently exposed two ways, neither fully satisfying HTTPS-on-443:

1. **Gateway HTTPRoutes** — hostnames like `pacific-1-rpc.rpc.prod.platform.sei.io` route through the Istio Gateway with TLS termination, but the wildcard cert `*.prod.platform.sei.io` doesn't cover two-level-deep subdomains (cert mismatch).
2. **Service-level NLBs** — each SeiNodeDeployment creates its own LoadBalancer with `external-dns.alpha.kubernetes.io/hostname: rpc.pacific-1.prod.platform.sei.io`. These are raw TCP (no TLS termination), exposing non-standard ports directly.

We want clean public URLs under `platform.sei.io` — everything behind HTTPS on port 443 — without breaking existing `prod.platform.sei.io` consumers.

## DNS Strategy

### Zone Setup

| Zone | Managed By | Purpose |
|------|-----------|---------|
| `prod.platform.sei.io` | Existing Route53 zone | Internal / legacy — no changes |
| `platform.sei.io` | **New** Route53 hosted zone (AWS CLI / console) | Public-facing endpoints |

The new `platform.sei.io` zone is created manually (one-time operation, no Terraform required). NS delegation from the parent `sei.io` zone is the only prerequisite. A/CNAME records within the zone are fully managed by External-DNS from HTTPRoute hostnames.

### Hostname Pattern

Per-namespace wildcard certs enable a structured, multi-level pattern:

```
{deploymentName}-{protocol}.{namespace}.platform.sei.io
```

Each namespace represents a deployment scope (today: a chain, eventually: a customer). The protocol is hyphen-delimited as a suffix on the deployment name, keeping it within the namespace wildcard's single-level match.

**Examples for `pacific-1` namespace:**

| Endpoint | Hostname |
|----------|----------|
| Tendermint RPC | `pacific-1-rpc-rpc.pacific-1.platform.sei.io` |
| EVM JSON-RPC | `pacific-1-rpc-evm.pacific-1.platform.sei.io` |
| EVM WebSocket | `pacific-1-rpc-evm-ws.pacific-1.platform.sei.io` |
| REST / LCD | `pacific-1-rpc-rest.pacific-1.platform.sei.io` |
| gRPC | `pacific-1-rpc-grpc.pacific-1.platform.sei.io` |

**Why per-namespace wildcards?** A single `*.platform.sei.io` wildcard cert only matches one subdomain level, which forces all identifying information into a flat hyphenated token — ambiguous when deployment names are arbitrary (is `my-cool-node-rpc` the deployment `my-cool-node` with protocol `rpc`, or `my-cool` with `node-rpc`?). Per-namespace wildcards (`*.pacific-1.platform.sei.io`) give us a clean separator: everything left of the first dot is `{deployment}-{protocol}`, everything right is the namespace scope.

**Multi-tenant readiness:** Today the set of namespaces is small and static (pacific-1, atlantic-2, arctic-1). Certs and listeners are declared in the platform repo per namespace. When this becomes a product, namespace+cert+listener creation becomes part of account provisioning — the controller or a provisioning workflow creates them dynamically.

## TLS

### Per-Namespace Certificates

One Certificate resource per namespace, stored in the platform repo:

```yaml
# clusters/prod/gateway/certificate-pacific-1.yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: sei-gateway-pacific-1-tls
  namespace: gateway
spec:
  secretName: sei-gateway-pacific-1-tls
  issuerRef:
    name: letsencrypt
    kind: ClusterIssuer
  dnsNames:
    - "*.pacific-1.platform.sei.io"
```

Repeat for `atlantic-2` and `arctic-1`. Three certs total today.

### ClusterIssuer Update

Add `platform.sei.io` to the existing ClusterIssuer's dnsZones selector so cert-manager can solve DNS-01 challenges for all subzones:

```yaml
# clusters/prod/cert-manager/issuer.yaml
solvers:
  - dns01:
      route53:
        region: eu-central-1
    selector:
      dnsZones:
        - prod.platform.sei.io
        - platform.sei.io        # <-- add
```

**Prerequisite:** The cert-manager IRSA role needs `route53:ChangeResourceRecordSets` on the new `platform.sei.io` hosted zone.

## Gateway Changes

Add a dedicated HTTPS listener per namespace. The existing listeners remain untouched:

```yaml
# clusters/prod/gateway/gateway.yaml — add to spec.listeners
- name: https-pacific-1
  port: 443
  protocol: HTTPS
  hostname: "*.pacific-1.platform.sei.io"
  tls:
    mode: Terminate
    certificateRefs:
      - name: sei-gateway-pacific-1-tls
  allowedRoutes:
    namespaces:
      from: All

- name: https-atlantic-2
  port: 443
  protocol: HTTPS
  hostname: "*.atlantic-2.platform.sei.io"
  tls:
    mode: Terminate
    certificateRefs:
      - name: sei-gateway-atlantic-2-tls
  allowedRoutes:
    namespaces:
      from: All

- name: https-arctic-1
  port: 443
  protocol: HTTPS
  hostname: "*.arctic-1.platform.sei.io"
  tls:
    mode: Terminate
    certificateRefs:
      - name: sei-gateway-arctic-1-tls
  allowedRoutes:
    namespaces:
      from: All
```

All listeners share port 443 on the same NLB. Istio multiplexes via SNI — the client's TLS handshake includes the hostname, and Istio selects the matching listener and cert. The existing `https` listener continues to serve `*.prod.platform.sei.io` traffic.

The existing HTTP→HTTPS redirect HTTPRoute already catches all port-80 traffic, so it covers the new domains automatically.

## External-DNS Changes

Add `platform.sei.io` to the domain filter. External-DNS will then create A/CNAME records in the new zone as hostnames appear on HTTPRoutes:

```yaml
# clusters/prod/external-dns/external-dns.yaml — values
domainFilters:
  - prod.platform.sei.io
  - platform.sei.io        # <-- add
```

**Prerequisite:** The external-dns IRSA role needs `route53:ChangeResourceRecordSets` + `route53:ListResourceRecordSets` on the new `platform.sei.io` hosted zone.

## Controller Changes

The controller currently reads a single `SEI_GATEWAY_DOMAIN` env var and generates one hostname per protocol. To emit public routes on the new domain, add a second env var:

```
SEI_GATEWAY_DOMAIN=prod.platform.sei.io        # existing, unchanged
SEI_GATEWAY_PUBLIC_DOMAIN=platform.sei.io       # new
```

### Hostname Generation Update

In `internal/controller/nodedeployment/networking.go`, `resolveEffectiveRoutes` currently produces:

```
{deploymentName}.{protocol}.{gatewayDomain}
```

When `SEI_GATEWAY_PUBLIC_DOMAIN` is set, each `effectiveRoute` gains a second hostname using the namespace-scoped pattern:

```
{deploymentName}-{protocol}.{namespace}.{publicDomain}
```

Both hostnames land in the same HTTPRoute's `spec.hostnames[]` array — no additional Route resources needed. The Gateway matches each hostname to the correct listener/cert via SNI.

```go
// networking.go — resolveEffectiveRoutes, per-protocol loop
hostnames := []string{
    fmt.Sprintf("%s.%s.%s", group.Name, proto.Prefix, domain),
}
if publicDomain != "" {
    hostnames = append(hostnames,
        fmt.Sprintf("%s-%s.%s.%s", group.Name, proto.Prefix, group.Namespace, publicDomain),
    )
}
```

### parentRefs

HTTPRoutes currently reference the Gateway without a `sectionName`, which binds to all listeners. This continues to work — the Gateway selects the correct listener based on hostname/SNI matching. No parentRefs change needed.

## What Stays the Same

| Component | Change? |
|-----------|---------|
| Existing `*.prod.platform.sei.io` cert | No |
| Existing Gateway `https` listener | No |
| Existing HTTPRoute hostnames | No — they stay in the `spec.hostnames` array |
| Existing service-level NLBs | No (but see note below) |
| HTTP→HTTPS redirect | No — already catches all port-80 traffic |
| SeiNodeDeployment manifests | No — controller handles hostname generation |

> **Note on service NLBs:** The per-deployment LoadBalancer services (e.g., `rpc.pacific-1.prod.platform.sei.io` → NLB → raw TCP) remain as-is for backward compatibility. They do NOT get `platform.sei.io` equivalents because they can't terminate TLS. Over time, consumers should migrate to the Gateway-fronted `platform.sei.io` endpoints. Once migration is complete, the service NLBs can be removed by setting `networking.service: null` on the SeiNodeDeployments.

## Rollout Plan

### Phase 1: Route53 Zone
1. Create `platform.sei.io` hosted zone via AWS CLI
2. Add NS delegation from `sei.io` to the new zone
3. Grant cert-manager and external-dns IRSA roles access to the new zone

### Phase 2: TLS + Gateway (Flux — platform repo)
1. Update ClusterIssuer — add `platform.sei.io` to dnsZones
2. Add per-namespace Certificate resources (pacific-1, atlantic-2, arctic-1)
3. Add per-namespace HTTPS listeners to Gateway
4. Update external-dns domainFilters
5. Update gateway kustomization.yaml to include new cert files
6. **Verify:** `kubectl get certificate -n gateway` shows all new certs Ready
7. **Verify:** `kubectl get gateway sei-gateway -n gateway -o yaml` shows new listeners Programmed

### Phase 3: Controller (sei-k8s-controller)
1. Add `GatewayPublicDomain` to `platform.Config` and `cmd/main.go`
2. Update `resolveEffectiveRoutes` to emit dual hostnames
3. Add tests for public hostname generation
4. Deploy updated controller image
5. **Verify:** `kubectl get httproute -A -o yaml | grep platform.sei.io` shows new hostnames
6. **Verify:** `dig pacific-1-rpc-rpc.pacific-1.platform.sei.io` resolves to Gateway NLB
7. **Verify:** `curl https://pacific-1-rpc-rpc.pacific-1.platform.sei.io/status` returns 200

### Phase 4: Documentation + Migration
1. Publish `platform.sei.io` endpoints as the canonical public URLs
2. Deprecate `prod.platform.sei.io` in external docs
3. (Future) Remove per-deployment NLBs once consumers have migrated

## Files Changed

| Repo | File | Change |
|------|------|--------|
| — | AWS Route53 | New `platform.sei.io` hosted zone (CLI) |
| — | `sei.io` parent zone | NS delegation for `platform.sei.io` |
| `platform` | `terraform/.../prod/cert-manager.tf` | IRSA policy for new zone |
| `platform` | `terraform/.../prod/external-dns.tf` | IRSA policy for new zone |
| `platform` | `clusters/prod/cert-manager/issuer.yaml` | Add `platform.sei.io` to dnsZones |
| `platform` | `clusters/prod/gateway/certificate-pacific-1.yaml` | New — `*.pacific-1.platform.sei.io` |
| `platform` | `clusters/prod/gateway/certificate-atlantic-2.yaml` | New — `*.atlantic-2.platform.sei.io` |
| `platform` | `clusters/prod/gateway/certificate-arctic-1.yaml` | New — `*.arctic-1.platform.sei.io` |
| `platform` | `clusters/prod/gateway/gateway.yaml` | Add per-namespace HTTPS listeners |
| `platform` | `clusters/prod/gateway/kustomization.yaml` | Include new cert files |
| `platform` | `clusters/prod/external-dns/external-dns.yaml` | Add domain filter |
| `controller` | `internal/platform/platform.go` | Add `GatewayPublicDomain` field |
| `controller` | `cmd/main.go` | Wire `SEI_GATEWAY_PUBLIC_DOMAIN` env var |
| `controller` | `internal/controller/nodedeployment/networking.go` | Dual hostname generation |
| `controller` | `internal/controller/nodedeployment/networking_test.go` | Test coverage |
| `platform` | `clusters/prod/sei-k8s-controller/...` | Bump image + add env var |
