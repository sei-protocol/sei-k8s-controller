package task

import (
	"context"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// deploy-fork-job: creates a Job + headless Service for state export.
type deployForkJobExecution struct {
	taskBase
	params DeployForkJobParams
	cfg    ExecutionConfig
}

func deserializeDeployForkJob(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p DeployForkJobParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing deploy-fork-job params: %w", err)
		}
	}
	return &deployForkJobExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *deployForkJobExecution) Execute(ctx context.Context) error {
	// TODO: Phase 2 — generate and create the fork export Job + Service.
	// Follows the same pattern as deployBootstrapJobExecution.
	e.complete()
	return nil
}

func (e *deployForkJobExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	existing := &batchv1.Job{}
	key := types.NamespacedName{Name: e.params.JobName, Namespace: e.params.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, existing); err == nil {
		e.complete()
	}
	return e.status
}

// await-fork-export: polls until the fork export Job completes.
type awaitForkExportExecution struct {
	taskBase
	params AwaitForkExportParams
	cfg    ExecutionConfig
}

func deserializeAwaitForkExport(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p AwaitForkExportParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing await-fork-export params: %w", err)
		}
	}
	return &awaitForkExportExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *awaitForkExportExecution) Execute(_ context.Context) error { return nil }

func (e *awaitForkExportExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	job := &batchv1.Job{}
	key := types.NamespacedName{Name: e.params.JobName, Namespace: e.params.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, job); err != nil {
		if apierrors.IsNotFound(err) {
			e.setFailed(fmt.Errorf("fork export job %s not found", e.params.JobName))
			return ExecutionFailed
		}
		return ExecutionRunning
	}
	if IsJobComplete(job) {
		e.complete()
		return ExecutionComplete
	}
	if IsJobFailed(job) {
		e.setFailed(fmt.Errorf("fork export job failed: %s", JobFailureReason(job)))
		return ExecutionFailed
	}
	return ExecutionRunning
}

// teardown-fork-job: deletes the fork export Job and its Service.
type teardownForkJobExecution struct {
	taskBase
	params TeardownForkJobParams
	cfg    ExecutionConfig
}

func deserializeTeardownForkJob(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p TeardownForkJobParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing teardown-fork-job params: %w", err)
		}
	}
	return &teardownForkJobExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *teardownForkJobExecution) Execute(ctx context.Context) error {
	kc := e.cfg.KubeClient
	ns := e.params.Namespace

	job := &batchv1.Job{}
	if err := kc.Get(ctx, types.NamespacedName{Name: e.params.JobName, Namespace: ns}, job); err == nil {
		prop := client.PropagationPolicy(metav1.DeletePropagationForeground)
		if err := kc.Delete(ctx, job, prop); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting fork export job: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("fetching fork export job: %w", err)
	}

	svc := &corev1.Service{}
	if err := kc.Get(ctx, types.NamespacedName{Name: e.params.ServiceName, Namespace: ns}, svc); err == nil {
		if err := kc.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting fork export service: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("fetching fork export service: %w", err)
	}

	return nil
}

func (e *teardownForkJobExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	kc := e.cfg.KubeClient
	ns := e.params.Namespace
	jobGone := apierrors.IsNotFound(kc.Get(ctx, types.NamespacedName{Name: e.params.JobName, Namespace: ns}, &batchv1.Job{}))
	svcGone := apierrors.IsNotFound(kc.Get(ctx, types.NamespacedName{Name: e.params.ServiceName, Namespace: ns}, &corev1.Service{}))
	if jobGone && svcGone {
		e.complete()
		return ExecutionComplete
	}
	return ExecutionRunning
}
