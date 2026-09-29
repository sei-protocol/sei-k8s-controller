package hashlogcompare

import (
	"io"
	"log/slog"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	sidecar "github.com/sei-protocol/sei-k8s-controller/sidecarapi/client"
)

func newTestRunner(migrating, reserve Source) (*pairRunner, *Metrics) {
	m := NewMetrics(prometheus.NewRegistry())
	c := &Comparator{
		Metrics: m,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	pr := &pairRunner{
		c:         c,
		labels:    []string{"arctic-1", "arctic-1/rpc-node-0", "arctic-1/memiavl-rpc-0"},
		migrating: NewReader(migrating),
		reserve:   NewReader(reserve),
		state:     newPairState(),
	}
	m.initPair(pr.labels[0], pr.labels[1], pr.labels[2])
	return pr, m
}

func TestPairRunner_MatchingLogs(t *testing.T) {
	g := NewWithT(t)
	migrating, reserve := &fakeSource{}, &fakeSource{}
	migrating.add(1, testHeader+rows(1, 10))
	reserve.add(1, testHeader+rows(1, 8))
	pr, m := newTestRunner(migrating, reserve)

	pr.poll(t.Context())
	g.Expect(testutil.ToFloat64(m.ComparedHeights.WithLabelValues(pr.labels...))).To(Equal(8.0))
	g.Expect(testutil.ToFloat64(m.LastComparedHeight.WithLabelValues(pr.labels...))).To(Equal(8.0))
	g.Expect(testutil.ToFloat64(m.LastComparedTimestamp.WithLabelValues(pr.labels...))).To(Equal(1_800_000_000.0))
	g.Expect(testutil.ToFloat64(m.Diverged.WithLabelValues(pr.labels...))).To(BeZero())
	g.Expect(testutil.ToFloat64(m.Mismatches.WithLabelValues(pr.labels...))).To(BeZero())
	g.Expect(testutil.ToFloat64(m.SourceHeight.WithLabelValues(append(pr.labels, RoleMigrating)...))).To(Equal(10.0))
}

func TestPairRunner_EVMRootDifferenceIsNotAMismatch(t *testing.T) {
	g := NewWithT(t)
	migrating, reserve := &fakeSource{}, &fakeSource{}
	migrating.add(1, testHeader+"1,bh,cs,rh,bank,flatkv-root\n")
	reserve.add(1, testHeader+"1,bh,cs,rh,bank,memiavl-root\n")
	pr, m := newTestRunner(migrating, reserve)

	pr.poll(t.Context())
	g.Expect(testutil.ToFloat64(m.ComparedHeights.WithLabelValues(pr.labels...))).To(Equal(1.0))
	g.Expect(testutil.ToFloat64(m.Diverged.WithLabelValues(pr.labels...))).To(BeZero())
}

func TestPairRunner_MismatchLatchesDiverged(t *testing.T) {
	g := NewWithT(t)
	migrating, reserve := &fakeSource{}, &fakeSource{}
	mf := migrating.add(1, testHeader+rows(1, 3))
	rf := reserve.add(1, testHeader+rows(1, 3))
	pr, m := newTestRunner(migrating, reserve)
	pr.poll(t.Context())

	mf.data = append(mf.data, row(4, "-bad")...)
	rf.data = append(rf.data, row(4, "")...)
	pr.poll(t.Context())
	g.Expect(testutil.ToFloat64(m.Diverged.WithLabelValues(pr.labels...))).To(Equal(1.0))
	g.Expect(testutil.ToFloat64(m.Mismatches.WithLabelValues(pr.labels...))).To(Equal(1.0))
	g.Expect(testutil.ToFloat64(m.MismatchedColumns.WithLabelValues(append(pr.labels, "changeset")...))).To(Equal(1.0))

	mf.data = append(mf.data, row(5, "")...)
	rf.data = append(rf.data, row(5, "")...)
	pr.poll(t.Context())
	g.Expect(testutil.ToFloat64(m.Diverged.WithLabelValues(pr.labels...))).To(Equal(1.0))
	g.Expect(testutil.ToFloat64(m.Mismatches.WithLabelValues(pr.labels...))).To(Equal(1.0))
	g.Expect(testutil.ToFloat64(m.LastComparedHeight.WithLabelValues(pr.labels...))).To(Equal(5.0))
}

func TestPairRunner_MissingReserveLogIsSourceError(t *testing.T) {
	g := NewWithT(t)
	migrating := &fakeSource{}
	migrating.add(1, testHeader+rows(1, 3))
	pr, m := newTestRunner(migrating, &fakeSource{listErr: sidecar.ErrHashLogNotFound})

	pr.poll(t.Context())
	reserveNotFound := m.SourceErrors.WithLabelValues(append(pr.labels, RoleReserve, ReasonNotFound)...)
	g.Expect(testutil.ToFloat64(reserveNotFound)).To(Equal(1.0))
	g.Expect(testutil.ToFloat64(m.ComparedHeights.WithLabelValues(pr.labels...))).To(BeZero())
}

func TestInitPair_ExportsZeroSeries(t *testing.T) {
	g := NewWithT(t)
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.initPair("c", "a", "b")

	n, err := testutil.GatherAndCount(reg,
		"sei_hashlog_compare_diverged", "sei_hashlog_compare_mismatches_total", "sei_hashlog_compare_source_errors_total")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(n).To(Equal(1 + 1 + 2*4))
}
