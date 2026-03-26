package task

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	sidecar "github.com/sei-protocol/seictl/sidecar/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// taskParamser is implemented by param structs that know how to convert
// themselves into sidecar TaskRequest params.
type taskParamser interface {
	toRequestParams() *map[string]any
	taskType() string
}

// sidecarExecution is a generic TaskExecution backed by the sidecar HTTP API.
// T is the typed params struct. The sidecar client is constructed lazily via
// buildSC on first use, allowing the executor to remain task-type-agnostic.
// For fire-and-forget tasks (e.g., mark-ready, config-validate), Execute
// succeeds and Status immediately returns Complete.
type sidecarExecution[T any] struct {
	buildSC       func() (SidecarClient, error)
	sc            SidecarClient
	id            string
	params        T
	fireAndForget bool
	status        ExecutionStatus
	err           error
}

// ensureClient lazily constructs the sidecar client, caching it for reuse.
func (e *sidecarExecution[T]) ensureClient() (SidecarClient, error) {
	if e.sc != nil {
		return e.sc, nil
	}
	sc, err := e.buildSC()
	if err != nil {
		return nil, err
	}
	e.sc = sc
	return sc, nil
}

func (e *sidecarExecution[T]) Execute(ctx context.Context) error {
	sc, err := e.ensureClient()
	if err != nil {
		return fmt.Errorf("building sidecar client: %w", err)
	}

	p, ok := any(&e.params).(taskParamser)
	if !ok {
		return fmt.Errorf("params type %T does not implement taskParamser", e.params)
	}

	req := sidecar.TaskRequest{
		Type:   p.taskType(),
		Params: p.toRequestParams(),
	}

	_, err = sc.SubmitTask(ctx, req)
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

	logger := log.FromContext(ctx)

	sc, err := e.ensureClient()
	if err != nil {
		logger.V(1).Info("sidecar client not ready, will retry", "task", e.id, "error", err)
		return ExecutionRunning
	}

	taskID, err := uuid.Parse(e.id)
	if err != nil {
		e.status = ExecutionFailed
		e.err = fmt.Errorf("invalid task UUID %q: %w", e.id, err)
		return e.status
	}

	result, err := sc.GetTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, sidecar.ErrNotFound) {
			return ExecutionRunning
		}
		logger.Info("sidecar GetTask error, will retry", "task", e.id, "error", err)
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
