package task

import (
	"context"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// DeployBootstrapJobParams holds the serialized parameters for the
// deploy-bootstrap-job task.
type DeployBootstrapJobParams struct {
	JobName   string `json:"jobName"`
	Namespace string `json:"namespace"`
}

type deployBootstrapJobExecution struct {
	id     string
	params DeployBootstrapJobParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeBootstrapJob(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p DeployBootstrapJobParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing deploy-bootstrap-job params: %w", err)
		}
	}
	return &deployBootstrapJobExecution{
		id:     id,
		params: p,
		cfg:    cfg,
		status: ExecutionRunning,
	}, nil
}

func (e *deployBootstrapJobExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return err
	}
	snap := node.Spec.SnapshotSource()
	job, err := GenerateBootstrapJob(node, snap, e.cfg.Platform)
	if err != nil {
		return fmt.Errorf("generating bootstrap job spec: %w", err)
	}
	if err := ctrl.SetControllerReference(node, job, e.cfg.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on bootstrap job: %w", err)
	}
	if err := e.cfg.KubeClient.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			e.status = ExecutionComplete
			return nil
		}
		return fmt.Errorf("creating bootstrap job: %w", err)
	}
	e.status = ExecutionComplete
	return nil
}

func (e *deployBootstrapJobExecution) Status(ctx context.Context) ExecutionStatus {
	if e.status == ExecutionComplete {
		return ExecutionComplete
	}
	existing := &batchv1.Job{}
	key := types.NamespacedName{Name: e.params.JobName, Namespace: e.params.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, existing); err == nil {
		e.status = ExecutionComplete
	}
	return e.status
}

func (e *deployBootstrapJobExecution) Err() error { return e.err }
