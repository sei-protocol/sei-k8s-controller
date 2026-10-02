// Package evmdigestcompare compares the full EVM logical state of the nodes in
// a group (FlatKV-migrating nodes and memIAVL reserves of one chain) and
// exports the result as Prometheus metrics for alerting.
//
// Unlike the hash-log comparator, which only sees keys that blocks touch, a
// digest covers every EVM key, dormant ones included. Each round picks one
// height a little below the group's lowest committed tip, has every node's
// sidecar scan its own store directories at that height (the
// evm-logical-digest task), and passes when version, final count and final
// digest are equal on all nodes. A group runs one round at a time, so a node
// never runs two scans at once. Like the hash-log comparator, a mismatch
// latches a diverged gauge and the comparator keeps running.
package evmdigestcompare
