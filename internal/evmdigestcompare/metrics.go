package evmdigestcompare

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const metricNamespace = "sei_evm_digest_compare"

const (
	labelChain = "chain"
	labelNode  = "node"
)

// Round results.
const (
	ResultMatch      = "match"
	ResultMismatch   = "mismatch"
	ResultIncomplete = "incomplete"
)

// Scan error reasons.
const (
	ReasonUnavailable   = "unavailable"
	ReasonFailed        = "failed"
	ReasonTimeout       = "timeout"
	ReasonInvalidResult = "invalid_result"
)

// Metrics holds the comparator's Prometheus collectors. Group series are
// labeled by chain; node series add the node label (namespace/name).
type Metrics struct {
	Diverged              *prometheus.GaugeVec
	Mismatches            *prometheus.CounterVec
	Rounds                *prometheus.CounterVec
	TargetHeight          *prometheus.GaugeVec
	LastComparedHeight    *prometheus.GaugeVec
	LastComparedTimestamp *prometheus.GaugeVec
	SourceHeight          *prometheus.GaugeVec
	ScanErrors            *prometheus.CounterVec
	ScanDuration          *prometheus.GaugeVec
	ScanEntries           *prometheus.GaugeVec
}

// NewMetrics registers the comparator's collectors on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	group := []string{labelChain}
	node := []string{labelChain, labelNode}
	m := &Metrics{
		Diverged: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "diverged",
			Help: "1 once any round found the group's EVM digests unequal; stays 1 until restart.",
		}, group),
		Mismatches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "mismatches_total",
			Help: "Rounds whose nodes disagreed on version, final count or final digest.",
		}, group),
		Rounds: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "rounds_total",
			Help: "Finished rounds by result (match, mismatch, incomplete).",
		}, []string{labelChain, "result"}),
		TargetHeight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "target_height",
			Help: "Height the current or last round scans every node at.",
		}, group),
		LastComparedHeight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "last_compared_height",
			Help: "Height of the last round in which every node returned a digest.",
		}, group),
		LastComparedTimestamp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "last_compared_timestamp_seconds",
			Help: "Unix time of the last round in which every node returned a digest (process start until the first).",
		}, group),
		SourceHeight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "source_height",
			Help: "Node's committed height as last read from its sidecar.",
		}, node),
		ScanErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "scan_errors_total",
			Help: "Node reads or scans that produced no digest, by reason.",
		}, []string{labelChain, labelNode, "reason"}),
		ScanDuration: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "scan_duration_seconds",
			Help: "Wall time of the node's last successful scan, as measured by its sidecar.",
		}, node),
		ScanEntries: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "scan_entries",
			Help: "Final entry count of the node's last successful scan.",
		}, node),
	}
	reg.MustRegister(m.Diverged, m.Mismatches, m.Rounds, m.TargetHeight, m.LastComparedHeight,
		m.LastComparedTimestamp, m.SourceHeight, m.ScanErrors, m.ScanDuration, m.ScanEntries)
	return m
}

// initGroup exports zero-valued series for a group so alerts can tell "no
// mismatch" from "no data", and starts the staleness clock at process start.
func (m *Metrics) initGroup(chain string, nodes []string, now time.Time) {
	m.Diverged.WithLabelValues(chain).Set(0)
	m.Mismatches.WithLabelValues(chain)
	for _, r := range []string{ResultMatch, ResultMismatch, ResultIncomplete} {
		m.Rounds.WithLabelValues(chain, r)
	}
	m.LastComparedTimestamp.WithLabelValues(chain).Set(float64(now.Unix()))
	for _, n := range nodes {
		for _, r := range []string{ReasonUnavailable, ReasonFailed, ReasonTimeout, ReasonInvalidResult} {
			m.ScanErrors.WithLabelValues(chain, n, r)
		}
	}
}
