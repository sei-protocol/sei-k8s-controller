package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sei-protocol/sei-k8s-controller/internal/task"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type TeardownBootstrapParams struct {
	JobName     string `json:"jobName"`
	ServiceName string `json:"serviceName"`
	Namespace   string `json:"namespace"`
}

type teardownBootstrapExecution struct {
	task.Base
	params TeardownBootstrapParams
	cfg    task.ExecutionConfig
}

func deserializeBootstrapTeardown(id string, params json.RawMessage, cfg task.ExecutionConfig) (task.TaskExecution, error) {
	var p TeardownBootstrapParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing teardown-bootstrap params: %w", err)
		}
	}
	return &teardownBootstrapExecution{
		Base:   task.NewBase(id),
		params: p,
		cfg:    cfg,
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

	e.Complete()
	return nil
}

func (e *teardownBootstrapExecution) Status(ctx context.Context) task.ExecutionStatus {
	if s, done := e.IsTerminal(); done {
		return s
	}

	kc := e.cfg.KubeClient
	ns := e.params.Namespace

	jobGone := apierrors.IsNotFound(kc.Get(ctx, types.NamespacedName{Name: e.params.JobName, Namespace: ns}, &batchv1.Job{}))
	svcGone := apierrors.IsNotFound(kc.Get(ctx, types.NamespacedName{Name: e.params.ServiceName, Namespace: ns}, &corev1.Service{}))

	if jobGone && svcGone {
		e.Complete()
		return task.ExecutionComplete
	}
	return task.ExecutionRunning
}
