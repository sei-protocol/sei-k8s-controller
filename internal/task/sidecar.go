package task

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
)

// taskParamser is implemented by param structs that know how to convert
// themselves into sidecar TaskRequest params.
type taskParamser interface {
	toRequestParams() *map[string]any
	taskType() string
}

// sidecarExecution is a generic TaskExecution backed by the sidecar HTTP API.
// T is the typed params struct. For fire-and-forget tasks (e.g., mark-ready,
// config-validate), Execute succeeds and Status immediately returns Complete.
type sidecarExecution[T any] struct {
	sc            SidecarClient
	id            string
	params        T
	fireAndForget bool
	status        ExecutionStatus
	err           error
}

func (e *sidecarExecution[T]) Execute(ctx context.Context) error {
	p, ok := any(&e.params).(taskParamser)
	if !ok {
		return fmt.Errorf("params type %T does not implement taskParamser", e.params)
	}

	req := sidecar.TaskRequest{
		Type:   p.taskType(),
		Params: p.toRequestParams(),
	}

	_, err := e.sc.SubmitTask(ctx, req)
	if err != nil {
		e.status = ExecutionFailed
		e.err = fmt.Errorf("submitting %s: %w", req.Type, err)
		return e.err
	}

	if e.fireAndForget {
		e.status = ExecutionComplete
	} else {
		e.status = ExecutionRunning
	}
	return nil
}

func (e *sidecarExecution[T]) Status(ctx context.Context) ExecutionStatus {
	if e.status == ExecutionComplete || e.status == ExecutionFailed {
		return e.status
	}

	taskID, err := uuid.Parse(e.id)
	if err != nil {
		e.status = ExecutionFailed
		e.err = fmt.Errorf("invalid task UUID %q: %w", e.id, err)
		return e.status
	}

	result, err := e.sc.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, sidecar.ErrNotFound) {
			return ExecutionRunning
		}
		return e.status
	}

	switch result.Status {
	case sidecar.Completed:
		e.status = ExecutionComplete
	case sidecar.Failed:
		e.status = ExecutionFailed
		if result.Error != nil && *result.Error != "" {
			e.err = fmt.Errorf("%s", *result.Error)
		} else {
			e.err = fmt.Errorf("task %s failed with unknown error", e.id)
		}
	}
	return e.status
}

func (e *sidecarExecution[T]) Err() error { return e.err }
