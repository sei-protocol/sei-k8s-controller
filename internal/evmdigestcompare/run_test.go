package evmdigestcompare

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// value reads the current value of a single gauge or counter series.
func value(t *testing.T, m prometheus.Metric) float64 {
	t.Helper()
	var out dto.Metric
	if err := m.Write(&out); err != nil {
		t.Fatal(err)
	}
	if c := out.GetCounter(); c != nil {
		return c.GetValue()
	}
	return out.GetGauge().GetValue()
}

// fakeSource returns a fixed tip and a fixed report per Digest call, and
// records the heights it was asked to scan.
type fakeSource struct {
	tip       int64
	tipErr    error
	report    *Report
	digestErr error
	scanned   []int64
	backends  []string
}

func (f *fakeSource) Tip(context.Context) (int64, error) { return f.tip, f.tipErr }

func (f *fakeSource) Digest(_ context.Context, height int64, backend string) (*Report, error) {
	f.scanned = append(f.scanned, height)
	f.backends = append(f.backends, backend)
	if f.digestErr != nil {
		return nil, f.digestErr
	}
	return f.report, nil
}

const (
	testChain     = "arctic-1"
	nodeMigrating = "migrating-0"
	nodeReserve0  = "reserve-0"
	nodeReserve1  = "reserve-1"
)

func testNode(name, backend string) Node {
	return Node{Endpoint: Endpoint{Namespace: testChain, Name: name}, Backend: backend}
}

func testReport(digest string) *Report {
	var r Report
	r.Version = 1
	r.Final.Count = 42
	r.Final.Digest = digest
	return &r
}

