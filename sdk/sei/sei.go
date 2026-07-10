// Package sei is a thin, typed, stateless, multi-mode API for
// SeiNetwork/SeiNode lifecycle. It mirrors database/sql: a provider registers in
// init(), the consumer blank-imports it, and Open selects the mode by name.
//
// The SDK ships WIRE providers — they speak RPC/CRDs to an already-running chain
// (k8s over the controller's CRDs; docker over container-per-node RPC), so
// pinning the SDK to a chain release is fine. It deliberately does NOT host an
// in-process ("local") provider: that would link sei-chain's Go source, which the
// controller cannot transitively build and which would co-version the harness
// with the chain. An in-process harness lives in sei-chain instead; it satisfies
// the Provider/*Handle interfaces by shape (structural typing) and is registered
// or constructed consumer-side via RegisterProvider — the SDK does not ship it.
//
// The SDK is a CRUD layer, NOT an orchestrator. The flow is: create a network ->
// wait ready -> create RPC nodes as peers -> wait ready -> run tests against the
// returned handles. Orchestration — cleanup, GC, rollback, composition — is the
// caller's job. The Client and providers hold only the mode connection; they
// never track provisioned resources (the runtime owns that state).
package sei

import (
	"context"
	"os"
	"strings"
)

// Open selects a provisioning mode and returns a Client bound to it. The mode's
// provider must have been registered via a blank import of its package (see
// package provider).
//
// With mode == "", Open resolves from env presence: SEI_NODE_CLUSTER => "k8s",
// SEI_DOCKER => "docker". More than one present (or none) is an error — never
// guess. The k8s provider resolves its config from the ambient kubeconfig chain;
// there is no caller-supplied connection string. A consumer that registers its
// own provider (e.g. an in-process harness) passes its mode name explicitly;
// only the SDK's built-in wire modes participate in env resolution.
func Open(ctx context.Context, mode string) (*Client, error) {
	resolved, err := resolveMode(mode)
	if err != nil {
		return nil, err
	}
	factory, ok := lookupFactory(resolved)
	if !ok {
		return nil, usageErr(
			"unknown mode %q (registered: %v) — forgotten blank import? "+
				`add: import _ "github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/%s"`,
			resolved, registeredNames(), resolved)
	}
	p, err := factory(ctx)
	if err != nil {
		return nil, err
	}
	return &Client{provider: p}, nil
}

// Mode names the core matches in env-presence detection — the SDK's built-in
// wire modes only. Kept in sync with the keys the provider packages pass to
// Register (the literals are duplicated there; nothing mechanical enforces the
// match). A consumer-registered mode is selected by passing its name to Open, not
// by env.
const (
	modeK8s    = "k8s"
	modeDocker = "docker"
)

// resolveMode picks the mode: explicit arg wins, else env presence (exactly one
// of SEI_NODE_CLUSTER / SEI_DOCKER).
func resolveMode(mode string) (string, error) {
	if mode != "" {
		return mode, nil
	}
	var present []string
	if _, ok := os.LookupEnv("SEI_NODE_CLUSTER"); ok {
		present = append(present, modeK8s)
	}
	if _, ok := os.LookupEnv("SEI_DOCKER"); ok {
		present = append(present, modeDocker)
	}
	switch len(present) {
	case 1:
		return present[0], nil
	case 0:
		return "", usageErr("no mode selected — pass a mode to Open, or set exactly one of SEI_NODE_CLUSTER / SEI_DOCKER")
	default:
		return "", usageErr("mode is ambiguous: %s all set — pass an explicit mode to Open", strings.Join(present, ", "))
	}
}

// Client is the mode-bound handle a harness holds. Stateless beyond the mode
// connection: safe for sequential use; not goroutine-safe across calls (the k8s
// provider's SSA field-owner is single-writer).
type Client struct {
	provider Provider
}

