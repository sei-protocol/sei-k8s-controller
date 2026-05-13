package task

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

const TaskTypeApplyRBACProxyConfig = "apply-rbac-proxy-config"

type ApplyRBACProxyConfigParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
}

type applyRBACProxyConfigExecution struct {
	taskBase
	params ApplyRBACProxyConfigParams
	cfg    ExecutionConfig
}

func deserializeApplyRBACProxyConfig(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ApplyRBACProxyConfigParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing apply-rbac-proxy-config params: %w", err)
		}
	}
	return &applyRBACProxyConfigExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *applyRBACProxyConfigExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return err
	}

	desired := noderesource.GenerateRBACProxyConfigMap(node)
	if desired == nil {
		e.complete()
		return nil
	}
	desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
	if err := ctrl.SetControllerReference(node, desired, e.cfg.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on rbac-proxy ConfigMap: %w", err)
	}

	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	if err := e.cfg.KubeClient.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("applying rbac-proxy ConfigMap: %w", err)
	}

	e.complete()
	return nil
}

func (e *applyRBACProxyConfigExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
