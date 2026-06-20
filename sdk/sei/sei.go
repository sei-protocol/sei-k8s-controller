// Package sei is the typed, provider-agnostic surface a CICD chaos harness
// programs against to provision SeiNetwork/SeiNode and read endpoints in-process
// (WS-E LLD §3). It mirrors database/sql: a provider package registers itself in
// init(), the consumer blank-imports it, and Open selects the flavor by name.
//
// It is also the canonical single source of truth for the readiness probe and
// the label/peer-wiring constants (§5.6) — the controller's copies are
// internal/-trapped and unimportable, so the SDK authors them once here.
package sei

import (
	"context"
	"os"
	"strings"
)

// Open selects a provisioning flavor by name and returns a Client bound to it.
// The flavor must have been registered via a blank import of its provider
// package (see package provider).
//
// With name == "", Open reads SEI_PROVIDER, then falls back to env-presence
// detection: SEI_NODE_CLUSTER => "k8s", SEI_LOCAL => "local". Both presence
// vars set is an ambiguous ClassUsage error — never guess. The k8s provider
// resolves its config from the ambient kubeconfig chain; there is no
// caller-supplied connection string (dsn dropped for MVP, §3.1).
func Open(ctx context.Context, name string) (*Client, error) {
	resolved, err := resolveFlavor(name)
	if err != nil {
		return nil, err
	}
	factory, ok := lookupFactory(resolved)
	if !ok {
		return nil, usageErr(
			"unknown provider %q (registered: %v) — forgotten blank import? "+
				`add: import _ "github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/%s"`,
			resolved, registeredNames(), resolved)
	}
	p, err := factory(ctx)
	if err != nil {
		return nil, err
	}
	return &Client{provider: p}, nil
}

// Provider-flavor names the core matches in env-presence detection. These MUST
// equal the keys the provider packages pass to RegisterProvider — the core only
// names the literals here; the providers register under their own string.
const (
	providerK8s   = "k8s"
	providerLocal = "local"
)

// resolveFlavor implements the four-step selection of LLD §4.3.
func resolveFlavor(name string) (string, error) {
	if name != "" {
		return name, nil
	}
	if v := os.Getenv("SEI_PROVIDER"); v != "" {
		return v, nil
	}
	_, k8s := os.LookupEnv("SEI_NODE_CLUSTER")
	_, local := os.LookupEnv("SEI_LOCAL")
	switch {
	case k8s && local:
		return "", usageErr("both SEI_NODE_CLUSTER and SEI_LOCAL are set — flavor is ambiguous; set SEI_PROVIDER or pass an explicit name")
	case k8s:
		return providerK8s, nil
	case local:
		return providerLocal, nil
	default:
		return "", usageErr("no provider flavor selected — pass a name to Open, or set SEI_PROVIDER / SEI_NODE_CLUSTER / SEI_LOCAL")
	}
}

// Client is the handle a harness holds for the life of a test. Safe for
// sequential use; NOT goroutine-safe across provisioning calls — the provider's
// SSA field-owner is single-writer (LLD §3.1/§5.5).
type Client struct {
	provider Provider
}

// Close releases provider resources (k8s: none; local: stops nodes).
func (c *Client) Close() error { return c.provider.Close() }

// ProvisionNetwork provisions a genesis SeiNetwork and returns once it is Ready
// or ctx/ReadyTimeout fires (the typed WS-A network apply + watch --until=Ready
// pair).
func (c *Client) ProvisionNetwork(ctx context.Context, spec NetworkSpec) (*Network, error) {
	if err := validateNetworkSpec(spec); err != nil {
		return nil, err
	}
	h, err := c.provider.ProvisionNetwork(ctx, spec.withDefaults())
	if err != nil {
		return nil, err
	}
	return &Network{handle: h}, nil
}

// ProvisionFleet provisions N follower SeiNodes peered to net and returns once
// every node is Running AND its readiness probe passes, or ctx/timeout fires
// (the typed WS-B provision-node fan-out + two-stage readiness gate). Peering is
// derived from net, never from spec (§5.3).
func (c *Client) ProvisionFleet(ctx context.Context, net *Network, spec FleetSpec) (*Fleet, error) {
	if net == nil {
		return nil, usageErr("ProvisionFleet: net is nil — provision a network first")
	}
	if err := validateFleetSpec(spec); err != nil {
		return nil, err
	}
	h, err := c.provider.ProvisionFleet(ctx, net.handle, spec.withDefaults())
	if err != nil {
		return nil, err
	}
	return &Fleet{handle: h}, nil
}

// Network is a provisioned genesis network handle.
type Network struct{ handle NetworkHandle }

// Name is the SeiNetwork resource name.
func (n *Network) Name() string { return n.handle.Name() }

// Endpoints reads typed URLs off SeiNetwork .status.endpoints — never
// reconstructed.
func (n *Network) Endpoints() Endpoints { return n.handle.Endpoints() }

// Teardown deletes the network this handle owns. Idempotent.
func (n *Network) Teardown(ctx context.Context) error { return n.handle.Teardown(ctx) }

// Fleet is a provisioned follower-node fleet handle.
type Fleet struct{ handle FleetHandle }

// Endpoints reads each follower SeiNode .status.endpoint into the 4-field leaf.
func (f *Fleet) Endpoints() FleetEndpoints { return f.handle.Endpoints() }

// Teardown deletes the fleet nodes this handle owns. Idempotent.
func (f *Fleet) Teardown(ctx context.Context) error { return f.handle.Teardown(ctx) }

func validateNetworkSpec(s NetworkSpec) error {
	switch {
	case strings.TrimSpace(s.Name) == "":
		return usageErr("NetworkSpec.Name is required")
	case strings.TrimSpace(s.ChainID) == "":
		return usageErr("NetworkSpec.ChainID is required")
	case strings.TrimSpace(s.Image) == "":
		return usageErr("NetworkSpec.Image is required")
	case s.Replicas < 1:
		return usageErr("NetworkSpec.Replicas must be >= 1, got %d", s.Replicas)
	}
	return nil
}

func validateFleetSpec(s FleetSpec) error {
	switch {
	case strings.TrimSpace(s.NamePrefix) == "":
		return usageErr("FleetSpec.NamePrefix is required")
	case strings.TrimSpace(s.Image) == "":
		return usageErr("FleetSpec.Image is required")
	case s.Replicas < 1:
		return usageErr("FleetSpec.Replicas must be >= 1, got %d", s.Replicas)
	}
	return nil
}
