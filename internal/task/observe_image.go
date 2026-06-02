package task

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/platform"
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

// Execute polls the StatefulSet rollout. If the rollout is complete, stamps
// status.currentImage and status.currentSidecarImage on the owning SeiNode
// and marks the task complete. If the rollout is still in progress, returns
// nil — the executor will re-invoke on the next reconcile since the task
// remains Pending.
func (e *observeImageExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}
	// Bypass cache: apply-statefulset just wrote this in the same reconcile.
	if err := e.cfg.APIReader.Get(ctx, key, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting statefulset: %w", err)
	}

	if sts.Status.ObservedGeneration < sts.Generation {
		return nil
	}
	if sts.Spec.Replicas == nil || sts.Status.UpdatedReplicas < *sts.Spec.Replicas {
		return nil
	}

	// Rollout complete — stamp both currentImage fields in-memory. STS
	// rollout-completion is container-agnostic (UpdatedReplicas covers the
	// whole pod spec), so seid and sidecar containers roll together.
	node.Status.CurrentImage = node.Spec.Image
	node.Status.CurrentSidecarImage = EffectiveSidecarImage(node, e.cfg.Platform)
	e.complete()
	return nil
}

// EffectiveSidecarImage returns the sidecar container image actually
// rendered onto the StatefulSet: the per-SeiNode override if set, else
// the controller-wide default from SEI_SIDECAR_IMAGE. Mirrors the
// resolution at noderesource.go and bootstrap_resources.go.
func EffectiveSidecarImage(node *seiv1alpha1.SeiNode, p platform.Config) string {
	if node.Spec.Sidecar != nil && node.Spec.Sidecar.Image != "" {
		return node.Spec.Sidecar.Image
	}
	return p.SidecarImage
}

// Status returns the cached execution status.
func (e *observeImageExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
