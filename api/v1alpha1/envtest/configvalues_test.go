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
	// values and overrides are omitted from the spec when empty, so a case can
	// exercise one channel without implying anything about the other. mode is
	// the mode sub-spec to populate, defaulting to fullNode; frozen gives that
	// sub-spec a freeze height. An empty errorText means the node must be
	// admitted, and the stored object is then compared field for field.
	type admissionCase struct {
		name      string
		values    []any
		overrides map[string]any
		mode      string
		frozen    bool
		errorText string
	}
	cases := []admissionCase{
		{name: "typed-values", values: []any{entry("app.toml", "evm.enable", true), entry("app.toml", "evm.port", int64(8545)), entry("config.toml", "rpc.name", "node"), entry("config.toml", "rpc.ratio", 1.25), entry("app.toml", "custom.array", []any{true, int64(2)}), entry("app.toml", "custom.table", map[string]any{"enabled": false})}},
		{name: "absent", values: []any{map[string]any{"fileName": "app.toml", "key": "evm.enable"}}, errorText: "value: Required value"},
		{name: "null", values: []any{entry("app.toml", "evm.enable", nil)}, errorText: "value: Required value"},
		{name: "duplicate", values: []any{entry("app.toml", "evm.enable", true), entry("app.toml", "evm.enable", false)}, errorText: "Duplicate value"},
		{name: "different-files", values: []any{entry("app.toml", "evm.enable", true), entry("config.toml", "evm.enable", false)}},
		{name: "false-value", values: []any{entry("app.toml", "evm.enable", false)}},
		{name: "zero-value", values: []any{entry("app.toml", "evm.port", int64(0))}},
		{name: "plain-app", values: []any{entry("app.toml", "custom.key", true)}},
		{name: "traversal", values: []any{entry("../../secrets.toml", "custom.key", true)}, errorText: "fileName"},
		{name: "absolute-path", values: []any{entry("/etc/passwd", "custom.key", true)}, errorText: "fileName"},
		{name: "embedded-traversal", values: []any{entry("config.toml/../x", "custom.key", true)}, errorText: "fileName"},
		{name: "long-file", values: []any{entry(strings.Repeat("a", 60)+".toml", "custom.key", true)}, errorText: "fileName"},
		{name: "dot-key", values: []any{entry("app.toml", ".", true)}, errorText: "key"},
		{name: "empty-key-segments", values: []any{entry("app.toml", "a..", true)}, errorText: "key"},
		{name: "whitespace-key", values: []any{entry("app.toml", "   ", true)}, errorText: "key"},
		{name: "long-key", values: []any{entry("app.toml", strings.Repeat("a", 257), true)}, errorText: "key"},
		{name: "both-channels", values: []any{entry("app.toml", "evm.enable", false)}, overrides: map[string]any{"evm.enable": "true"}},
		{name: "nested-null", values: []any{entry("app.toml", "custom.table", map[string]any{"nested": nil})}},
		{name: "empty-file", values: []any{entry("", "evm.enable", true)}, errorText: "fileName"},
		{name: "empty-key", values: []any{entry("app.toml", "", true)}, errorText: "key"},
	}
	// The two channels diverge on the freeze and halt keys, and that divergence
	// is the contract, so both halves are generated over the same matrix and
	// asserted together.
	//
	// configValues is ADMITTED for every one of these keys, on purpose. Spec
	// 002-config-override-substrate, Requirement 1 criterion 7: "THE controller
	// SHALL apply a config value for any key, without a check against the
	// sei-config allow-list." Its Assumptions name this exact case: "A config
	// value can set a key the existing field's freeze and halt guards block.
	// The controller applies it. The operator accepts the outcome", and "The
	// owner signed off on this trade-off." An admission guard on this channel
	// is therefore a spec violation and not a hardening fix — commit 8117837
	// added one and it has been reverted. These cases exist to catch it coming
	// back; they are not a missing guard.
	//
	// overrides is still REJECTED for the same keys, because the same
	// Assumption closes with "The existing field keeps those guards." The
	// freeze-height guard fires on any node; the halt guards fire only on a
	// frozen one.
	for _, mode := range []string{"fullNode", "archive"} {
		for _, frozen := range []bool{false, true} {
			for _, key := range []string{"chain.freeze_height", "chain.halt_height", "chain.halt_time"} {
				suffix := strings.ToLower(mode) + "-" + strings.NewReplacer(".", "-", "_", "-").Replace(key)
				if frozen {
					suffix += "-frozen"
				}
				overridesError := ""
				switch {
				case key == "chain.freeze_height":
					overridesError = "set the freeze height via fullNode.freeze or archive.freeze, not overrides: user overrides outrank controller-derived ones"
				case frozen:
					overridesError = "a frozen node cannot also set chain.halt_height or chain.halt_time: seid refuses to load the combination"
				}
				cases = append(cases,
					admissionCase{
						name:   "configvalues-unguarded-" + suffix,
						values: []any{entry("app.toml", key, int64(12345))},
						mode:   mode,
						frozen: frozen,
					},
					admissionCase{
						name:      "overrides-guarded-" + suffix,
						overrides: map[string]any{key: "12345"},
						mode:      mode,
						frozen:    frozen,
						errorText: overridesError,
					},
				)
			}
		}
	}
	tooMany := make([]any, 101)
	for i := range tooMany {
		tooMany[i] = entry("app.toml", fmt.Sprintf("custom.key%d", i), true)
	}
	cases = append(cases, admissionCase{name: "too-many", values: tooMany, errorText: "must have at most 100 items"})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode := tc.mode
			if mode == "" {
				mode = "fullNode"
			}
			modeSpec := map[string]any{}
			if tc.frozen {
				modeSpec["freeze"] = map[string]any{"height": int64(100)}
			}
			spec := map[string]any{"chainId": "test", "image": "sei:latest", mode: modeSpec}
			if len(tc.values) > 0 {
				spec["configValues"] = tc.values
			}
			if len(tc.overrides) > 0 {
				spec["overrides"] = tc.overrides
			}
			node := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "sei.io/v1alpha1", "kind": "SeiNode",
				"metadata": map[string]any{"name": tc.name, "namespace": "default"},
				"spec":     spec,
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
			if len(tc.overrides) > 0 {
				overrides, _, err := unstructured.NestedMap(stored.Object, "spec", "overrides")
				if err != nil || !reflect.DeepEqual(tc.overrides, overrides) {
					t.Fatalf("overrides changed: want %#v, got %#v, error %v", tc.overrides, overrides, err)
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
