package planner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// Covers spec freeze-node: FN-7, and the constant/CEL pin the keyFreezeHeight
// comment promises.

func TestFreezeOverrides_EmitsHeight(t *testing.T) {
	got := freezeOverrides(&seiv1alpha1.FreezeSpec{Height: 12345})

	if want := "12345"; got[keyFreezeHeight] != want {
		t.Errorf("%s: got %q, want %q", keyFreezeHeight, got[keyFreezeHeight], want)
	}
	if len(got) != 1 {
		t.Errorf("freeze must contribute exactly one override; got %v", got)
	}
}

func TestFreezeOverrides_NilWhenUnfrozen(t *testing.T) {
	if got := freezeOverrides(nil); got != nil {
		t.Errorf("an unfrozen node must contribute no override; got %v", got)
	}
}

// FN-7 for both RPC-serving modes. The planners hold the mode in hand, so each
// reads its own sub-spec rather than the shared accessor.
func TestControllerOverrides_CarryFreezeHeight(t *testing.T) {
	tests := []struct {
		name string
		node *seiv1alpha1.SeiNode
		want string
	}{
		{
			name: "full node frozen",
			node: &seiv1alpha1.SeiNode{Spec: seiv1alpha1.SeiNodeSpec{
				FullNode: &seiv1alpha1.FullNodeSpec{
					Freeze: &seiv1alpha1.FreezeSpec{Height: 777},
				},
			}},
			want: "777",
		},
		{
			name: "archive frozen",
			node: &seiv1alpha1.SeiNode{Spec: seiv1alpha1.SeiNodeSpec{
				Archive: &seiv1alpha1.ArchiveSpec{
					Freeze: &seiv1alpha1.FreezeSpec{Height: 888},
				},
			}},
			want: "888",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got map[string]string
			switch {
			case tt.node.Spec.FullNode != nil:
				got = (&fullNodePlanner{}).controllerOverrides(tt.node)
			default:
				got = (&archiveNodePlanner{}).controllerOverrides(tt.node)
			}
			if got[keyFreezeHeight] != tt.want {
				t.Errorf("%s: got %q, want %q", keyFreezeHeight, got[keyFreezeHeight], tt.want)
			}
		})
	}
}

// FN-9 at the planner: an unfrozen node's overrides are unchanged, so the new
// branch cannot leak the key into every other node.
func TestControllerOverrides_OmitFreezeWhenUnfrozen(t *testing.T) {
	full := &seiv1alpha1.SeiNode{Spec: seiv1alpha1.SeiNodeSpec{
		FullNode: &seiv1alpha1.FullNodeSpec{},
	}}
	if got := (&fullNodePlanner{}).controllerOverrides(full); got[keyFreezeHeight] != "" {
		t.Errorf("an unfrozen full node must not carry %s; got %v", keyFreezeHeight, got)
	}

	archive := &seiv1alpha1.SeiNode{Spec: seiv1alpha1.SeiNodeSpec{
		Archive: &seiv1alpha1.ArchiveSpec{},
	}}
	if got := (&archiveNodePlanner{}).controllerOverrides(archive); got[keyFreezeHeight] != "" {
		t.Errorf("an unfrozen archive node must not carry %s; got %v", keyFreezeHeight, got)
	}
}

// TestFreezeHeightKeyMatchesCELGuard pins the Go constant to the literal in the
// SeiNodeSpec CEL marker. A kubebuilder marker cannot reference a constant, so
// the two spellings can drift silently; renaming one without the other would
// leave the guard watching a key the controller no longer emits.
//
// The rule is read from the parsed CRD, not the raw file: YAML escapes the
// inner quotes of a single-quoted scalar, so a text search matches neither
// spelling reliably.
func TestFreezeHeightKeyMatchesCELGuard(t *testing.T) {
	path := filepath.Join("..", "..", "manifests", "sei.io_seinodes.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated CRD: %v", err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("parsing generated CRD: %v", err)
	}

	var rules []string
	for _, version := range crd.Spec.Versions {
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			continue
		}
		spec, ok := version.Schema.OpenAPIV3Schema.Properties["spec"]
		if !ok {
			continue
		}
		for _, validation := range spec.XValidations {
			rules = append(rules, validation.Rule)
		}
	}
	if len(rules) == 0 {
		t.Fatal("generated CRD exposes no spec-level CEL rules; run `make manifests generate`")
	}

	want := "'" + keyFreezeHeight + "' in self.overrides"
	for _, rule := range rules {
		if strings.Contains(rule, want) {
			return
		}
	}
	t.Errorf("no spec-level CEL rule guards %q in overrides.\nrules: %v", keyFreezeHeight, rules)
}
