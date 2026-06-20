// Package k8s is the real sei SDK provider: it creates SeiNetwork/SeiNode via
// controller-runtime server-side apply, stamps the canonical object labels, reads
// typed endpoints off .status, and runs a light readiness probe. It registers
// itself as "k8s" via init(), so a consumer opts in with a blank import.
//
// The provider is stateless beyond the kube client: it tracks no provisioned
// resources. Get reads a resource back from the apiserver into a handle.
package k8s

import (
	"context"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider"
)

func init() { provider.Register("k8s", New) }

// Provider is the k8s mode. Single-writer: the SSA field-owner is one manager,
// so a *sei.Client wrapping it is safe for sequential use only.
type Provider struct {
	c          ctrlclient.Client
	httpClient *http.Client
	defaultNS  string
}

// New is the registered Factory. It builds the controller-runtime client from
// the ambient kubeconfig chain and a probe HTTP client with a client-side timeout
// (http.DefaultClient has none, and a hung node RPC would block a poll iteration
// past its budget).
func New(context.Context) (provider.Provider, error) {
	c, defaultNS, err := buildClient()
	if err != nil {
		return nil, fmt.Errorf("building kube client: %w", err)
	}
	return &Provider{
		c:          c,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		defaultNS:  defaultNS,
	}, nil
}

// Name reports the registered mode name.
func (p *Provider) Name() string { return "k8s" }

// CreateNetwork SSA-applies the SeiNetwork and returns a handle immediately. It
// does not wait — the caller calls Network.WaitReady.
func (p *Provider) CreateNetwork(ctx context.Context, spec sei.NetworkSpec) (sei.NetworkHandle, error) {
	ns := p.ns(spec.Namespace)
	net := renderNetwork(spec, ns)
	if err := p.apply(ctx, net, fmt.Sprintf("SeiNetwork %s/%s", ns, net.Name)); err != nil {
		return nil, err
	}
	return &networkHandle{p: p, namespace: ns, name: net.Name, net: net}, nil
}

// GetNetwork reads an existing SeiNetwork into a handle.
func (p *Provider) GetNetwork(ctx context.Context, name, namespace string) (sei.NetworkHandle, error) {
	ns := p.ns(namespace)
	net := &seiv1alpha1.SeiNetwork{}
	if err := p.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, net); err != nil {
		return nil, err
	}
	return &networkHandle{p: p, namespace: ns, name: name, net: net}, nil
}

// CreateNode SSA-applies one SeiNode peered to spec.Network and returns a handle
// immediately. Peering is derived from spec.Network — the canonical
// sei.io/seinetwork wiring — at the node's own namespace.
func (p *Provider) CreateNode(ctx context.Context, spec sei.NodeSpec) (sei.NodeHandle, error) {
	ns := p.ns(spec.Namespace)
	node := renderNode(spec, ns)
	if err := p.apply(ctx, node, fmt.Sprintf("SeiNode %s/%s", ns, node.Name)); err != nil {
		return nil, err
	}
	return &nodeHandle{p: p, namespace: ns, name: node.Name, node: node}, nil
}

// GetNode reads an existing SeiNode into a handle.
func (p *Provider) GetNode(ctx context.Context, name, namespace string) (sei.NodeHandle, error) {
	ns := p.ns(namespace)
	node := &seiv1alpha1.SeiNode{}
	if err := p.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, node); err != nil {
		return nil, err
	}
	return &nodeHandle{p: p, namespace: ns, name: name, node: node}, nil
}

// ns returns specNS or the provider default when specNS is empty.
func (p *Provider) ns(specNS string) string {
	if specNS != "" {
		return specNS
	}
	return p.defaultNS
}

// apply server-side-applies obj under the SDK's field owner with ForceOwnership.
func (p *Provider) apply(ctx context.Context, obj ctrlclient.Object, resource string) error {
	if err := p.c.Patch(ctx, obj, ctrlclient.Apply, fieldOwner, ctrlclient.ForceOwnership); err != nil { //nolint:staticcheck // SA1019: SSA via ctrlclient.Apply intentionally matches the controller's established pattern (internal/task); module-wide migration tracked separately
		if apierrors.IsConflict(err) {
			return fmt.Errorf("%s: SSA conflict (another field manager owns a field): %w", resource, err)
		}
		return fmt.Errorf("%s: server-side apply: %w", resource, err)
	}
	return nil
}
