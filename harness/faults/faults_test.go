package faults

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

var params = Params{ChainID: "bench-a", RunID: "r1", Namespace: "eng-x", Duration: "3m"}

func TestCatalog_EveryFaultRendersToItsKind(t *testing.T) {
	for _, f := range Catalog {
		t.Run(f.Name, func(t *testing.T) {
			g := NewWithT(t)
			out, err := f.Render(params)
			g.Expect(err).NotTo(HaveOccurred())

			var m map[string]any
			g.Expect(yaml.Unmarshal(out, &m)).To(Succeed())
			g.Expect(m["kind"]).To(Equal(f.Kind))
			g.Expect(f.Resource()).To(Equal(strings.ToLower(f.Kind)))

			meta := m["metadata"].(map[string]any)
			g.Expect(meta["name"]).To(HaveSuffix("-r1"), "CR name must carry the run id")
			g.Expect(meta["namespace"]).To(Equal("eng-x"))
			g.Expect(meta["labels"]).To(HaveKeyWithValue("sei.io/harness-run", "r1"))

			spec := m["spec"].(map[string]any)
			_, hasDuration := spec["duration"]
			g.Expect(hasDuration).To(Equal(!f.OneShot), "spec.duration presence must match OneShot")
			g.Expect(string(out)).To(ContainSubstring(`values: ["bench-a"]`), "selector must pin the pool")
			g.Expect(string(out)).NotTo(ContainSubstring("{{"), "unrendered template action")
		})
	}
}

func TestLookup(t *testing.T) {
	g := NewWithT(t)
	f, err := Lookup("pod-failure")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(f.OneShot).To(BeTrue())

	_, err = Lookup("dns-chaos")
	g.Expect(err).To(MatchError(ContainSubstring("unknown fault \"dns-chaos\"")))
	g.Expect(Names()).To(HaveLen(len(Catalog)))
}

func TestRender_RejectsIncompleteParams(t *testing.T) {
	partition, _ := Lookup("network-partition")
	kill, _ := Lookup("pod-failure")
	cases := []struct {
		name  string
		f     Fault
		p     Params
		wants string
	}{
		{"missing chain id", partition, Params{RunID: "r", Namespace: "n", Duration: "1m"}, "chainID"},
		{"missing run id", partition, Params{ChainID: "c", Namespace: "n", Duration: "1m"}, "runID"},
		{"missing namespace", partition, Params{ChainID: "c", RunID: "r", Duration: "1m"}, "namespace"},
		{"bad duration", partition, Params{ChainID: "c", RunID: "r", Namespace: "n", Duration: "soon"}, "duration"},
		{"zero duration", partition, Params{ChainID: "c", RunID: "r", Namespace: "n", Duration: "0s"}, "positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			_, err := tc.f.Render(tc.p)
			g.Expect(err).To(MatchError(ContainSubstring(tc.wants)))
		})
	}

	g := NewWithT(t)
	_, err := kill.Render(Params{ChainID: "c", RunID: "r", Namespace: "n"})
	g.Expect(err).NotTo(HaveOccurred(), "one-shot faults take no duration")
}

// TestCatalog_BlastRadius pins the f=1 contract the package doc states: every
// non-MeshWide fault hits exactly one validator, either through mode: one or
// a sei.io/node=<chainID>-0 pin; MeshWide faults select the whole pool.
func TestCatalog_BlastRadius(t *testing.T) {
	for _, f := range Catalog {
		t.Run(f.Name, func(t *testing.T) {
			g := NewWithT(t)
			out, err := f.Render(Params{ChainID: "c", RunID: "r", Namespace: "ns", Duration: "1m"})
			g.Expect(err).NotTo(HaveOccurred())
			var cr struct {
				Spec struct {
					Mode     string `json:"mode"`
					Selector struct {
						ExpressionSelectors []struct {
							Key      string   `json:"key"`
							Operator string   `json:"operator"`
							Values   []string `json:"values"`
						} `json:"expressionSelectors"`
					} `json:"selector"`
				} `json:"spec"`
			}
			g.Expect(yaml.Unmarshal(out, &cr)).To(Succeed())

			pinnedToValidator0 := false
			for _, es := range cr.Spec.Selector.ExpressionSelectors {
				if es.Key == "sei.io/node" && es.Operator == "In" && len(es.Values) == 1 && es.Values[0] == "c-0" {
					pinnedToValidator0 = true
				}
			}
			if f.MeshWide {
				g.Expect(cr.Spec.Mode).To(Equal("all"))
				g.Expect(pinnedToValidator0).To(BeFalse(), "MeshWide fault must not pin validator-0")
				return
			}
			g.Expect(cr.Spec.Mode == "one" || pinnedToValidator0).To(BeTrue(),
				"fault must target one pod: mode=%q pinned=%v", cr.Spec.Mode, pinnedToValidator0)
		})
	}
}
