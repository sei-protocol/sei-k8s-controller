package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/sei-protocol/sei-k8s-controller/sidecar/engine"
	"github.com/sei-protocol/sei-k8s-controller/sidecar/tasks"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch"
)

func TestConfigOverlayHTTPPreservesLargeInteger(t *testing.T) {
	home := t.TempDir()
	if _, err := tasks.NewConfigApplier(home).Handler()(context.Background(), map[string]any{"mode": "full"}); err != nil {
		t.Fatal(err)
	}
	eng := newTestEngine(t, map[engine.TaskType]engine.TaskHandler{
		"config-patch": tasks.NewConfigPatcher(home).Handler(),
	})
	srv := NewServer(":0", eng, home, AuthnModeUnauthenticated)
	rec := serveHTTP(srv, http.MethodPost, "/v0/tasks",
		`{"type":"config-patch","params":{"files":{"app.toml":{"boundary":9007199254740993}}}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	var submitted map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &submitted); err != nil {
		t.Fatal(err)
	}
	if result := waitForTaskResult(eng, submitted["id"]); result == nil {
		t.Fatal("task did not complete")
	}
	doc, err := tomlpatch.ReadTOML(filepath.Join(home, "config", "app.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := doc["boundary"]; got != int64(9007199254740993) {
		t.Fatalf("integer rounded: got %v (%T), want 9007199254740993", got, got)
	}
	t.Log("HTTP -> engine -> typed config-patch -> TOML: 9007199254740993 preserved exactly")
}
