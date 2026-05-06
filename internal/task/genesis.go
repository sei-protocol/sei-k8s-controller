package task

import sidecar "github.com/sei-protocol/seictl/sidecar/client"

// Re-exported so the planner doesn't need a second seictl import.
type (
	GenesisNodeParam    = sidecar.GenesisNodeParam
	GenesisAccountEntry = sidecar.GenesisAccountEntry
)
