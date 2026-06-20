package sei

import (
	"context"
	"sort"
	"sync"
)

// Provider is the flavor contract. The core *Client is a thin typed facade over
// exactly these methods. k8s implements it for MVP; local is a registered stub
// (LLD §4.2). It lives in core so providers depend on core, not vice-versa —
// the public provider.Provider is an alias of this type.
type Provider interface {
	Name() string
	ProvisionNetwork(ctx context.Context, spec NetworkSpec) (NetworkHandle, error)
	ProvisionFleet(ctx context.Context, net NetworkHandle, spec FleetSpec) (FleetHandle, error)
	Close() error
}

// NetworkHandle is the provider-side state the core *Network wraps. Endpoints()
// reads typed status; Teardown() deletes; Name()/Namespace() identify the
// resource so ProvisionFleet wires followers' peer discovery at the genesis
// network's actual location — a network in namespace A must yield A, not the
// provider default.
type NetworkHandle interface {
	Name() string
	Namespace() string
	Endpoints() Endpoints
	Teardown(ctx context.Context) error
}

// FleetHandle is the provider-side state the core *Fleet wraps.
type FleetHandle interface {
	Endpoints() FleetEndpoints
	Teardown(ctx context.Context) error
}

// Factory builds a provider. Deferred so registration (init time) does no I/O —
// the kube client is built lazily at Open time from the ambient kubeconfig
// chain (LLD §4.2). No dsn arg for MVP (§3.1); structured config is an additive
// append to this signature when a provider needs it.
type Factory func(ctx context.Context) (Provider, error)

// registry holds the registered factories. Structure mirrors database/sql's
// drivers map: init() runs single-threaded before main so registration races
// are impossible, but the RWMutex guards the Open-time read against pathological
// dynamic registration.
var registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// RegisterProvider makes a provider available by name. It is the registration
// entry point a driver package's init() calls (via provider.Register, the
// public façade). A duplicate name or nil factory panics — a programming error
// surfaced at process start, not a runtime branch (mirrors sql.Register).
func RegisterProvider(name string, factory Factory) {
	if factory == nil {
		panic("sei: RegisterProvider factory is nil")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.factories == nil {
		registry.factories = make(map[string]Factory)
	}
	if _, dup := registry.factories[name]; dup {
		panic("sei: RegisterProvider called twice for provider " + name)
	}
	registry.factories[name] = factory
}

// lookupFactory returns the registered factory for name.
func lookupFactory(name string) (Factory, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	f, ok := registry.factories[name]
	return f, ok
}

// registeredNames returns the registered provider names, sorted — used to make
// the "forgotten blank import?" error name what IS available.
func registeredNames() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	names := make([]string, 0, len(registry.factories))
	for n := range registry.factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
