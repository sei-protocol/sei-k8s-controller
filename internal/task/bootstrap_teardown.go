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

// TeardownBootstrapParams holds the serialized parameters for the
// teardown-bootstrap task.
type TeardownBootstrapParams struct {
	JobName     string `json:"jobName"`
	ServiceName string `json:"serviceName"`
	Namespace   string `json:"namespace"`
}

type teardownBootstrapExecution struct {
	id     string
	params TeardownBootstrapParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeBootstrapTeardown(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p TeardownBootstrapParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing teardown-bootstrap params: %w", err)
		}
	}
	return &teardownBootstrapExecution{
		id:     id,
		params: p,
		cfg:    cfg,
		status: ExecutionRunning,
	}, nil
}

func (e *teardownBootstrapExecution) Execute(ctx context.Context) error {
	kc := e.cfg.KubeClient
	ns := e.params.Namespace

	job := &batchv1.Job{}
	jobKey := types.NamespacedName{Name: e.params.JobName, Namespace: ns}
	if err := kc.Get(ctx, jobKey, job); err == nil {
		prop := client.PropagationPolicy(metav1.DeletePropagationForeground)
		if err := kc.Delete(ctx, job, prop); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting bootstrap job: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("fetching bootstrap job for deletion: %w", err)
	}

	svc := &corev1.Service{}
	svcKey := types.NamespacedName{Name: e.params.ServiceName, Namespace: ns}
	if err := kc.Get(ctx, svcKey, svc); err == nil {
		if err := kc.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting bootstrap service: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("fetching bootstrap service for deletion: %w", err)
	}

	return nil
}

func (e *teardownBootstrapExecution) Status(ctx context.Context) ExecutionStatus {
	if e.status == ExecutionComplete {
		return ExecutionComplete
	}

	kc := e.cfg.KubeClient
	ns := e.params.Namespace

	job := &batchv1.Job{}
	jobKey := types.NamespacedName{Name: e.params.JobName, Namespace: ns}
	jobGone := false
	if err := kc.Get(ctx, jobKey, job); apierrors.IsNotFound(err) {
		jobGone = true
	}

	svc := &corev1.Service{}
	svcKey := types.NamespacedName{Name: e.params.ServiceName, Namespace: ns}
	svcGone := false
	if err := kc.Get(ctx, svcKey, svc); apierrors.IsNotFound(err) {
		svcGone = true
	}

	if jobGone && svcGone {
		e.status = ExecutionComplete
		return ExecutionComplete
	}
	return ExecutionRunning
}

func (e *teardownBootstrapExecution) Err() error { return e.err }
