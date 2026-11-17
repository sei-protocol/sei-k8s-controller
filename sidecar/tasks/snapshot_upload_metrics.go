package tasks

import "github.com/prometheus/client_golang/prometheus"

var (
	// snapshotUploadLastRunSuccess is refreshed on any clean terminal — an
	// upload OR a noop. A chain that has not advanced far enough to produce a
	// new snapshot is healthy, not stalled, so a noop keeping this fresh is
	// deliberate: an alert fires on "no clean run in N hours", not "no upload".
	snapshotUploadLastRunSuccess = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "seictl_snapshot_upload_last_run_success_timestamp_seconds",
			Help: "Unix timestamp of the last snapshot-upload run that reached a clean terminal (uploaded or noop).",
		},
		[]string{"chain"},
	)

	// snapshotUploadLastUploaded records local archive publication finalization.
	// It does not report when the archive was transferred or latest.txt advanced.
	snapshotUploadLastUploaded = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "seictl_snapshot_upload_last_uploaded_timestamp_seconds",
			Help: "Unix timestamp of the last archive publication finalized by this sidecar.",
		},
		[]string{"chain"},
	)

	snapshotUploadLastUploadedHeight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "seictl_snapshot_upload_last_uploaded_height",
			Help: "Snapshot height of the last archive publication finalized by this sidecar.",
		},
		[]string{"chain"},
	)

	snapshotUploadOutcomes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "seictl_snapshot_upload_outcome_total",
			Help: "Count of snapshot-upload terminals by outcome (uploaded, noop, or error).",
		},
		[]string{"chain", "outcome"},
	)
)

func init() {
	prometheus.MustRegister(snapshotUploadLastRunSuccess)
	prometheus.MustRegister(snapshotUploadLastUploaded)
	prometheus.MustRegister(snapshotUploadLastUploadedHeight)
	prometheus.MustRegister(snapshotUploadOutcomes)
}
