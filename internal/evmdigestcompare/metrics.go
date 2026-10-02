package evmdigestcompare

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const metricNamespace = "sei_evmdigest_compare"

// Error reasons for the source_errors_total metric.
const (
	ReasonUnavailable = "unavailable"
	ReasonScanFailed  = "scan_failed"
	ReasonNoTip       = "no_tip"
)

// Metrics is the comparator's Prometheus surface.
type Metrics struct {
	Diverged              *prometheus.GaugeVec
	Mismatches            *prometheus.CounterVec
	ComparedHeights       *prometheus.CounterVec
	LastComparedHeight    *prometheus.GaugeVec
	LastComparedTimestamp *prometheus.GaugeVec
	SourceHeight          *prometheus.GaugeVec
	ScannedHeight         *prometheus.GaugeVec
	SourceErrors          *prometheus.CounterVec
}

// Label names shared by every comparator metric.
const (
	labelChain = "chain"
	labelNode  = "node"
	labelNodeA = "node_a"
	labelNodeB = "node_b"
)

var (
	chainLabels  = []string{labelChain}
	nodeLabels   = []string{labelChain, labelNode}
	nodePairLabs = []string{labelChain, labelNodeA, labelNodeB}
)

// NewMetrics registers the comparator metrics on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Diverged: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "diverged",
			Help: "1 once the node pair's digest has mismatched at a compared height since the comparator started, else 0.",
		}, nodePairLabs),
		Mismatches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "mismatches_total",
			Help: "Rounds where the node pair's digest report differed.",
		}, nodePairLabs),
		ComparedHeights: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "compared_heights_total",
			Help: "Heights compared across the chain's nodes.",
		}, chainLabels),
		LastComparedHeight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "last_compared_height",
			Help: "Highest block height compared across the chain.",
		}, chainLabels),
		LastComparedTimestamp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "last_compared_timestamp_seconds",
			Help: "Unix time the chain last compared a new height.",
		}, chainLabels),
		SourceHeight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "source_height",
			Help: "Latest committed height reported by the node's sidecar.",
		}, nodeLabels),
		ScannedHeight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricNamespace, Name: "scanned_height",
			Help: "Latest height the node returned a digest report for.",
		}, nodeLabels),
		SourceErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricNamespace, Name: "source_errors_total",
			Help: "Failed or unusable sidecar reads and digest scans for the node, by reason.",
		}, append(nodeLabels[:len(nodeLabels):len(nodeLabels)], "reason")),
	}
	reg.MustRegister(m.Diverged, m.Mismatches, m.ComparedHeights, m.LastComparedHeight,
		m.LastComparedTimestamp, m.SourceHeight, m.ScannedHeight, m.SourceErrors)
	return m
}

// initChain creates every series an alert reads, so a chain that has never
// mismatched, errored or compared a height still exports it. The last-compared
// timestamp starts at now, so a chain that never compares goes stale.
func (m *Metrics) initChain(chain Chain, now time.Time) {
	m.ComparedHeights.WithLabelValues(chain.Name)
	m.LastComparedTimestamp.WithLabelValues(chain.Name).Set(float64(now.Unix()))
	for _, n := range chain.Nodes {
		label := n.Label()
		m.SourceHeight.WithLabelValues(chain.Name, label)
		m.ScannedHeight.WithLabelValues(chain.Name, label)
		for _, reason := range []string{ReasonUnavailable, ReasonScanFailed, ReasonNoTip} {
			m.SourceErrors.WithLabelValues(chain.Name, label, reason)
		}
	}
	for i := range chain.Nodes {
		for j := i + 1; j < len(chain.Nodes); j++ {
			m.Diverged.WithLabelValues(chain.Name, chain.Nodes[i].Label(), chain.Nodes[j].Label()).Set(0)
			m.Mismatches.WithLabelValues(chain.Name, chain.Nodes[i].Label(), chain.Nodes[j].Label())
		}
	}
}
