package task

import (
	"context"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

type AwaitBootstrapCompleteParams struct {
	JobName   string `json:"jobName"`
	Namespace string `json:"namespace"`
}

type awaitBootstrapCompleteExecution struct {
	taskBase
	params AwaitBootstrapCompleteParams
	cfg    ExecutionConfig
}

func deserializeBootstrapAwait(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p AwaitBootstrapCompleteParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-bootstrap-complete params: %w", err)
		}
	}
	return &awaitBootstrapCompleteExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *awaitBootstrapCompleteExecution) Execute(_ context.Context) error { return nil }

func (e *awaitBootstrapCompleteExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}

	job := &batchv1.Job{}
	key := types.NamespacedName{Name: e.params.JobName, Namespace: e.params.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			e.setFailed(fmt.Errorf("bootstrap job %s not found", e.params.JobName))
			return ExecutionFailed
		}
		return ExecutionRunning
	}

	if IsJobComplete(job) {
		e.complete()
		return ExecutionComplete
	}
	if IsJobFailed(job) {
		e.setFailed(fmt.Errorf("bootstrap job failed: %s", JobFailureReason(job)))
		return ExecutionFailed
	}

	return ExecutionRunning
}
