package task

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const TaskTypeReplacePod = "replace-pod"

// ReplacePodParams identifies the node whose StatefulSet pods should be
// re-created at the new revision. Fields are serialized for plan observability.
type ReplacePodParams struct {
	NodeName  string `json:"nodeName"`
	Namespace string `json:"namespace"`
}

type replacePodExecution struct {
	taskBase
	params ReplacePodParams
	cfg    ExecutionConfig
}

func deserializeReplacePod(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p ReplacePodParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing replace-pod params: %w", err)
		}
	}
	return &replacePodExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

// Execute deletes pods owned by the StatefulSet that are still at the old
// revision after a NodeUpdate plan applied a new StatefulSet template.
//
// K8s native StatefulSet RollingUpdate refuses to delete pods that are not
// Ready, which deadlocks the rollout when seid is intentionally unready —
// e.g., halted at a chain upgrade height awaiting a binary swap. By
// proactively deleting the old pod, we let the StatefulSet recreate it at
// the new revision (StatefulSet's create path doesn't gate on readiness).
//
// Idempotent: pods already at the update revision are skipped, terminating
// pods are skipped, and a missing StatefulSet is treated as a transient
// retry (apply-statefulset is the preceding task and should have created it).
func (e *replacePodExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}
	if err := e.cfg.KubeClient.Get(ctx, key, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting statefulset: %w", err)
	}

	// Wait for the StatefulSet controller to publish a non-empty UpdateRevision.
	// Without it we can't tell which pods are stale.
	if sts.Status.UpdateRevision == "" {
		return nil
	}

	// Already converged — current matches update revision. Nothing to delete.
	if sts.Status.CurrentRevision == sts.Status.UpdateRevision {
		e.complete()
		return nil
	}

	if sts.Spec.Selector == nil || len(sts.Spec.Selector.MatchLabels) == 0 {
		return Terminal(fmt.Errorf("statefulset %q has no selector; cannot identify owned pods", node.Name))
	}

	pods := &corev1.PodList{}
	if err := e.cfg.KubeClient.List(ctx, pods,
		client.InNamespace(node.Namespace),
		client.MatchingLabels(sts.Spec.Selector.MatchLabels),
	); err != nil {
		return fmt.Errorf("listing pods for statefulset %q: %w", node.Name, err)
	}

	updateRev := sts.Status.UpdateRevision
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Labels[appsv1.ControllerRevisionHashLabelKey] == updateRev {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if err := e.cfg.KubeClient.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting pod %q: %w", pod.Name, err)
		}
	}

	e.complete()
	return nil
}

// Status returns the cached execution status.
func (e *replacePodExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
