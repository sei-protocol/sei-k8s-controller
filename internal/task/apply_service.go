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

const TaskTypeApplyService = "apply-service"

var serviceFieldOwner = client.FieldOwner("seinode-controller")

// ApplyServiceParams identifies the node whose headless Service should be applied.
type ApplyServiceParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
}

type applyServiceExecution struct {
	taskBase
	params ApplyServiceParams
	cfg    ExecutionConfig
}

func deserializeApplyService(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ApplyServiceParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing apply-service params: %w", err)
		}
	}
	return &applyServiceExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *applyServiceExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return err
	}

	desired := noderesource.GenerateHeadlessService(node)
	desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	if err := ctrl.SetControllerReference(node, desired, e.cfg.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on service: %w", err)
	}

	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	if err := e.cfg.KubeClient.Patch(ctx, desired, client.Apply, serviceFieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("applying service: %w", err)
	}

	e.complete()
	return nil
}

func (e *applyServiceExecution) Status(_ context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}
	return e.status
}