func newTestRunner(names []string, sources map[string]*fakeSource) (*chainRunner, *Metrics) {
	m := NewMetrics(prometheus.NewRegistry())
	c := &Comparator{
		Metrics:   m,
		HeightLag: 20,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:       func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	chain := Chain{Name: testChain}
	srcs := make([]chainSource, 0, len(names))
	for _, name := range names {
		backend := BackendMemiavl
		if name == nodeMigrating {
			backend = BackendComposite
		}
		node := testNode(name, backend)
		chain.Nodes = append(chain.Nodes, node)
		srcs = append(srcs, chainSource{node: node, src: sources[name]})
	}
	r := &chainRunner{c: c, chain: chain, sources: srcs}
	m.initChain(chain, c.Now())
	return r, m
}

func TestRound_AllMatch(t *testing.T) {
	g := NewWithT(t)
	report := testReport("0xabc")
	sources := map[string]*fakeSource{
		nodeMigrating: {tip: 105, report: report},
		nodeReserve0:  {tip: 100, report: report},
		nodeReserve1:  {tip: 110, report: report},
	}
	r, m := newTestRunner([]string{nodeMigrating, nodeReserve0, nodeReserve1}, sources)

	r.round(t.Context())

	// H = minTip(100) - HeightLag(20) = 80, scanned on every node.
	g.Expect(sources[nodeMigrating].scanned).To(Equal([]int64{80}))
	g.Expect(sources[nodeReserve1].scanned).To(Equal([]int64{80}))
	g.Expect(sources[nodeMigrating].backends).To(Equal([]string{BackendComposite}))
	g.Expect(sources[nodeReserve0].backends).To(Equal([]string{BackendMemiavl}))

	g.Expect(value(t, m.ComparedHeights.WithLabelValues(testChain))).To(Equal(1.0))
	g.Expect(value(t, m.LastComparedHeight.WithLabelValues(testChain))).To(Equal(80.0))
	g.Expect(value(t, m.LastComparedTimestamp.WithLabelValues(testChain))).To(Equal(1_800_000_000.0))
	g.Expect(value(t, m.SourceHeight.WithLabelValues(testChain, testChain+"/"+nodeReserve0))).To(Equal(100.0))
	g.Expect(value(t, m.ScannedHeight.WithLabelValues(testChain, testChain+"/"+nodeMigrating))).To(Equal(80.0))
}

func TestRound_OneNodeDiverges(t *testing.T) {
	g := NewWithT(t)
	report := testReport("0xabc")
	sources := map[string]*fakeSource{
		nodeMigrating: {tip: 100, report: testReport("0xBAD")},
		nodeReserve0:  {tip: 100, report: report},
		nodeReserve1:  {tip: 100, report: report},
	}
	r, m := newTestRunner([]string{nodeMigrating, nodeReserve0, nodeReserve1}, sources)

	r.round(t.Context())

	// Every pair involving the diverging node latches; the agreeing pair stays 0.
	diverged := map[string]float64{}
	mismatches := map[string]float64{}
	pairs := [][2]string{
		{testChain + "/" + nodeMigrating, testChain + "/" + nodeReserve0},
		{testChain + "/" + nodeMigrating, testChain + "/" + nodeReserve1},
		{testChain + "/" + nodeReserve0, testChain + "/" + nodeReserve1},
	}
	for _, p := range pairs {
		diverged[p[0]+"|"+p[1]] = value(t, m.Diverged.WithLabelValues(testChain, p[0], p[1]))
		mismatches[p[0]+"|"+p[1]] = value(t, m.Mismatches.WithLabelValues(testChain, p[0], p[1]))
	}
	g.Expect(diverged[testChain+"/"+nodeMigrating+"|"+testChain+"/"+nodeReserve0]).To(Equal(1.0))
	g.Expect(diverged[testChain+"/"+nodeMigrating+"|"+testChain+"/"+nodeReserve1]).To(Equal(1.0))
	g.Expect(diverged[testChain+"/"+nodeReserve0+"|"+testChain+"/"+nodeReserve1]).To(BeZero())
	g.Expect(mismatches[testChain+"/"+nodeReserve0+"|"+testChain+"/"+nodeReserve1]).To(BeZero())
	g.Expect(value(t, m.ComparedHeights.WithLabelValues(testChain))).To(Equal(1.0))
}

func TestRound_UnreachableNodeStillComparesTheRest(t *testing.T) {
	g := NewWithT(t)
	report := testReport("0xabc")
	sources := map[string]*fakeSource{
		nodeMigrating: {tip: 100, report: report},
		nodeReserve0:  {tipErr: errors.New("connection refused")},
		nodeReserve1:  {tip: 100, report: report},
	}
	r, m := newTestRunner([]string{nodeMigrating, nodeReserve0, nodeReserve1}, sources)

	r.round(t.Context())

	g.Expect(sources[nodeReserve0].scanned).To(BeEmpty())
	g.Expect(value(t, m.SourceErrors.WithLabelValues(testChain, testChain+"/"+nodeReserve0, ReasonUnavailable))).To(Equal(1.0))
	g.Expect(value(t, m.ComparedHeights.WithLabelValues(testChain))).To(Equal(1.0))
}

func TestRound_SingleLiveNodeSkipsRound(t *testing.T) {
	g := NewWithT(t)
	sources := map[string]*fakeSource{
		nodeMigrating: {tip: 100, report: testReport("0xabc")},
		nodeReserve0:  {tipErr: errors.New("down")},
		nodeReserve1:  {tipErr: errors.New("down")},
	}
	r, m := newTestRunner([]string{nodeMigrating, nodeReserve0, nodeReserve1}, sources)

	r.round(t.Context())

	g.Expect(sources[nodeMigrating].scanned).To(BeEmpty())
	g.Expect(value(t, m.ComparedHeights.WithLabelValues(testChain))).To(BeZero())
}

func TestRound_ScanFailureSkipsOnlyThatNode(t *testing.T) {
	g := NewWithT(t)
	report := testReport("0xabc")
	sources := map[string]*fakeSource{
		nodeMigrating: {tip: 100, report: report},
		nodeReserve0:  {tip: 100, digestErr: errors.New("changelog ends mid-record")},
		nodeReserve1:  {tip: 100, report: report},
	}
	r, m := newTestRunner([]string{nodeMigrating, nodeReserve0, nodeReserve1}, sources)

	r.round(t.Context())

	g.Expect(value(t, m.SourceErrors.WithLabelValues(testChain, testChain+"/"+nodeReserve0, ReasonScanFailed))).To(Equal(1.0))
	g.Expect(value(t, m.ComparedHeights.WithLabelValues(testChain))).To(Equal(1.0))
}

func TestRound_HeightMustAdvance(t *testing.T) {
	g := NewWithT(t)
	report := testReport("0xabc")
	src := map[string]*fakeSource{
		nodeMigrating: {tip: 100, report: report},
		nodeReserve0:  {tip: 100, report: report},
	}
	r, m := newTestRunner([]string{nodeMigrating, nodeReserve0}, src)

	r.round(t.Context())
	r.round(t.Context()) // same tips → same H → no rescan
	g.Expect(src[nodeMigrating].scanned).To(Equal([]int64{80}))
	g.Expect(value(t, m.ComparedHeights.WithLabelValues(testChain))).To(Equal(1.0))

	src[nodeMigrating].tip = 150 // H advances to 130
	src[nodeReserve0].tip = 150
	r.round(t.Context())
	g.Expect(src[nodeMigrating].scanned).To(Equal([]int64{80, 130}))
	g.Expect(value(t, m.LastComparedHeight.WithLabelValues(testChain))).To(Equal(130.0))
}

func TestInitChain_ExportsZeroSeries(t *testing.T) {
	g := NewWithT(t)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.initChain(Chain{
		Name: testChain,
		Nodes: []Node{
			testNode(nodeMigrating, BackendComposite),
			testNode(nodeReserve0, BackendMemiavl),
			testNode(nodeReserve1, BackendMemiavl),
		},
	}, time.Unix(1_800_000_000, 0))

	families, err := reg.Gather()
	g.Expect(err).NotTo(HaveOccurred())
	series := map[string]int{}
	for _, f := range families {
		series[f.GetName()] = len(f.GetMetric())
	}
	g.Expect(series).To(HaveKeyWithValue("sei_evmdigest_compare_diverged", 3))
	g.Expect(series).To(HaveKeyWithValue("sei_evmdigest_compare_mismatches_total", 3))
	g.Expect(series).To(HaveKeyWithValue("sei_evmdigest_compare_last_compared_timestamp_seconds", 1))
	g.Expect(series).To(HaveKeyWithValue("sei_evmdigest_compare_source_errors_total", 3*3))
}
