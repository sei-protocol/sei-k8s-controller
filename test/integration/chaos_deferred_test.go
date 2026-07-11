//go:build integration

package integration

// deferredChaosScenarios documents faults ported from the platform chaos suite
// but withheld from chaosScenarios (chaossuite_test.go), and the condition that
// would let each be re-added. Kept in its own file, out of the chaosScenarios
// literal, so that list stays a clean lookup table instead of interleaving
// active entries with paragraphs of deferral rationale.
//
// dns-chaos: it's a rediscovery fault (live MConnections don't re-resolve), so
// the under-fault progress assert can't perturb it — needs a recovery-focused
// assert + peer-FQDN-matching patterns.
//
// disk-io-latency: chaos-mesh IOChaos's toda injector reopens each target fd by
// pathname while the process is ptrace-paused — the pause freezes the process,
// not the filesystem, so an already-removed path stays removed. Both of SeiDB's
// storage engines remove old on-disk files while a live reader can still hold
// them: pebbledb (an LSM-tree store) unlinks an old SST the instant compaction
// makes a new one durable, still readable via the old fd; memIAVL prunes old
// snapshot directories by path in the background while a live reader can still
// hold one mmap'd via refcount. Either way, a path-based reopen against the
// now-removed path is a structural ENOENT, not a rare race, independent of the
// current $HOME/.sei mount nesting. Confirmed the harness itself wasn't the
// problem: volumePath was correctly templated at platform.DataDir (the real,
// current mount root), not a stale or parent path. Re-add if chaos-mesh ships
// an injector that doesn't reopen-by-path over a live store, or a non-chaos-mesh
// mechanism (cgroup v2 io.max, dm-delay) is validated as a replacement.
var deferredChaosScenarios = []string{"dns-chaos", "disk-io-latency"}
