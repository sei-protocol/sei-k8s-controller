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

	versions, ok := crd["spec"].(map[string]any)["versions"].([]any)
	if !ok || len(versions) == 0 {
		t.Fatalf("%s: no served versions", path)
	}
	schema := versions[0].(map[string]any)["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
	specProps := schema["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)
	values, ok := specProps["configValues"].(map[string]any)
	if !ok {
		t.Fatalf("%s: spec.configValues is absent", path)
	}
	return stripDescriptions(values).(map[string]any)
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
