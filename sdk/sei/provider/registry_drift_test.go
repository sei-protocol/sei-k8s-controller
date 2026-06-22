package provider_test

// External test package + own binary: the only registry writers here are the
// three provider blank imports, so package sei's resetRegistry can't race it.

import (
	"slices"
	"testing"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"

	_ "github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/docker"
	_ "github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/k8s"
	_ "github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/local"
)

// TestProviderKeysMatchOpenModes pins each provider's Register key to the mode
// literal Open resolves to ("k8s"/"local"/"docker"). Open's doc notes this match
// is by duplicated literal with nothing mechanical enforcing it; this is that
// enforcement — a drifted key fails the build instead of failing Open at runtime.
func TestProviderKeysMatchOpenModes(t *testing.T) {
	registered := sei.RegisteredProviders()
	for _, mode := range []string{"k8s", "local", "docker"} {
		if !slices.Contains(registered, mode) {
			t.Errorf("mode %q resolvable by Open but no provider Registered under it; registered: %v", mode, registered)
		}
	}
}