// CreateNetwork creates a SeiNetwork and returns a handle immediately — it does
// NOT wait for readiness. Call Network.WaitReady to block on it.
func (c *Client) CreateNetwork(ctx context.Context, spec NetworkSpec) (*Network, error) {
	if err := validateNetworkSpec(spec); err != nil {
		return nil, err
	}
	h, err := c.provider.CreateNetwork(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &Network{handle: h}, nil
}

// GetNetwork reads an existing SeiNetwork into a handle. A missing resource
// surfaces as the provider's not-found error (k8s: apierrors.IsNotFound).
func (c *Client) GetNetwork(ctx context.Context, name, namespace string) (*Network, error) {
	h, err := c.provider.GetNetwork(ctx, name, namespace)
	if err != nil {
		return nil, err
	}
	return &Network{handle: h}, nil
}

// CreateNode creates one SeiNode peered to spec.Network and returns a handle
// immediately. The caller loops CreateNode for N RPC nodes; there is no fan-out
// here.
func (c *Client) CreateNode(ctx context.Context, spec NodeSpec) (*Node, error) {
	if err := validateNodeSpec(spec); err != nil {
		return nil, err
	}
	h, err := c.provider.CreateNode(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &Node{handle: h}, nil
}

// GetNode reads an existing SeiNode into a handle.
func (c *Client) GetNode(ctx context.Context, name, namespace string) (*Node, error) {
	h, err := c.provider.GetNode(ctx, name, namespace)
	if err != nil {
		return nil, err
	}
	return &Node{handle: h}, nil
}

// Network is a handle to a SeiNetwork. Endpoint getters read the
// runtime's status verbatim — never reconstructed.
type Network struct{ handle NetworkHandle }

// Name is the SeiNetwork resource name.
func (n *Network) Name() string { return n.handle.Name() }

// Namespace is where the SeiNetwork lives.
func (n *Network) Namespace() string { return n.handle.Namespace() }

// TendermintRPC is the aggregate Tendermint RPC URL off .status; "" until Ready.
func (n *Network) TendermintRPC() string { return n.handle.TendermintRPC() }

// REST is the aggregate Cosmos REST URL off .status; "" until Ready.
func (n *Network) REST() string { return n.handle.REST() }

// WaitReady blocks until the network reaches the Ready phase and a light serve-
// probe passes, or the caller's ctx fires (IsTimeout on a deadline).
func (n *Network) WaitReady(ctx context.Context) error { return n.handle.WaitReady(ctx) }

// Delete removes the network. Caller-invoked: the SDK never auto-deletes.
// Idempotent — a not-found is success.
func (n *Network) Delete(ctx context.Context) error { return n.handle.Delete(ctx) }

// Object returns the mode-specific raw resource (k8s: *v1alpha1.SeiNetwork) for
// callers that need fields the mode-agnostic surface does not expose. The caller
// type-asserts; a provider with no native object (e.g. a non-k8s wire mode)
// returns nil.
func (n *Network) Object() any { return n.handle.Object() }

// Node is a handle to a SeiNode.
type Node struct{ handle NodeHandle }

// Name is the SeiNode resource name.
func (n *Node) Name() string { return n.handle.Name() }

// Namespace is where the SeiNode lives.
func (n *Node) Namespace() string { return n.handle.Namespace() }

// EVMRPC is the node's EVM JSON-RPC URL off .status; "" until it serves EVM.
func (n *Node) EVMRPC() string { return n.handle.EVMRPC() }

// TendermintRPC is the node's Tendermint RPC URL off .status.
func (n *Node) TendermintRPC() string { return n.handle.TendermintRPC() }

// REST is the node's Cosmos REST (LCD) URL off .status; "" unless the node
// serves REST (fullNode RPCs do; bare validators do not unless configured).
func (n *Node) REST() string { return n.handle.REST() }

// WaitReady blocks until the node reaches the Running phase and a light serve-
// probe passes, or the caller's ctx fires (IsTimeout on a deadline).
func (n *Node) WaitReady(ctx context.Context) error { return n.handle.WaitReady(ctx) }

// Delete removes the node. Caller-invoked; idempotent.
func (n *Node) Delete(ctx context.Context) error { return n.handle.Delete(ctx) }

// Object returns the mode-specific raw resource (k8s: *v1alpha1.SeiNode).
func (n *Node) Object() any { return n.handle.Object() }

func validateNetworkSpec(s NetworkSpec) error {
	switch {
	case strings.TrimSpace(s.Name) == "":
		return usageErr("NetworkSpec.Name is required")
	case strings.TrimSpace(s.Image) == "":
		return usageErr("NetworkSpec.Image is required")
	case s.Validators < 1:
		return usageErr("NetworkSpec.Validators must be >= 1, got %d", s.Validators)
	}
	return nil
}

func validateNodeSpec(s NodeSpec) error {
	switch {
	case strings.TrimSpace(s.Name) == "":
		return usageErr("NodeSpec.Name is required")
	case strings.TrimSpace(s.Network) == "":
		return usageErr("NodeSpec.Network is required (the peer-wire target)")
	case strings.TrimSpace(s.Image) == "":
		return usageErr("NodeSpec.Image is required")
	case s.StateSync != nil && len(s.StateSync.RpcServers) < 2:
		return usageErr("NodeSpec.StateSync.RpcServers must have >= 2 entries (the state-sync witness floor), got %d", len(s.StateSync.RpcServers))
	}
	return nil
}
