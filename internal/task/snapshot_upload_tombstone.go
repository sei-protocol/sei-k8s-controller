package task

import (
	"context"
	"encoding/json"
)

// snapshotUploadTombstone is a no-op TaskExecution registered for the
// snapshot-upload task type.
//
// Invariant: a stored plan carrying a snapshot-upload task advances past it as
// if the task had completed, with zero sidecar interaction — Execute submits
// nothing and Status reports Complete. The planner does not emit this task
// type into any new plan; the registry entry exists solely so that stored
// plans that still carry this type drain past it instead of hitting
// UnknownTaskTypeError and failing the plan.
//
// This type deliberately holds no SidecarClient and no submit path: it cannot
// re-submit to the sidecar. Re-submitting would respawn the streaming upload
// loop the sidecar side is reaping.
type snapshotUploadTombstone struct{}

func (snapshotUploadTombstone) Execute(context.Context) error          { return nil }
func (snapshotUploadTombstone) Status(context.Context) ExecutionStatus { return ExecutionComplete }
func (snapshotUploadTombstone) Err() error                             { return nil }

// deserializeSnapshotUploadTombstone reconstructs the snapshot-upload tombstone.
// Params are ignored — the stored form carries no state the tombstone needs.
func deserializeSnapshotUploadTombstone(_ string, _ json.RawMessage, _ ExecutionConfig) (TaskExecution, error) {
	return snapshotUploadTombstone{}, nil
}
