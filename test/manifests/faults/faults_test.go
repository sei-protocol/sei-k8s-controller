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

func TestExtendedCatalog_EveryFaultRendersToItsKind(t *testing.T) {
	for _, f := range ExtendedCatalog {
		t.Run(f.Name, func(t *testing.T) {
			g := NewWithT(t)
			out, err := f.Render(params)
			g.Expect(err).NotTo(HaveOccurred())

			var m map[string]any
			g.Expect(yaml.Unmarshal(out, &m)).To(Succeed())
			g.Expect(m["kind"]).To(Equal(f.Kind))

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

func TestExtendedCatalog_NamesAreDisjointFromCatalog(t *testing.T) {
	g := NewWithT(t)
	for _, name := range ExtendedNames() {
		_, err := Lookup(name)
		g.Expect(err).To(HaveOccurred(),
			"%s must not be reachable through Lookup: the nightly suite and seictl iterate Catalog", name)
	}
	g.Expect(ExtendedNames()).To(HaveLen(len(ExtendedCatalog)))
}

func TestLookupExtended(t *testing.T) {
	g := NewWithT(t)
	f, err := LookupExtended("container-kill-all")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(f.QuorumLoss).To(BeTrue())

	_, err = LookupExtended("pod-failure")
	g.Expect(err).To(MatchError(ContainSubstring(`unknown extended fault "pod-failure"`)))
}

// TestExtendedCatalog_BlastRadius pins what each extended fault may hit, the
// counterpart of TestCatalog_BlastRadius. A fault is either f=1 (one pod, or a
// pin to validator-0) or flagged QuorumLoss, and a QuorumLoss fault must really
// take quorum away: a kill of every validator, or a split with no side holding
// 2/3 of four validators. A fault flagged QuorumLoss that did not would run
// without the under-fault liveness check for no reason, and the reverse would
// fail that check on a chain that is supposed to be live.
func TestExtendedCatalog_BlastRadius(t *testing.T) {
	for _, f := range ExtendedCatalog {
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

			pinned := 0 // validators the sei.io/node In-pin names
			for _, es := range cr.Spec.Selector.ExpressionSelectors {
				if es.Key == "sei.io/node" && es.Operator == "In" {
					pinned = len(es.Values)
				}
			}

			switch {
			case !f.QuorumLoss:
				g.Expect(cr.Spec.Mode == "one" || pinned == 1).To(BeTrue(),
					"an f=1 fault must target one pod: mode=%q pinned=%d", cr.Spec.Mode, pinned)
			case f.OneShot:
				g.Expect(cr.Spec.Mode).To(Equal("all"), "a QuorumLoss kill must take every validator")
				g.Expect(pinned).To(BeZero(), "a QuorumLoss kill must not pin a subset")
			default:
				// A partition: neither side may hold 2/3 of four validators.
				g.Expect(pinned).To(Equal(2), "a split must put exactly half the validators on one side")
			}
		})
	}
}
