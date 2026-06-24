package sei

import (
	"context"
	"sort"
	"sync"
)

// The Provider/*Handle interfaces, the *Spec structs (spec.go), and the registry
// are the SDK's stable, dependency-light CONTRACT — the seam a consumer's
// in-process harness conforms to by shape, and the boundary a future leaf module
// would extract. This contract surface MUST stay free of controller-runtime /
// apimachinery / CRD imports; those belong to the k8s provider package alone, the
// concrete object surfacing only through Object() any.

// Provider is the flavor contract — a thin, stateless CRUD driver. The core
// *Client is a typed facade over exactly these methods. k8s implements it as a
// real wire provider; docker is a registered wire stub. A consumer may register
// its own provider (e.g. sei-chain's in-process harness) — the SDK ships none
// beyond the wire modes. It lives in core so providers depend on core, not
// vice-versa; the public provider.Provider is an alias of this type.
//
// A provider holds only the mode connection/config (kube client, etc.) — never a
// registry, cache, or tracking of provisioned resources. The runtime owns
// resource state; Get reads it back from the runtime, not from the provider.
type Provider interface {
	Name() string
	CreateNetwork(ctx context.Context, spec NetworkSpec) (NetworkHandle, error)
	GetNetwork(ctx context.Context, name, namespace string) (NetworkHandle, error)
	CreateNode(ctx context.Context, spec NodeSpec) (NodeHandle, error)
	GetNode(ctx context.Context, name, namespace string) (NodeHandle, error)
	RunTask(ctx context.Context, spec TaskSpec) (TaskHandle, error)
	GetTask(ctx context.Context, name, namespace string) (TaskHandle, error)
}

// NetworkHandle is the provider-side state the core *Network wraps. It exposes
// endpoints off the runtime's status, a light readiness wait, deletion, and the
// raw underlying object for the mode-specific escape hatch.
type NetworkHandle interface {
	Name() string
	Namespace() string
	TendermintRPC() string
	REST() string
	WaitReady(ctx context.Context) error
	Delete(ctx context.Context) error
	Object() any // mode-specific raw resource (k8s: *v1alpha1.SeiNetwork)
}

// NodeHandle is the provider-side state the core *Node wraps.
type NodeHandle interface {
	Name() string
	Namespace() string
	EVMRPC() string
	TendermintRPC() string
	REST() string
	WaitReady(ctx context.Context) error
	Delete(ctx context.Context) error
	Object() any // mode-specific raw resource (k8s: *v1alpha1.SeiNode)
}

// TaskHandle is the provider-side state the core *Task wraps. A SeiNodeTask is
// one-shot: WaitComplete blocks to a terminal phase and returns the typed
// outputs, rather than exposing long-lived endpoint getters like the
// network/node handles.
type TaskHandle interface {
	Name() string
	Namespace() string
	WaitComplete(ctx context.Context) (*TaskOutputs, error)
	Delete(ctx context.Context) error
	Object() any // mode-specific raw resource (k8s: *v1alpha1.SeiNodeTask)
}

// Factory builds a provider. Deferred so registration (init time) does no I/O —
// the mode connection is built lazily at Open time. No dsn arg; structured
// config is an additive append when a provider needs it.
type Factory func(ctx context.Context) (Provider, error)

// registry holds the registered factories — driver registration ONLY. It tracks
// no provisioned resources. Structure mirrors database/sql's drivers map: init()
// runs single-threaded before main, but the RWMutex guards the Open-time read.
var registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// RegisterProvider makes a provider available by name — the entry point a driver
// package's init() calls (via provider.Register). A duplicate name or nil factory
// panics: a programming error surfaced at process start (mirrors sql.Register).
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

// RegisteredProviders returns the names providers have registered under, sorted —
// the public mirror of sql.Drivers().
func RegisteredProviders() []string { return registeredNames() }
