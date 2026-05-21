package task

import (
	"context"
	"encoding/json"
	"fmt"

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
	if _, err := noderesource.SyncStatefulSet(ctx, e.cfg.KubeClient, e.cfg.Scheme, node, e.cfg.Platform); err != nil {
		return fmt.Errorf("syncing statefulset: %w", err)
	}
	e.complete()
	return nil
}

func (e *applyStatefulSetExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
