package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sei-protocol/sei-k8s-controller/internal/task"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

type DeployBootstrapServiceParams struct {
	ServiceName string `json:"serviceName"`
	Namespace   string `json:"namespace"`
}

type deployBootstrapServiceExecution struct {
	task.Base
	params DeployBootstrapServiceParams
	cfg    task.ExecutionConfig
}

func deserializeBootstrapService(id string, params json.RawMessage, cfg task.ExecutionConfig) (task.TaskExecution, error) {
	var p DeployBootstrapServiceParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing deploy-bootstrap-service params: %w", err)
		}
	}
	return &deployBootstrapServiceExecution{
		Base:   task.NewBase(id),
		params: p,
		cfg:    cfg,
	}, nil
}

func (e *deployBootstrapServiceExecution) Execute(ctx context.Context) error {
	node, err := task.ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return err
	}
	inputs := nodeToBootstrapInputs(node, node.Spec.SnapshotSource())
	svc := GenerateService(inputs)
	if err := ctrl.SetControllerReference(node, svc, e.cfg.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on bootstrap service: %w", err)
	}
	if err := e.cfg.KubeClient.Create(ctx, svc); err != nil {
		if apierrors.IsAlreadyExists(err) {
			e.Complete()
			return nil
		}
		return fmt.Errorf("creating bootstrap service: %w", err)
	}
	e.Complete()
	return nil
}

func (e *deployBootstrapServiceExecution) Status(ctx context.Context) task.ExecutionStatus {
	if s, done := e.IsTerminal(); done {
		return s
	}
	existing := &corev1.Service{}
	key := types.NamespacedName{Name: e.params.ServiceName, Namespace: e.params.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, existing); err == nil {
		e.Complete()
	}
	return e.DefaultStatus()
}
