# LLD: Peer Configuration Refactoring

## Overview

Refactor peer configuration in the SeiNode CRD to:
1. Hoist `Peers` from mode sub-specs to `SeiNodeSpec` (all modes use it identically)
2. Add `label`-based peer discovery via Kubernetes label selectors
3. Continuously reconcile resolved peers into `status.resolvedPeers` for future dynamic peer updates

## CRD Changes

### PeerSource (common_types.go)

Add `Label` as a third variant:

```go
// +kubebuilder:validation:XValidation:rule="(has(self.ec2Tags) ? 1 : 0) + (has(self.static) ? 1 : 0) + (has(self.label) ? 1 : 0) == 1",message="exactly one of ec2Tags, static, or label must be set"
type PeerSource struct {
    EC2Tags *EC2TagsPeerSource   `json:"ec2Tags,omitempty"`
    Static  *StaticPeerSource    `json:"static,omitempty"`
    Label   *LabelPeerSource     `json:"label,omitempty"`
}

// LabelPeerSource discovers peers by selecting SeiNode resources via
// Kubernetes labels. The controller resolves matching nodes to stable
// headless Service DNS names and tracks them in status.resolvedPeers.
type LabelPeerSource struct {
    // Selector is a set of key-value label pairs. SeiNode resources
    // matching ALL labels are included as peers.
    // +kubebuilder:validation:MinProperties=1
    Selector map[string]string `json:"selector"`

    // Namespace restricts discovery to a specific namespace.
    // When empty, defaults to the namespace of the discovering node.
    // +optional
    Namespace string `json:"namespace,omitempty"`
}
```

### SeiNodeSpec (seinode_types.go)

Hoist `Peers` from mode sub-specs:

```go
type SeiNodeSpec struct {
    ChainID    string            `json:"chainId"`
    Image      string            `json:"image"`
    Peers      []PeerSource      `json:"peers,omitempty"`   // NEW: hoisted from sub-specs
    Entrypoint *EntrypointConfig `json:"entrypoint,omitempty"`
    Overrides  map[string]string `json:"overrides,omitempty"`
    Sidecar    *SidecarConfig    `json:"sidecar,omitempty"`
    PodLabels  map[string]string `json:"podLabels,omitempty"`
    FullNode   *FullNodeSpec     `json:"fullNode,omitempty"`
    Archive    *ArchiveSpec      `json:"archive,omitempty"`
    Replayer   *ReplayerSpec     `json:"replayer,omitempty"`
    Validator  *ValidatorSpec    `json:"validator,omitempty"`
}
```

Add XValidation for replayer peers requirement:
```
+kubebuilder:validation:XValidation:rule="!has(self.replayer) || (has(self.peers) && size(self.peers) > 0)",message="peers is required when replayer mode is set"
```

Remove `Peers` field from FullNodeSpec, ValidatorSpec, ArchiveSpec, ReplayerSpec.

### SeiNodeStatus (seinode_types.go)

Add resolved peers tracking:

```go
type SeiNodeStatus struct {
    // ...existing fields...

    // ResolvedPeers is the current set of peer addresses discovered
    // from label-based peer sources. Reconciled continuously.
    // Format: "{dns-hostname}" (node ID resolved at task execution time by sidecar)
    // +optional
    ResolvedPeers []string `json:"resolvedPeers,omitempty"`
}
```

## Architecture: Where Each Step Runs

### Controller Reconciler (continuous)
- `reconcilePeers`: lists SeiNodes matching each `label` source selector, builds DNS addresses, writes `status.resolvedPeers`
- Runs every reconcile, not just at plan-build time
- Future: compare against previous resolvedPeers, set `PeersChanged` condition on drift

### Planner (plan-build time)
- Reads `node.Spec.Peers` for ec2Tags and static sources (pass through)
- Reads `node.Status.ResolvedPeers` for label-resolved addresses (converted to `dnsEndpoints` task param)
- Planner stays pure: no K8s API calls

### Sidecar (task execution time)
- Receives `dnsEndpoints` source type: list of DNS hostnames
- Queries each hostname's `:26657/status` for Tendermint node ID
- Builds `nodeId@dns:26656` peer strings
- Writes to `config.toml` persistent-peers
- **No Kubernetes awareness, no new RBAC**

## Sidecar Changes

### New PeerSource: DNSEndpointsSource (peers.go)

