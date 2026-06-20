package k8s

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultNamespace_TrimsSANamespaceNewline pins the trim contract: the
// projected SA-namespace file carries a trailing newline, and an untrimmed
// value would propagate a stray "\n" into every defaulted namespace (e.g. a
// NamespacedName Get against "nightly\n" 404s). POD_NAMESPACE is cleared so the
// file path is the one under test.
func TestDefaultNamespace_TrimsSANamespaceNewline(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")

	saFile := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(saFile, []byte("nightly\n"), 0o600); err != nil {
		t.Fatalf("write SA-namespace fixture: %v", err)
	}

	if got := defaultNamespace(saFile); got != "nightly" {
		t.Errorf("defaultNamespace = %q, want %q (trailing newline not trimmed)", got, "nightly")
	}
}

// TestDefaultNamespace_FallsBackToDefault confirms an unreadable SA-file falls
// through to "default" (POD_NAMESPACE unset, file absent).
func TestDefaultNamespace_FallsBackToDefault(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "")
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if got := defaultNamespace(missing); got != "default" {
		t.Errorf("defaultNamespace = %q, want %q", got, "default")
	}
}
