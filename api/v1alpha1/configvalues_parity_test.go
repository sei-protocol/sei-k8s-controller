package v1alpha1_test

import (
	"os"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"
)

// Spec 003-config-substrate-parity-seinetwork requires the SeiNetwork's
// config-value field to carry the same name and the same shape as the
// SeiNode's, so an operator writes one thing in both places and the network's
// values are accepted by the child field verbatim. Markers are the shape here,
// so the comparison is over the generated schemas rather than the Go types.
// Descriptions are dropped: each side documents its own scope.
func TestConfigValuesSchemaParity(t *testing.T) {
	node := configValuesSchema(t, "../../config/crd/sei.io_seinodes.yaml")
	network := configValuesSchema(t, "../../config/crd/sei.io_seinetworks.yaml")

	if !reflect.DeepEqual(node, network) {
		t.Fatalf("spec.configValues shape differs between the CRDs:\nSeiNode:    %#v\nSeiNetwork: %#v", node, network)
	}
}

// configValuesSchema reads spec.configValues out of a CRD manifest with every
// description stripped, at any depth.
func configValuesSchema(t *testing.T, path string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	crd := map[string]any{}
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}

	versions, ok := descend(crd, "spec")["versions"].([]any)
	if !ok || len(versions) == 0 {
		t.Fatalf("%s: no served versions", path)
	}
	version, ok := versions[0].(map[string]any)
	if !ok {
		t.Fatalf("%s: malformed version entry", path)
	}
	values := descend(version, "schema", "openAPIV3Schema", "properties", "spec", "properties", "configValues")
	if values == nil {
		t.Fatalf("%s: spec.configValues is absent", path)
	}
	stripped, ok := stripDescriptions(values).(map[string]any)
	if !ok {
		t.Fatalf("%s: spec.configValues is not an object", path)
	}
	return stripped
}

// descend walks a chain of object keys, returning nil at the first key that is
// missing or does not hold an object, so a reshaped manifest reports through
// t.Fatalf rather than panicking.
func descend(node map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		next, ok := node[key].(map[string]any)
		if !ok {
			return nil
		}
		node = next
	}
	return node
}

func stripDescriptions(node any) any {
	switch typed := node.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, v := range typed {
			if k == "description" {
				continue
			}
			out[k] = stripDescriptions(v)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, v := range typed {
			out[i] = stripDescriptions(v)
		}
		return out
	default:
		return node
	}
}
