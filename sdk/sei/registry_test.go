package sei

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// Shared test constants for the sei package tests (registry/Open env selection).
// Provider-name literals reuse the source consts (providerK8s/providerLocal).
const (
	envProvider    = "SEI_PROVIDER"
	envNodeCluster = "SEI_NODE_CLUSTER"
	envLocal       = "SEI_LOCAL"
)

// stubProvider is a no-op Provider for registry/Open tests.
type stubProvider struct{ name string }

func (s stubProvider) Name() string { return s.name }
func (stubProvider) ProvisionNetwork(context.Context, NetworkSpec) (NetworkHandle, error) {
	return nil, nil
}
func (stubProvider) ProvisionFleet(context.Context, NetworkHandle, FleetSpec) (FleetHandle, error) {
	return nil, nil
}
func (stubProvider) Close() error { return nil }

// resetRegistry clears the package registry so each test starts clean. Tests in
// this file run sequentially (no t.Parallel) because they mutate global state.
func resetRegistry(t *testing.T) {
	t.Helper()
	registry.mu.Lock()
	registry.factories = nil
	registry.mu.Unlock()
}

func registerStub(name string) {
	RegisterProvider(name, func(context.Context) (Provider, error) {
		return stubProvider{name: name}, nil
	})
}

func TestRegister_DuplicatePanics(t *testing.T) {
	resetRegistry(t)
	registerStub(providerK8s)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	registerStub(providerK8s)
}

func TestRegister_NilFactoryPanics(t *testing.T) {
	resetRegistry(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("nil factory Register did not panic")
		}
	}()
	RegisterProvider(providerK8s, nil)
}

func TestOpen_ExplicitNameResolves(t *testing.T) {
	resetRegistry(t)
	registerStub(providerK8s)
	c, err := Open(context.Background(), providerK8s)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := c.provider.Name(); got != providerK8s {
		t.Fatalf("resolved provider = %q, want k8s", got)
	}
}

func TestOpen_UnknownNameNamesTheBlankImport(t *testing.T) {
	resetRegistry(t)
	registerStub(providerK8s)
	_, err := Open(context.Background(), "postgres")
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !IsUsage(t, err) {
		t.Fatalf("want ClassUsage, got %v", err)
	}
	// The error must name the fix — the forgotten blank import.
	if !strings.Contains(err.Error(), "blank import") {
		t.Fatalf("error should name the blank-import fix: %v", err)
	}
}

func TestOpen_EnvSelection(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{"SEI_PROVIDER wins", map[string]string{envProvider: providerK8s}, providerK8s, false},
		{"SEI_NODE_CLUSTER => k8s", map[string]string{envNodeCluster: "1"}, providerK8s, false},
		{"SEI_LOCAL => local", map[string]string{envLocal: "1"}, providerLocal, false},
		{"both set => fail fast", map[string]string{envNodeCluster: "1", envLocal: "1"}, "", true},
		{"none set => fail", map[string]string{}, "", true},
		{
			"SEI_PROVIDER beats presence ambiguity",
			map[string]string{envProvider: providerLocal, envNodeCluster: "1", envLocal: "1"},
			providerLocal, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{envProvider, envNodeCluster, envLocal} {
				_ = os.Unsetenv(k)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := resolveFlavor("")
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveFlavor err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if got != tc.want {
					t.Fatalf("resolved %q, want %q", got, tc.want)
				}
			} else if !IsUsage(t, err) {
				t.Fatalf("flavor-selection error should be ClassUsage, got %v", err)
			}
		})
	}
}

func TestOpen_BothEnvSet_FailFastThroughOpen(t *testing.T) {
	resetRegistry(t)
	registerStub(providerK8s)
	registerStub(providerLocal)
	for _, k := range []string{envProvider, envNodeCluster, envLocal} {
		_ = os.Unsetenv(k)
	}
	t.Setenv(envNodeCluster, "1")
	t.Setenv(envLocal, "1")
	if _, err := Open(context.Background(), ""); err == nil || !IsUsage(t, err) {
		t.Fatalf("both-set should fail fast with ClassUsage through Open, got %v", err)
	}
}

// IsUsage reports whether err is a ClassUsage SDK error.
func IsUsage(t *testing.T, err error) bool {
	t.Helper()
	var e *Error
	return errors.As(err, &e) && e.Class == ClassUsage
}
