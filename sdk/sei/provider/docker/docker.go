// Package docker is the registered stub for the SEI_DOCKER mode. The interface is
// shaped so a docker-compose-backed provider can be added without touching core
// or the k8s provider, but the implementation is not built. Every verb returns a
// clear "mode not implemented" error so a harness that selects "docker" by env
// fails clearly rather than silently no-op'ing.
package docker

import (
	"context"
	"errors"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider"
)

func init() { provider.Register("docker", New) }

// ErrNotImplemented is returned by every docker-mode verb.
var ErrNotImplemented = errors.New("docker mode not implemented — only the k8s mode ships; use SEI_NODE_CLUSTER / the k8s mode")

// Provider is the docker stub.
type Provider struct{}

// New is the registered Factory. It succeeds (so Open resolves "docker") but
// every verb fails with ErrNotImplemented — the cut is honest, not a crash.
func New(context.Context) (provider.Provider, error) { return &Provider{}, nil }

func (*Provider) Name() string { return "docker" }

func (*Provider) CreateNetwork(context.Context, sei.NetworkSpec) (sei.NetworkHandle, error) {
	return nil, ErrNotImplemented
}

func (*Provider) GetNetwork(context.Context, string, string) (sei.NetworkHandle, error) {
	return nil, ErrNotImplemented
}

func (*Provider) CreateNode(context.Context, sei.NodeSpec) (sei.NodeHandle, error) {
	return nil, ErrNotImplemented
}

func (*Provider) GetNode(context.Context, string, string) (sei.NodeHandle, error) {
	return nil, ErrNotImplemented
}
