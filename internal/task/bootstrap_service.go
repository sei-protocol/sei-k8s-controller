package task

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// DeployBootstrapServiceParams holds the serialized parameters for the
// deploy-bootstrap-service task.
type DeployBootstrapServiceParams struct {
	ServiceName string `json:"serviceName"`
	Namespace   string `json:"namespace"`
}

type deployBootstrapServiceExecution struct {
	id     string
	params DeployBootstrapServiceParams
	cfg    ExecutionConfig
	status ExecutionStatus
	err    error
}

func deserializeBootstrapService(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p DeployBootstrapServiceParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing deploy-bootstrap-service params: %w", err)
		}
	}
	return &deployBootstrapServiceExecution{
		id:     id,
		params: p,
		cfg:    cfg,
		status: ExecutionRunning,
	}, nil
}

func (e *deployBootstrapServiceExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return err
	}
	svc := GenerateBootstrapService(node)
	if err := ctrl.SetControllerReference(node, svc, e.cfg.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on bootstrap service: %w", err)
	}
	if err := e.cfg.KubeClient.Create(ctx, svc); err != nil {
		if apierrors.IsAlreadyExists(err) {
			e.status = ExecutionComplete
			return nil
		}
		return fmt.Errorf("creating bootstrap service: %w", err)
	}
	e.status = ExecutionComplete
	return nil
}

func (e *deployBootstrapServiceExecution) Status(ctx context.Context) ExecutionStatus {
	if e.status == ExecutionComplete {
		return ExecutionComplete
	}
	existing := &corev1.Service{}
	key := types.NamespacedName{Name: e.params.ServiceName, Namespace: e.params.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, existing); err == nil {
		e.status = ExecutionComplete
	}
	return e.status
}

func (e *deployBootstrapServiceExecution) Err() error { return e.err }
