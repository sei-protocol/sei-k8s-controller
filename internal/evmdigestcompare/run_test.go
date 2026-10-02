package evmdigestcompare

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/wire"
)

const testChain = "arctic-1"

var (
	migratingNode = Node{Namespace: testChain, Name: "rpc-node-0-0", Backend: wire.EVMDigestComposite}
	reserveNode   = Node{Namespace: testChain, Name: "memiavl-rpc-0-0", Backend: wire.EVMDigestMemIAVL}
)

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

func newTestGroup(migrating, reserve *fakeSource) (*groupRunner, *Metrics) {
	m := NewMetrics(prometheus.NewRegistry())
	c := &Comparator{
		Metrics:          m,
		HeightLag:        10,
		TaskPollInterval: time.Millisecond,
		ScanTimeout:      50 * time.Millisecond,
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:              func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	g := &groupRunner{c: c, chain: testChain, nodes: []*nodeRunner{
		{node: migratingNode, label: migratingNode.Label(), src: migrating},
		{node: reserveNode, label: reserveNode.Label(), src: reserve},
	}}
	m.initGroup(testChain, []string{migratingNode.Label(), reserveNode.Label()}, time.Unix(1_700_000_000, 0))
	return g, m
}

func TestRound_MatchScansEveryNodeBelowLowestTip(t *testing.T) {
	g := NewWithT(t)
	migrating, reserve := newFakeSource(1000, digestOf("ab", 7)), newFakeSource(990, digestOf("ab", 7))
	gr, m := newTestGroup(migrating, reserve)

	gr.round(t.Context())
	for _, src := range []*fakeSource{migrating, reserve} {
		g.Expect(src.submits).To(HaveLen(1))
		g.Expect((*src.submits[0].Params)["height"]).To(Equal(int64(980)))
	}
	g.Expect((*migrating.submits[0].Params)["backend"]).To(Equal("composite"))
	g.Expect((*reserve.submits[0].Params)["backend"]).To(Equal("memiavl"))
	g.Expect(value(t, m.Rounds.WithLabelValues(testChain, ResultMatch))).To(Equal(1.0))
	g.Expect(value(t, m.Diverged.WithLabelValues(testChain))).To(BeZero())
	g.Expect(value(t, m.LastComparedHeight.WithLabelValues(testChain))).To(Equal(980.0))
	g.Expect(value(t, m.LastComparedTimestamp.WithLabelValues(testChain))).To(Equal(1_800_000_000.0))
	g.Expect(value(t, m.ScanEntries.WithLabelValues(testChain, reserveNode.Label()))).To(Equal(7.0))
	g.Expect(value(t, m.ScanDuration.WithLabelValues(testChain, reserveNode.Label()))).To(Equal(12.0))
	g.Expect(value(t, m.SourceHeight.WithLabelValues(testChain, migratingNode.Label()))).To(Equal(1000.0))

	// The tips have not moved: no second scan at the same height.
	gr.round(t.Context())
	g.Expect(migrating.submits).To(HaveLen(1))
	g.Expect(value(t, m.Rounds.WithLabelValues(testChain, ResultMatch))).To(Equal(1.0))
}

func TestRound_MismatchLatchesDiverged(t *testing.T) {
	cases := map[string]*fakeSource{
		"digest": newFakeSource(1000, digestOf("cd", 7)),
		"count":  newFakeSource(1000, digestOf("ab", 8)),
		"version": newFakeSource(1000, func(h int64) *wire.EVMLogicalDigestResult {
			r := digestOf("ab", 7)(h)
			r.Version = h - 1
			return r
		}),
	}
	for name, migrating := range cases {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			reserve := newFakeSource(1000, digestOf("ab", 7))
			gr, m := newTestGroup(migrating, reserve)

			gr.round(t.Context())
			g.Expect(value(t, m.Diverged.WithLabelValues(testChain))).To(Equal(1.0))
			g.Expect(value(t, m.Mismatches.WithLabelValues(testChain))).To(Equal(1.0))
			g.Expect(value(t, m.Rounds.WithLabelValues(testChain, ResultMismatch))).To(Equal(1.0))

			// A later matching round does not clear the latch.
			migrating.digest = digestOf("ab", 7)
			*migrating.height, *reserve.height = 1100, 1100
			gr.round(t.Context())
			g.Expect(value(t, m.Rounds.WithLabelValues(testChain, ResultMatch))).To(Equal(1.0))
			g.Expect(value(t, m.Diverged.WithLabelValues(testChain))).To(Equal(1.0))
		})
	}
}

