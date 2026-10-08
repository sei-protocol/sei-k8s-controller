package hashlogcompare

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const metricNamespace = "sei_hashlog_compare"

// Error reasons for the source_errors_total metric.
const (
	ReasonNotFound    = "not_found"
	ReasonUnavailable = "unavailable"
	ReasonCoverageGap = "coverage_gap"
	ReasonTornRow     = "torn_row"
	ReasonInvalidRow  = "invalid_row"
)

// Roles for the role label.
const (
	RoleMigrating = "migrating"
	RoleReserve   = "reserve"
)

// Metrics is the comparator's Prometheus surface.
type Metrics struct {
	Diverged              *prometheus.GaugeVec
	Mismatches            *prometheus.CounterVec
	MismatchedColumns     *prometheus.CounterVec
	ComparedHeights       *prometheus.CounterVec
	LastComparedHeight    *prometheus.GaugeVec
	LastComparedTimestamp *prometheus.GaugeVec
	SourceHeight          *prometheus.GaugeVec
	SourceErrors          *prometheus.CounterVec
	HeightGaps            *prometheus.CounterVec
}

// Label names shared by every comparator metric.
const (
	labelChain     = "chain"
	labelMigrating = "migrating"
	labelReserve   = "reserve"
	labelRole      = "role"
)

var (
	pairLabels = []string{labelChain, labelMigrating, labelReserve}
	roleLabels = []string{labelChain, labelMigrating, labelReserve, labelRole}
)

// NewMetrics registers the comparator metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Diverged: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "diverged",
			Help: "1 once any compared height of the pair has mismatched since the comparator started, else 0.",
		}, pairLabels),
		Mismatches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "mismatches_total",
			Help: "Compared heights whose comparable hashes differ between the migrating and reserve node.",
		}, pairLabels),
		MismatchedColumns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "mismatched_columns_total",
			Help: "Hash columns that differed on a mismatched height, by column.",
		}, append(pairLabels[:len(pairLabels):len(pairLabels)], "column")),
		ComparedHeights: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "compared_heights_total",
			Help: "Heights compared between the migrating and reserve node.",
		}, pairLabels),
		LastComparedHeight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "last_compared_height",
			Help: "Highest block height compared for the pair.",
		}, pairLabels),
		LastComparedTimestamp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "last_compared_timestamp_seconds",
			Help: "Unix time the pair last compared a new height.",
		}, pairLabels),
		SourceHeight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "source_height",
			Help: "Highest block height read from the node's hash log.",
		}, roleLabels),
		SourceErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "source_errors_total",
			Help: "Failed or unusable hash log reads from the node, by reason.",
		}, append(roleLabels[:len(roleLabels):len(roleLabels)], "reason")),
		HeightGaps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "height_gaps_total",
			Help: "Heights the node passed without writing a row the other node of the pair has.",
		}, roleLabels),
	}
	reg.MustRegister(m.Diverged, m.Mismatches, m.MismatchedColumns, m.ComparedHeights,
		m.LastComparedHeight, m.LastComparedTimestamp, m.SourceHeight, m.SourceErrors, m.HeightGaps)
	return m
}

// initPair creates every series an alert reads, so a pair that has never
// mismatched, errored or compared a height still exports it. The last-compared
// timestamp starts at now, so a pair that never compares goes stale.
func (m *Metrics) initPair(chain, migrating, reserve string, now time.Time) {
	m.Diverged.WithLabelValues(chain, migrating, reserve).Set(0)
	m.LastComparedTimestamp.WithLabelValues(chain, migrating, reserve).Set(float64(now.Unix()))
	m.Mismatches.WithLabelValues(chain, migrating, reserve)
	m.ComparedHeights.WithLabelValues(chain, migrating, reserve)
	for _, role := range []string{RoleMigrating, RoleReserve} {
		m.HeightGaps.WithLabelValues(chain, migrating, reserve, role)
		for _, reason := range []string{ReasonNotFound, ReasonUnavailable, ReasonCoverageGap, ReasonTornRow, ReasonInvalidRow} {
			m.SourceErrors.WithLabelValues(chain, migrating, reserve, role, reason)
		}
	}
}
