package sei

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Shared test constants for the sei package tests (registry/Open env selection).
const (
	envNodeCluster = "SEI_NODE_CLUSTER"
	envLocal       = "SEI_LOCAL"
	envDocker      = "SEI_DOCKER"
)

// stubProvider is a no-op Provider for registry/Open tests.
type stubProvider struct{ name string }

func (s stubProvider) Name() string { return s.name }
func (stubProvider) CreateNetwork(context.Context, NetworkSpec) (NetworkHandle, error) {
	return nil, nil
}
func (stubProvider) GetNetwork(context.Context, string, string) (NetworkHandle, error) {
	return nil, nil
}
func (stubProvider) CreateNode(context.Context, NodeSpec) (NodeHandle, error) { return nil, nil }
func (stubProvider) GetNode(context.Context, string, string) (NodeHandle, error) {
	return nil, nil
}

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
	registerStub(modeK8s)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	registerStub(modeK8s)
}

func TestRegister_NilFactoryPanics(t *testing.T) {
	resetRegistry(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("nil factory Register did not panic")
		}
	}()
	RegisterProvider(modeK8s, nil)
}

func TestOpen_ExplicitModeResolves(t *testing.T) {
	resetRegistry(t)
	registerStub(modeK8s)
	c, err := Open(context.Background(), modeK8s)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := c.provider.Name(); got != modeK8s {
		t.Fatalf("resolved mode = %q, want k8s", got)
	}
}

func TestOpen_UnknownModeNamesTheBlankImport(t *testing.T) {
	resetRegistry(t)
	registerStub(modeK8s)
	_, err := Open(context.Background(), "postgres")
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
	if !strings.Contains(err.Error(), "blank import") {
		t.Fatalf("error should name the blank-import fix: %v", err)
	}
}

func TestResolveMode_EnvSelection(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr bool
	}{
		{"SEI_NODE_CLUSTER => k8s", map[string]string{envNodeCluster: "1"}, modeK8s, false},
		{"SEI_LOCAL => local", map[string]string{envLocal: "1"}, modeLocal, false},
		{"SEI_DOCKER => docker", map[string]string{envDocker: "1"}, modeDocker, false},
		{"two set => ambiguous", map[string]string{envNodeCluster: "1", envLocal: "1"}, "", true},
		{"all set => ambiguous", map[string]string{envNodeCluster: "1", envLocal: "1", envDocker: "1"}, "", true},
		{"none set => fail", map[string]string{}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{envNodeCluster, envLocal, envDocker} {
				_ = os.Unsetenv(k)
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			got, err := resolveMode("")
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveMode err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("resolved %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveMode_ExplicitBeatsEnv(t *testing.T) {
	t.Setenv(envNodeCluster, "1")
	t.Setenv(envLocal, "1") // ambiguous env...
	got, err := resolveMode(modeDocker)
	if err != nil {
		t.Fatalf("explicit mode should bypass env ambiguity: %v", err)
	}
	if got != modeDocker {
		t.Fatalf("resolved %q, want docker", got)
	}
}

func TestOpen_AmbiguousEnv_FailFastThroughOpen(t *testing.T) {
	resetRegistry(t)
	registerStub(modeK8s)
	registerStub(modeLocal)
	for _, k := range []string{envNodeCluster, envLocal, envDocker} {
		_ = os.Unsetenv(k)
	}
	t.Setenv(envNodeCluster, "1")
	t.Setenv(envLocal, "1")
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("ambiguous env should fail fast through Open")
	}
}