func TestRound_UnreadableTipSkipsRound(t *testing.T) {
	g := NewWithT(t)
	migrating, reserve := newFakeSource(1000, digestOf("ab", 7)), newFakeSource(1000, digestOf("ab", 7))
	reserve.statusErr = errors.New("connection refused")
	gr, m := newTestGroup(migrating, reserve)

	gr.round(t.Context())
	g.Expect(migrating.submits).To(BeEmpty())
	g.Expect(value(t, m.ScanErrors.WithLabelValues(testChain, reserveNode.Label(), ReasonUnavailable))).To(Equal(1.0))
	g.Expect(value(t, m.Rounds.WithLabelValues(testChain, ResultIncomplete))).To(Equal(1.0))
	g.Expect(value(t, m.LastComparedTimestamp.WithLabelValues(testChain))).To(Equal(1_700_000_000.0))
}

func TestRound_FailedScanIsIncompleteNotMismatch(t *testing.T) {
	g := NewWithT(t)
	migrating := newFakeSource(1000, func(int64) *wire.EVMLogicalDigestResult { return nil })
	reserve := newFakeSource(1000, digestOf("ab", 7))
	gr, m := newTestGroup(migrating, reserve)

	gr.round(t.Context())
	g.Expect(value(t, m.ScanErrors.WithLabelValues(testChain, migratingNode.Label(), ReasonFailed))).To(Equal(1.0))
	g.Expect(value(t, m.Rounds.WithLabelValues(testChain, ResultIncomplete))).To(Equal(1.0))
	g.Expect(value(t, m.Diverged.WithLabelValues(testChain))).To(BeZero())
	g.Expect(value(t, m.LastComparedHeight.WithLabelValues(testChain))).To(BeZero())
}

func TestRound_TimedOutScanIsCancelled(t *testing.T) {
	g := NewWithT(t)
	migrating, reserve := newFakeSource(1000, digestOf("ab", 7)), newFakeSource(1000, digestOf("ab", 7))
	migrating.stuck = true
	gr, m := newTestGroup(migrating, reserve)

	gr.round(t.Context())
	g.Expect(value(t, m.ScanErrors.WithLabelValues(testChain, migratingNode.Label(), ReasonTimeout))).To(Equal(1.0))
	g.Expect(migrating.deleted).To(Equal([]uuid.UUID{*migrating.submits[0].Id}))
	g.Expect(value(t, m.Rounds.WithLabelValues(testChain, ResultIncomplete))).To(Equal(1.0))
}

func TestRound_CancelsScansLeftByAnotherRun(t *testing.T) {
	g := NewWithT(t)
	migrating, reserve := newFakeSource(1000, digestOf("ab", 7)), newFakeSource(1000, digestOf("ab", 7))
	stale := taskID(testChain, migratingNode.Label(), 500)
	migrating.tasks[stale] = &sidecar.TaskResult{Id: stale, Type: sidecar.TaskTypeEVMLogicalDigest, Status: sidecar.Running}
	gr, m := newTestGroup(migrating, reserve)

	gr.round(t.Context())
	g.Expect(migrating.deleted).To(Equal([]uuid.UUID{stale}))
	g.Expect(value(t, m.Rounds.WithLabelValues(testChain, ResultMatch))).To(Equal(1.0))
}

func TestTaskIDIsDeterministic(t *testing.T) {
	g := NewWithT(t)
	g.Expect(taskID("a", "ns/n", 1)).To(Equal(taskID("a", "ns/n", 1)))
	g.Expect(taskID("a", "ns/n", 1)).NotTo(Equal(taskID("a", "ns/n", 2)))
	g.Expect(taskID("a", "ns/n", 1)).NotTo(Equal(taskID("a", "ns/m", 1)))
}
