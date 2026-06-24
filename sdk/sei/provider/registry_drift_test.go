package provider_test

// External test package + own binary: the only registry writers here are the
// three provider blank imports, so package sei's resetRegistry can't race it.

import (
	"slices"
	"testing"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"

	_ "github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/docker"
	_ "github.com/sei-protocol/sei-k8s-controller/sdk/sei/provider/k8s"
)

// TestProviderKeysMatchOpenModes pins the SDK's built-in wire providers to
// exactly the modes Open resolves by env ("k8s", "docker"). Open's doc notes the
// key match is a duplicated literal with nothing mechanical enforcing it; this is
// that enforcement — a drifted or extra built-in key fails the build instead of
// failing Open at runtime. Consumer-registered modes are out of scope here: they
// are not blank-imported by the SDK and are selected by explicit name, not env.
func TestProviderKeysMatchOpenModes(t *testing.T) {
	registered := sei.RegisteredProviders()
	want := []string{"docker", "k8s"}
	slices.Sort(registered)
	if !slices.Equal(registered, want) {
		t.Errorf("SDK built-in providers = %v, want exactly %v", registered, want)
	}
}
