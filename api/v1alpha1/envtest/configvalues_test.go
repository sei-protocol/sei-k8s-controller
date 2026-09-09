//go:build envtest

package envtest_test

import (
	"context"
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
		{"empty-file", []any{entry("", "evm.enable", true)}, "fileName"},
		{"empty-key", []any{entry("app.toml", "", true)}, "key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "sei.io/v1alpha1", "kind": "SeiNode",
				"metadata": map[string]any{"name": tc.name, "namespace": "default"},
				"spec":     map[string]any{"chainId": "test", "image": "sei:latest", "fullNode": map[string]any{}, "configValues": tc.values},
			}}
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
