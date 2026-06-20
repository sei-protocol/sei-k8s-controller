// Package provider is the public driver-registration façade — the database/sql
// model for the sei SDK. A flavor package's init() calls Register; a consumer
// blank-imports the flavor package for that side effect. The Provider interface
// and handle types live in core (package sei) so flavors depend on core, not
// vice-versa; this package re-exports them as the stable public names.
package provider

import "github.com/sei-protocol/sei-k8s-controller/sdk/sei"

// Provider is the flavor contract (alias of sei.Provider).
type Provider = sei.Provider

// Factory builds a provider (alias of sei.Factory).
type Factory = sei.Factory

// Register makes a provider available by name. Called from a driver package's
// init(). Duplicate name or nil factory panics — a programming error surfaced
// at process start (mirrors sql.Register).
func Register(name string, factory Factory) {
	sei.RegisterProvider(name, factory)
}
