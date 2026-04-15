package task

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

const TaskTypeApplyStatefulSet = "apply-statefulset"

// ApplyStatefulSetParams identifies the node whose StatefulSet should be applied.
// Fields are serialized into the plan for observability (the task itself
// reads the node from ExecutionConfig.Resource).
type ApplyStatefulSetParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
}

type applyStatefulSetExecution struct {
	taskBase
	params ApplyStatefulSetParams
	cfg    ExecutionConfig
}

func deserializeApplyStatefulSet(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ApplyStatefulSetParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing apply-statefulset params: %w", err)
		}
	}
	return &applyStatefulSetExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

func (e *applyStatefulSetExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return err
	}

	desired := noderesource.GenerateStatefulSet(node, e.cfg.Platform)
	desired.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
	if err := ctrl.SetControllerReference(node, desired, e.cfg.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on statefulset: %w", err)
	}

	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	if err := e.cfg.KubeClient.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("applying statefulset: %w", err)
	}

	e.complete()
	return nil
}

func (e *applyStatefulSetExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
