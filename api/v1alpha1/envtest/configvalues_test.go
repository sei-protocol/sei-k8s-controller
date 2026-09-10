//go:build envtest

package envtest_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// Only SeiNode is needed to exercise admission of configValues.
// Kubernetes 1.34 prunes explicit null from this non-nullable JSON field, then
// rejects the missing required value. CEL cannot access the untyped field; the
// probes below retain that evidence without adding an unusable schema rule.
func TestConfigValuesAdmission(t *testing.T) {
	ctx := context.Background()
	environment := &envtest.Environment{}
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../../config/crd/sei.io_seinodes.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, crd); err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"self.value != null", "has(self.value) && self.value != null"} {
		t.Run(rule, func(t *testing.T) {
			candidate := crd.DeepCopy()
			spec := candidate.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
			values := spec.Properties["configValues"]
			values.Items.Schema.XValidations = []apiextensionsv1.ValidationRule{{Rule: rule}}
			err := cli.Create(ctx, candidate)
			if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "undefined field 'value'") {
				t.Fatalf("expected inaccessible untyped field, got %v", err)
			}
			t.Logf("API-server CEL result: %v", err)
		})
	}
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{CRDs: []*apiextensionsv1.CustomResourceDefinition{crd}}); err != nil {
		t.Fatal(err)
	}
	entry := func(file, key string, value any) map[string]any {
		return map[string]any{"fileName": file, "key": key, "value": value}
	}
	cases := []struct {
		name      string
		values    []any
		errorText string
	}{
		{"typed-values", []any{entry("app.toml", "evm.enable", true), entry("app.toml", "evm.port", int64(8545)), entry("config.toml", "rpc.name", "node"), entry("config.toml", "rpc.ratio", 1.25), entry("app.toml", "custom.array", []any{true, int64(2)}), entry("app.toml", "custom.table", map[string]any{"enabled": false})}, ""},
		{"absent", []any{map[string]any{"fileName": "app.toml", "key": "evm.enable"}}, "value: Required value"},
		{"null", []any{entry("app.toml", "evm.enable", nil)}, "value: Required value"},
		{"duplicate", []any{entry("app.toml", "evm.enable", true), entry("app.toml", "evm.enable", false)}, "Duplicate value"},
		{"different-files", []any{entry("app.toml", "evm.enable", true), entry("config.toml", "evm.enable", false)}, ""},
		{"false-value", []any{entry("app.toml", "evm.enable", false)}, ""},
		{"zero-value", []any{entry("app.toml", "evm.port", int64(0))}, ""},
		{"plain-app", []any{entry("app.toml", "custom.key", true)}, ""},
		{"traversal", []any{entry("../../secrets.toml", "custom.key", true)}, "fileName"},
		{"absolute-path", []any{entry("/etc/passwd", "custom.key", true)}, "fileName"},
		{"embedded-traversal", []any{entry("config.toml/../x", "custom.key", true)}, "fileName"},
		{"long-file", []any{entry(strings.Repeat("a", 60)+".toml", "custom.key", true)}, "fileName"},
		{"dot-key", []any{entry("app.toml", ".", true)}, "key"},
		{"empty-key-segments", []any{entry("app.toml", "a..", true)}, "key"},
		{"whitespace-key", []any{entry("app.toml", "   ", true)}, "key"},
		{"long-key", []any{entry("app.toml", strings.Repeat("a", 257), true)}, "key"},
		{"both-channels", []any{entry("app.toml", "evm.enable", false)}, ""},
		{"nested-null", []any{entry("app.toml", "custom.table", map[string]any{"nested": nil})}, ""},
		{"empty-file", []any{entry("", "evm.enable", true)}, "fileName"},
		{"empty-key", []any{entry("app.toml", "", true)}, "key"},
	}
	for _, mode := range []string{"fullNode", "archive"} {
		for _, frozen := range []bool{false, true} {
			for _, key := range []string{"chain.freeze_height", "chain.halt_height", "chain.halt_time"} {
				name := strings.ToLower(mode) + "-" + strings.ReplaceAll(key, ".", "-")
				errorText := ""
				if key == "chain.freeze_height" {
					errorText = "set the freeze height via fullNode.freeze or archive.freeze, not configValues: user configValues outrank controller-derived ones"
				} else if frozen {
					errorText = "a frozen node cannot also set chain.halt_height or chain.halt_time: seid refuses to load the combination"
				}
				if frozen {
					name += "-frozen"
				}
				cases = append(cases, struct {
					name      string
					values    []any
					errorText string
				}{name, []any{entry("app.toml", key, int64(10))}, errorText})
			}
		}
	}
	tooMany := make([]any, 101)
	for i := range tooMany {
		tooMany[i] = entry("app.toml", fmt.Sprintf("custom.key%d", i), true)
	}
	cases = append(cases, struct {
		name      string
		values    []any
		errorText string
	}{"too-many", tooMany, "must have at most 100 items"})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "sei.io/v1alpha1", "kind": "SeiNode",
				"metadata": map[string]any{"name": tc.name, "namespace": "default"},
				"spec":     map[string]any{"chainId": "test", "image": "sei:latest", "fullNode": map[string]any{}, "configValues": tc.values},
			}}
			spec := node.Object["spec"].(map[string]any)
			mode := "fullNode"
			if strings.HasPrefix(tc.name, "archive-") {
				delete(spec, "fullNode")
				mode = "archive"
				spec[mode] = map[string]any{}
			}
			if strings.HasSuffix(tc.name, "-frozen") {
				spec[mode] = map[string]any{"freeze": map[string]any{"height": int64(100)}}
			}
			if tc.name == "both-channels" {
				spec["overrides"] = map[string]any{"evm.enable": "true"}
			}
			err := cli.Create(ctx, node)
			if tc.errorText != "" {
				if !apierrors.IsInvalid(err) || !strings.Contains(err.Error(), tc.errorText) {
					t.Fatalf("expected %q, got %v", tc.errorText, err)
				}
				t.Logf("API-server rejection: %v", err)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			stored := &unstructured.Unstructured{}
			stored.SetGroupVersionKind(node.GroupVersionKind())
			if err := cli.Get(ctx, client.ObjectKeyFromObject(node), stored); err != nil {
				t.Fatal(err)
			}
			if tc.name == "both-channels" {
				overrides, _, err := unstructured.NestedStringMap(stored.Object, "spec", "overrides")
				if err != nil || !reflect.DeepEqual(overrides, map[string]string{"evm.enable": "true"}) {
					t.Fatalf("overrides changed: got %#v, error %v", overrides, err)
				}
			}
			values, _, err := unstructured.NestedSlice(stored.Object, "spec", "configValues")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(tc.values, values) {
				t.Fatalf("typed values changed: want %#v, got %#v", tc.values, values)
			}
		})
	}
}
