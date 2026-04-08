package planner

// Replayer-specific SeiDB state-commit override keys.
const (
	keySCAsyncCommitBuffer       = "storage.state_commit.async_commit_buffer"
	keySCSnapshotKeepRecent      = "storage.state_commit.memiavl.snapshot_keep_recent"
	keySCSnapshotMinTimeInterval = "storage.state_commit.memiavl.snapshot_min_time_interval"
)
