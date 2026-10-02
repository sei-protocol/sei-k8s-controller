// Package evmdigestcompare compares the EVM logical state digest of every
// node in a FlatKV migration cell — migrating nodes plus their memIAVL
// reserves — round by round, and exports the result as Prometheus metrics for
// alerting.
//
// Each round picks one height H a little below the lowest committed tip
// across the chain's nodes and asks every node's sidecar to run
// `seidb evm-logical-digest` at H through its evm-digest task, which scans the
// node's own store directories and returns the report JSON on the task
// result. A round passes when the report's version, final count, and final
// digest are equal on every node that produced one.
//
// Unlike hash-log comparison — which sees only the keys the covered blocks
// touched — the digest covers every EVM key including dormant ones, at the
// cost of a scan that takes minutes per node per height. The comparator never
// exits on a mismatch: it latches a diverged gauge per node pair and keeps
// scanning, so a single process can page on any divergence without being
// restarted.
package evmdigestcompare
