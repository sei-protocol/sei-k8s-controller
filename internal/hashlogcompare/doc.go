// Package hashlogcompare compares the HashLogger output of a FlatKV-migrating
// SeiNode against a memIAVL-only reserve node, row by row, and exports the
// result as Prometheus metrics for alerting.
//
// Each node's hash log is read through its sidecar's /v0/hashlog endpoints.
// Rows are aligned by block height and compared on the columns that do not
// depend on the storage backend (see ComparableHashes). The comparator never
// exits on a mismatch: it latches a diverged gauge per pair and keeps reading,
// so a single process can page on any pair without being restarted.
package hashlogcompare
