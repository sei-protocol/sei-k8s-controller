package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sei-protocol/sei-k8s-controller/internal/task"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

type AwaitBootstrapCompleteParams struct {
	JobName   string `json:"jobName"`
	Namespace string `json:"namespace"`
}

type awaitBootstrapCompleteExecution struct {
	task.Base
	params AwaitBootstrapCompleteParams
	cfg    task.ExecutionConfig
}

func deserializeBootstrapAwait(id string, params json.RawMessage, cfg task.ExecutionConfig) (task.TaskExecution, error) {
	var p AwaitBootstrapCompleteParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-bootstrap-complete params: %w", err)
		}
	}
	return &awaitBootstrapCompleteExecution{
		Base:   task.NewBase(id),
		params: p,
		cfg:    cfg,
	}, nil
}

func (e *awaitBootstrapCompleteExecution) Execute(_ context.Context) error { return nil }

func (e *awaitBootstrapCompleteExecution) Status(ctx context.Context) task.ExecutionStatus {
	if s, done := e.IsTerminal(); done {
		return s
	}

	job := &batchv1.Job{}
	key := types.NamespacedName{Name: e.params.JobName, Namespace: e.params.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			e.SetFailed(fmt.Errorf("bootstrap job %s not found", e.params.JobName))
			return task.ExecutionFailed
		}
		return task.ExecutionRunning
	}

	if IsJobComplete(job) {
		e.Complete()
		return task.ExecutionComplete
	}
	if IsJobFailed(job) {
		e.SetFailed(fmt.Errorf("bootstrap job failed: %s", JobFailureReason(job)))
		return task.ExecutionFailed
	}

	return task.ExecutionRunning
}
