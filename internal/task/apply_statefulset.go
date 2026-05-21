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

// Execute writes the SeiNode's StatefulSet via the shared SSA helper.
// The SeiNode controller continuously calls the same helper from its
// own reconcile loop; both paths Apply the same rendered content under
// the same fieldManager, so they are idempotent against each other.
// The task remains in the plan as a sync gate for replace-pod.
//
// A nil StatefulSet with no error means SyncStatefulSet detected a
// UID-mismatch impostor and deleted it without applying. Leave the
// task in Running so the executor re-invokes on the next reconcile,
// at which point the impostor is gone and Apply creates fresh.
func (e *applyStatefulSetExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return err
	}
	sts, err := noderesource.SyncStatefulSet(ctx, e.cfg.KubeClient, e.cfg.Scheme, node, e.cfg.Platform)
	if err != nil {
		return fmt.Errorf("applying statefulset: %w", err)
	}
	if sts == nil {
		return nil
	}
	e.complete()
	return nil
}

func (e *applyStatefulSetExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
