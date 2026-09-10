package tasks

import (
	"context"
	"encoding/json"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPatchHandlerTypedOverlay(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(path, []byte("untouched = 'base'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Model the HTTP/engine params map before the typed handler consumes it.
	var params map[string]any
	if err := json.Unmarshal([]byte(`{"files":{"app.toml":{"arbitrary":{"enabled":true,"count":42,"ratio":1.25,"label":"true"}}}}`), &params); err != nil {
		t.Fatal(err)
	}
	if _, err := NewConfigPatcher(home).Handler()(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	doc, err := tomlpatch.ReadTOML(path)
	if err != nil {
		t.Fatal(err)
	}
	values := doc["arbitrary"].(map[string]any)
	if values["enabled"] != true || values["count"] != int64(42) || values["ratio"] != float64(1.25) || values["label"] != "true" {
		t.Fatalf("wrong TOML types: %#v", values)
	}
	if doc["untouched"] != "base" {
		t.Fatal("base value erased")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "enabled = true") {
		t.Fatalf("boolean quoted: %s", raw)
	}
	t.Logf("SC-008 handler output:\n%s", raw)
}
