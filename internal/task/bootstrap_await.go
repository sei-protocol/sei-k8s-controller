package task

import (
	"context"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// AwaitBootstrapCompleteParams holds the serialized parameters for the
// await-bootstrap-complete task.
type AwaitBootstrapCompleteParams struct {
	JobName   string `json:"jobName"`
	Namespace string `json:"namespace"`
}

type awaitBootstrapCompleteExecution struct {
	id     string
	params AwaitBootstrapCompleteParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeBootstrapAwait(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p AwaitBootstrapCompleteParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-bootstrap-complete params: %w", err)
		}
	}
	return &awaitBootstrapCompleteExecution{
		id:     id,
		params: p,
		cfg:    cfg,
		status: ExecutionRunning,
	}, nil
}

// Execute is a no-op — the Job is already running from a previous task.
func (e *awaitBootstrapCompleteExecution) Execute(_ context.Context) error { return nil }

func (e *awaitBootstrapCompleteExecution) Status(ctx context.Context) ExecutionStatus {
	if e.status == ExecutionComplete || e.status == ExecutionFailed {
		return e.status
	}

	job := &batchv1.Job{}
	key := types.NamespacedName{Name: e.params.JobName, Namespace: e.params.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			e.err = fmt.Errorf("bootstrap job %s not found", e.params.JobName)
			e.status = ExecutionFailed
			return ExecutionFailed
		}
		return ExecutionRunning
	}

	if IsJobComplete(job) {
		e.status = ExecutionComplete
		return ExecutionComplete
	}
	if IsJobFailed(job) {
		e.err = fmt.Errorf("bootstrap job failed: %s", JobFailureReason(job))
		e.status = ExecutionFailed
		return ExecutionFailed
	}

	return ExecutionRunning
}

func (e *awaitBootstrapCompleteExecution) Err() error { return e.err }
