package faults

import (
	"slices"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/yaml"
)

var params = Params{
	ChainID: "bench-a", RunID: "r1", Namespace: "eng-x", Duration: "3m",
	Node: "bench-a-rpc-1", Followers: []string{"bench-a-rpc-0"},
}

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

func TestRender_NodeTargetedFaultNeedsANodeInTheChain(t *testing.T) {
	g := NewWithT(t)
	f, err := LookupExtended("container-kill-node")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(f.NeedsNode).To(BeTrue())
	base := Params{ChainID: "c", RunID: "r", Namespace: "n"}

	_, err = f.Render(base)
	g.Expect(err).To(MatchError(ContainSubstring("node is required")))

	// A name outside the chain would let the selector reach another chain's pod.
	bad := base
	bad.Node = "other-rpc-0"
	_, err = f.Render(bad)
	g.Expect(err).To(MatchError(ContainSubstring("does not belong to chain")))

	ok := base
	ok.Node = "c-rpc-1"
	out, err := f.Render(ok)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(out)).To(ContainSubstring(`values: ["c-rpc-1"]`))

	// The state-sync follower is named <chain>-statesync. Naming it statesync-<chain>
	// instead was rejected here, which made the scenario unrunnable until a review
	// caught it; this pins both sides of that rule.
	ss := base
	ss.Node = "c-statesync"
	_, err = f.Render(ss)
	g.Expect(err).NotTo(HaveOccurred())
	ss.Node = "statesync-c"
	_, err = f.Render(ss)
	g.Expect(err).To(MatchError(ContainSubstring("does not belong to chain")))
}

// TestRender_NodeIsReadOnlyByNodeTargetedFaults guards the converse of NeedsNode:
// a template that interpolates {{.Node}} must be flagged NeedsNode, or Render never
// checks the field and a caller who leaves it empty gets an empty selector.
func TestRender_NodeIsReadOnlyByNodeTargetedFaults(t *testing.T) {
	const sentinel = "sentinel-node-name"
	for _, f := range append(slices.Clone(Catalog), ExtendedCatalog...) {
		if f.NeedsNode {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			g := NewWithT(t)
			out, err := f.Render(Params{ChainID: "c", RunID: "r", Namespace: "n", Duration: "1m", Node: sentinel})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(string(out)).NotTo(ContainSubstring(sentinel),
				"%s reads Params.Node but is not flagged NeedsNode, so Render would not validate it", f.Name)
		})
	}
}

