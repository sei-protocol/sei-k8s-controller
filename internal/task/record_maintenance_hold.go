package task

import (
	"context"
	"encoding/json"
	"fmt"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const TaskTypeRecordMaintenanceHold = "record-maintenance-hold"

// RecordMaintenanceHoldParams carries the hold now in effect: Immediate when
// the plan left seid parked, AfterExit when it left the gate armed, "" when it
// released seid.
type RecordMaintenanceHoldParams struct {
	Hold seiv1alpha1.MaintenanceHold `json:"hold"`
}

type recordMaintenanceHoldExecution struct {
	taskBase
	params RecordMaintenanceHoldParams
	cfg    ExecutionConfig
}

func deserializeRecordMaintenanceHold(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p RecordMaintenanceHoldParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing record-maintenance-hold params: %w", err)
		}
	}
	return &recordMaintenanceHoldExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

// Execute sets status.maintenanceHold in memory. It is the last step of every
// plan that changes what the hold does to seid, so the status names the hold in
// effect only after the gate and seid are in that state.
func (e *recordMaintenanceHoldExecution) Execute(_ context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}
	node.Status.MaintenanceHold = e.params.Hold
	e.complete()
	return nil
}

func (e *recordMaintenanceHoldExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
