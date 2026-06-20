// Package local is the registered stub for the SEI_LOCAL in-process flavor. The
// interface is designed so an in-process provider can be added without touching
// core or the k8s provider, but the implementation (booting connected seid
// nodes in-process) is cut from the MVP (WS-E LLD §6.3). Every verb returns a
// ClassUsage "not implemented" error so a harness that selects "local" by env
// fails clearly rather than silently no-op'ing.
package local

import (
	"context"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
	"github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider"
)

func init() { provider.Register("local", New) }

const notImplemented = "SEI_LOCAL not implemented in MVP — only the k8s provider ships (LLD §6.3); use SEI_NODE_CLUSTER / the k8s flavor"

// Provider is the local stub.
type Provider struct{}

// New is the registered Factory. It succeeds (so Open resolves "local") but
// every provisioning verb fails with ClassUsage — the cut is honest, not a
// crash.
func New(context.Context) (provider.Provider, error) { return &Provider{}, nil }

func (*Provider) Name() string { return "local" }

func (*Provider) Close() error { return nil }

func (*Provider) ProvisionNetwork(context.Context, sei.NetworkSpec) (sei.NetworkHandle, error) {
	return nil, &sei.Error{Class: sei.ClassUsage, Err: errNotImplemented{}}
}

func (*Provider) ProvisionFleet(context.Context, sei.NetworkHandle, sei.FleetSpec) (sei.FleetHandle, error) {
	return nil, &sei.Error{Class: sei.ClassUsage, Err: errNotImplemented{}}
}

type errNotImplemented struct{}

func (errNotImplemented) Error() string { return notImplemented }