func TestRender_FollowersMustBelongToTheChain(t *testing.T) {
	g := NewWithT(t)
	f, err := LookupExtended("network-partition-split")
	g.Expect(err).NotTo(HaveOccurred())
	_, err = f.Render(Params{ChainID: "c", RunID: "r", Namespace: "n", Duration: "1m", Followers: []string{"other-rpc-0"}})
	g.Expect(err).To(MatchError(ContainSubstring("does not belong to chain")))

	out, err := f.Render(Params{ChainID: "c", RunID: "r", Namespace: "n", Duration: "1m"})
	g.Expect(err).NotTo(HaveOccurred(), "a chain with no followers renders a split of the validators alone")
	g.Expect(string(out)).NotTo(ContainSubstring(`"c-rpc`), "no follower names are templated in")
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
			// How a selector pins the chain is checked precisely, on both sides, by
			// TestExtendedCatalog_BlastRadius; here only that the run's chain id is templated in.
			g.Expect(string(out)).To(ContainSubstring(`"bench-a`), "selector must carry the run's chain id")
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

// crSelector is the selector shape shared by spec.selector and spec.target.selector.
type crSelector struct {
	Namespaces          []string `json:"namespaces"`
	ExpressionSelectors []struct {
		Key      string   `json:"key"`
		Operator string   `json:"operator"`
		Values   []string `json:"values"`
	} `json:"expressionSelectors"`
}

// crSpec is the part of a rendered fault the blast-radius tests read.
type crSpec struct {
	Spec struct {
		Mode     string     `json:"mode"`
		Selector crSelector `json:"selector"`
		Target   *struct {
			Mode     string     `json:"mode"`
			Selector crSelector `json:"selector"`
		} `json:"target"`
	} `json:"spec"`
}

// nodePins returns the values of the selector's sei.io/node In-pin, or nil.
func (s crSelector) nodePins() []string {
	for _, es := range s.ExpressionSelectors {
		if es.Key == "sei.io/node" && es.Operator == "In" {
			return es.Values
		}
	}
	return nil
}

// pinsChain reports whether the selector is confined to the run's own chain: a
// sei.io/nodedeployment In-pin on the chain id, or a sei.io/node In-pin whose
// every value belongs to the chain (names start with "<chain>-").
func (s crSelector) pinsChain(chain string) bool {
	for _, es := range s.ExpressionSelectors {
		if es.Key == "sei.io/nodedeployment" && es.Operator == "In" && len(es.Values) == 1 && es.Values[0] == chain {
			return true
		}
	}
	pins := s.nodePins()
	if len(pins) == 0 {
		return false
	}
	for _, v := range pins {
		if !strings.HasPrefix(v, chain+"-") {
			return false
		}
	}
	return true
}

// TestExtendedCatalog_BlastRadius pins what each extended fault may hit, the
// counterpart of TestCatalog_BlastRadius, and it reads BOTH selectors: a
// NetworkChaos target.selector that lost its chain pin would widen the blast
// radius to every pod in the namespace while a test that parsed only
// spec.selector stayed green.
//
// Every selector, on both sides, must be confined to the run's namespace and
// its own chain. Beyond that a fault is either f=1 (one pod, or a pin to
// validator-0) or flagged QuorumLoss, and a QuorumLoss fault must really take
// quorum away: a kill of every validator, or a split with no side holding 2/3
// of four validators. A fault flagged QuorumLoss that did not would run without
// the under-fault liveness check for no reason, and the reverse would fail that
// check on a chain that is supposed to be live.
func TestExtendedCatalog_BlastRadius(t *testing.T) {
	for _, f := range ExtendedCatalog {
		t.Run(f.Name, func(t *testing.T) {
			g := NewWithT(t)
			out, err := f.Render(Params{
				ChainID: "c", RunID: "r", Namespace: "ns", Duration: "1m",
				Node: "c-rpc-2", Followers: splitFollowers,
			})
			g.Expect(err).NotTo(HaveOccurred())
			var cr crSpec
			g.Expect(yaml.Unmarshal(out, &cr)).To(Succeed())

			selectors := []crSelector{cr.Spec.Selector}
			if cr.Spec.Target != nil {
				selectors = append(selectors, cr.Spec.Target.Selector)
			}
			for i, sel := range selectors {
				g.Expect(sel.Namespaces).To(Equal([]string{"ns"}), "selector %d must be pinned to the run's namespace", i)
				g.Expect(sel.pinsChain("c")).To(BeTrue(), "selector %d must be confined to the run's chain", i)
			}

			pinned := len(cr.Spec.Selector.nodePins())
			switch {
			case !f.QuorumLoss:
				g.Expect(cr.Spec.Mode == "one" || pinned == 1).To(BeTrue(),
					"an f=1 fault must target one pod: mode=%q pinned=%d", cr.Spec.Mode, pinned)
			case f.OneShot:
				g.Expect(cr.Spec.Mode).To(Equal("all"), "a QuorumLoss kill must take every validator")
				g.Expect(pinned).To(BeZero(), "a QuorumLoss kill must not pin a subset")
			default:
				g.Expect(cr.Spec.Target).NotTo(BeNil(), "a partition has a target side")
				// mode: one would have chaos-mesh pick a single pod from a side, which
				// leaves quorum intact and makes the split a no-op.
				g.Expect(cr.Spec.Mode).To(Equal("all"), "a split must apply to every pod on the source side")
				g.Expect(cr.Spec.Target.Mode).To(Equal("all"), "a split must apply to every pod on the target side")
				assertSplitHasNoQuorum(g, cr.Spec.Selector.nodePins(), cr.Spec.Target.Selector.nodePins(), splitFollowers)
			}
		})
	}
}

// splitFollowers are the followers the blast-radius test renders the split with.
// Two, so that a template which handled only the first would fail.
var splitFollowers = []string{"c-rpc-0", "c-rpc-1"}

// assertSplitHasNoQuorum checks a 2|2 split of four validators (c-0 .. c-3) and
// that every RPC follower sits on the source side. A follower on neither side
// bridges the split, because it peers with every validator and relays votes,
// and one on each side would too.
func assertSplitHasNoQuorum(g *WithT, side, other, followers []string) {
	isFollower := func(v string) bool { return strings.Contains(v, "-rpc-") }
	validators := func(names []string) (n int) {
		for _, v := range names {
			if !isFollower(v) {
				n++
			}
		}
		return n
	}
	g.Expect(validators(side)).To(Equal(2), "each side must hold exactly two of the four validators")
	g.Expect(validators(other)).To(Equal(2), "each side must hold exactly two of the four validators")

	count := func(names []string, want string) (n int) {
		for _, v := range names {
			if v == want {
				n++
			}
		}
		return n
	}
	// The sides must be disjoint and together name all four validators: a name on
	// both sides, or a validator on neither, is not a 2|2 split of the four.
	all := append(slices.Clone(side), other...)
	for _, want := range []string{"c-0", "c-1", "c-2", "c-3"} {
		g.Expect(count(all, want)).To(Equal(1), "validator %s must be on exactly one side", want)
	}

	// Every follower the run has must be on the source side, once: left out, or on
	// the target side, it bridges the partition. Nothing else may be listed.
	for _, f := range followers {
		g.Expect(count(side, f)).To(Equal(1), "follower %s must be on the source side", f)
		g.Expect(count(other, f)).To(BeZero(), "follower %s must not be on the target side", f)
	}
	g.Expect(len(side)-validators(side)).To(Equal(len(followers)), "the source side lists exactly the run's followers")
}

// TestCatalog_HoldsNoQuorumLossFault guards what QuorumLoss's doc comment
// promises: the nightly Catalog is the f=1 set. A quorum-loss fault there would
// turn the nightly red by design.
func TestCatalog_HoldsNoQuorumLossFault(t *testing.T) {
	for _, f := range Catalog {
		t.Run(f.Name, func(t *testing.T) {
			NewWithT(t).Expect(f.QuorumLoss).To(BeFalse(), "Catalog is the f=1 set the nightly suite runs")
		})
	}
}
