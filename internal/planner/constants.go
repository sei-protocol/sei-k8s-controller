package planner

// sei-config unified schema keys used by planner controllerOverrides.
const (
	keyConcurrencyWorkers = "chain.concurrency_workers"
	keyMinRetainBlocks    = "chain.min_retain_blocks"
	keyPruning            = "storage.pruning"
	keyPruningKeepRecent  = "storage.pruning_keep_recent"
	keyPruningKeepEvery   = "storage.pruning_keep_every"
	keyPruningInterval    = "storage.pruning_interval"
	keySnapshotInterval   = "storage.snapshot_interval"
	keySnapshotKeepRecent = "storage.snapshot_keep_recent"

	keySCAsyncCommitBuffer       = "storage.state_commit.async_commit_buffer"
	keySCSnapshotKeepRecent      = "storage.state_commit.memiavl.snapshot_keep_recent"
	keySCSnapshotMinTimeInterval = "storage.state_commit.memiavl.snapshot_min_time_interval"

	keyP2PExternalAddress = "p2p.external_address"

	valCustom = "custom"
	valNothing = "nothing"

	defaultConcurrencyWorkers = "500"
	defaultSnapshotInterval   = int64(2000)
)