```go
type DNSEndpointsSource struct {
    Endpoints   []string
    QueryNodeID NodeIDQuerier
}

func (s *DNSEndpointsSource) Discover(ctx context.Context) ([]string, error) {
    // For each endpoint: query RPC for node ID, build nodeId@endpoint:26656
    // Skip unreachable endpoints (peer not ready yet)
}
```

Wire into `buildSources` dispatcher as `"dnsEndpoints"` type.

Named `dnsEndpoints` (not `label`) so the sidecar stays infrastructure-agnostic.

### PeerSourceEntry update

```go
type PeerSourceEntry struct {
    Type      string            `json:"type"`
    Region    string            `json:"region,omitempty"`
    Tags      map[string]string `json:"tags,omitempty"`
    Addresses []string          `json:"addresses,omitempty"`
    Endpoints []string          `json:"endpoints,omitempty"` // DNS endpoints for dnsEndpoints type
}
```

## Controller Changes

### Delete: PeersFor helper (planner.go)
No longer needed. All callers read `node.Spec.Peers` directly.

### Update: discoverPeersParams (planner.go)
Accept pre-resolved sources. Label sources come from `status.resolvedPeers`:

```go
func discoverPeersParams(specPeers []seiv1alpha1.PeerSource, resolvedPeers []string) *task.DiscoverPeersParams {
    var sources []task.PeerSourceParam
    for _, s := range specPeers {
        switch {
        case s.EC2Tags != nil:
            sources = append(sources, task.PeerSourceParam{Type: "ec2Tags", ...})
        case s.Static != nil:
            sources = append(sources, task.PeerSourceParam{Type: "static", ...})
        case s.Label != nil:
            // Handled via resolvedPeers below
        }
    }
    if len(resolvedPeers) > 0 {
        sources = append(sources, task.PeerSourceParam{Type: "dnsEndpoints", Endpoints: resolvedPeers})
    }
    return &task.DiscoverPeersParams{Sources: sources}
}
```

### Update: All planners
Change `node.Spec.{Mode}.Peers` to `node.Spec.Peers` in full.go, validator.go, archive.go, replay.go.

### Update: genesis_peers.go
Patch `node.Spec.Peers` instead of `node.Spec.Validator.Peers`. Remove `Validator == nil` guard.

### New: reconcilePeers (controller/node/)

```go
func (r *SeiNodeReconciler) reconcilePeers(ctx context.Context, node *seiv1alpha1.SeiNode) error {
    var resolved []string
    for _, src := range node.Spec.Peers {
        if src.Label == nil {
            continue
        }
        ns := node.Namespace
        if src.Label.Namespace != "" {
            ns = src.Label.Namespace
        }
        excludeSelf := src.Label.ExcludeSelf == nil || *src.Label.ExcludeSelf

        var nodeList seiv1alpha1.SeiNodeList
        r.List(ctx, &nodeList, client.InNamespace(ns), client.MatchingLabels(src.Label.Selector))

        for _, peer := range nodeList.Items {
            if excludeSelf && peer.Name == node.Name { continue }
            dns := fmt.Sprintf("%s-0.%s.%s.svc.cluster.local", peer.Name, peer.Name, peer.Namespace)
            resolved = append(resolved, dns)
        }
    }
    node.Status.ResolvedPeers = resolved
    return nil
}
```

## RBAC

No new RBAC needed. Controller already has SeiNode list/watch. Sidecar gets zero new permissions.

## Migration

Breaking change acceptable for v1alpha1. Mechanical field-path move from `spec.{mode}.peers` to `spec.peers`. Small number of existing CRs.

## Mixed EC2 + K8s Environments

Works naturally — PeerSource is a list:
```yaml
peers:
  - label:
      selector:
        sei.io/nodegroup: my-validators
  - ec2Tags:
      region: us-east-1
      tags: { Chain: pacific-1 }
```

## Example CR

```yaml
apiVersion: sei.io/v1alpha1
kind: SeiNode
metadata:
  name: my-validator
spec:
  chainId: atlantic-2
  image: ghcr.io/sei-protocol/seid:v6.3.0
  peers:
    - label:
        selector:
          sei.io/nodegroup: atlantic-validators
  validator:
    snapshot:
      s3:
        targetHeight: 50000000
```

## Future Work (not in scope)

- `PeersChanged` condition when resolvedPeers differs from last reconcile
- Config-apply plan triggered by PeersChanged to update persistent-peers at runtime
- Cross-cluster peer discovery (would use `static` source with known addresses)
