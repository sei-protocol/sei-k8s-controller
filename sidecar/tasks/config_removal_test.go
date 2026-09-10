package tasks

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch"
)

// Exercise the actual config-apply and config-patch handlers, including removal
// of the final entry (no overlay), a changed entry, and base-value restoration.
func TestConfigRegenerationRemovesOverlayFromGeneratedFiles(t *testing.T) {
	for _, file := range []string{"config.toml", "app.toml"} {
		t.Run(file, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			apply := NewConfigApplier(home).Handler()
			patch := NewConfigPatcher(home).Handler()
			params := map[string]any{"mode": "full"}
			if _, err := apply(ctx, params); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "config", file)
			base, err := tomlpatch.ReadTOML(path)
			if err != nil {
				t.Fatal(err)
			}
			key := "moniker"
			if file == "app.toml" {
				key = "minimum-gas-prices"
			}
			if _, exists := base[key]; !exists {
				t.Fatalf("base key %q missing", key)
			}
			initial := map[string]any{"files": map[string]any{file: map[string]any{
				key: "operator-value", "removed-custom-key": true, "kept-custom-key": int64(1),
			}}}
			if _, err := patch(ctx, initial); err != nil {
				t.Fatal(err)
			}
			// Recompute after dropping two entries and changing a third.
			if _, err := apply(ctx, params); err != nil {
				t.Fatal(err)
			}
			if _, err := patch(ctx, map[string]any{"files": map[string]any{
				file: map[string]any{"kept-custom-key": int64(2)},
			}}); err != nil {
				t.Fatal(err)
			}
			got, err := tomlpatch.ReadTOML(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got[key], base[key]) {
				t.Fatalf("base value not restored: %v", got[key])
			}
			if _, exists := got["removed-custom-key"]; exists {
				t.Fatal("removed unknown key survived regeneration")
			}
			if got["kept-custom-key"] != int64(2) {
				t.Fatalf("changed entry: %v", got["kept-custom-key"])
			}
			// Remove the last configValue: base regeneration alone must suffice.
			if _, err := apply(ctx, params); err != nil {
				t.Fatal(err)
			}
			got, err = tomlpatch.ReadTOML(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, base) {
				t.Fatal("removing all entries did not restore base")
			}
		})
	}
}

func TestConfigRegenerationLeavesNonGeneratedFileEntriesInPlace(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	apply := NewConfigApplier(home).Handler()
	if _, err := apply(ctx, map[string]any{"mode": "full"}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewConfigPatcher(home).Handler()(ctx, map[string]any{"files": map[string]any{
		"extra.toml": map[string]any{"retained": true},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := apply(ctx, map[string]any{"mode": "full"}); err != nil {
		t.Fatal(err)
	}
	got, err := tomlpatch.ReadTOML(filepath.Join(home, "config", "extra.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got["retained"] != true {
		t.Fatalf("documented non-generated file behavior changed: %v", got)
	}
}
