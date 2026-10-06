package task

import (
	"context"
	"encoding/json"
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const TaskTypeRecordDataReset = "record-data-reset"

// RecordDataResetParams carries the spec.dataResetGeneration the reset plan was
// built for. The plan records that value, not the live spec: if the counter rose
// again while the plan ran, the start guard still sees a pending reset, refuses
// the plan's mark-ready, and the planner runs one more reset.
type RecordDataResetParams struct {
	Generation int64 `json:"generation"`
}

type recordDataResetExecution struct {
	taskBase
	params RecordDataResetParams
	cfg    ExecutionConfig
}

func deserializeRecordDataReset(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p RecordDataResetParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing record-data-reset params: %w", err)
		}
	}
	return &recordDataResetExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

// Execute sets status.dataResetGeneration in memory. It sits between
// reset-data and mark-ready, so the handled counter is never ahead of a
// finished wipe; the reconciler's single status patch persists it together with
// the plan progress. It never lowers the counter.
func (e *recordDataResetExecution) Execute(_ context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}
	if e.params.Generation > node.Status.DataResetGeneration {
		node.Status.DataResetGeneration = e.params.Generation
	}
	e.complete()
	return nil
}

func (e *recordDataResetExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
