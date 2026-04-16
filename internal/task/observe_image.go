package task

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const TaskTypeObserveImage = "observe-image"

// ObserveImageParams identifies the node whose StatefulSet rollout to observe.
// Fields are serialized into the plan for observability (the task itself
// reads the node from ExecutionConfig.Resource).
type ObserveImageParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
}

type observeImageExecution struct {
	taskBase
	params ObserveImageParams
	cfg    ExecutionConfig
}

func deserializeObserveImage(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ObserveImageParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing observe-image params: %w", err)
		}
	}
	return &observeImageExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

// Execute is a no-op for observe-image — all work happens in Status polling.
func (e *observeImageExecution) Execute(_ context.Context) error {
	return nil
}

// Status polls the StatefulSet rollout. Returns ExecutionRunning until the
// rollout completes (UpdatedReplicas >= Replicas and ObservedGeneration >= Generation),
// then stamps status.currentImage on the owning SeiNode and returns ExecutionComplete.
func (e *observeImageExecution) Status(ctx context.Context) ExecutionStatus {
	if s, done := e.isTerminal(); done {
		return s
	}

	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		e.setFailed(err)
		return ExecutionFailed
	}

	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, sts); err != nil {
		if apierrors.IsNotFound(err) {
			// StatefulSet doesn't exist yet — keep waiting.
			return ExecutionRunning
		}
		e.setFailed(fmt.Errorf("getting statefulset: %w", err))
		return ExecutionFailed
	}

	if sts.Status.ObservedGeneration < sts.Generation {
		return ExecutionRunning
	}
	if sts.Spec.Replicas == nil || sts.Status.UpdatedReplicas < *sts.Spec.Replicas {
		return ExecutionRunning
	}

	// Rollout complete — stamp currentImage in-memory.
	node.Status.CurrentImage = node.Spec.Image
	e.complete()
	return ExecutionComplete
}
