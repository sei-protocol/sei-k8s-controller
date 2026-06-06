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

const TaskTypeRestartPod = "restart-pod"

// RestartPodParams identifies the target SeiNode whose pod is restarted so
// seid re-reads config.toml on the next start.
//
// RestartedPodUID is the content-addressed restart signal, captured once at
// synthesis and threaded through params so it is stable across reconciles. The
// task deletes only this pod and completes once an owned Ready pod with a
// different UID exists. Keying on UID rather than a creation-time epoch avoids
// the same-second-truncation race: an OnDelete replacement always has a fresh
// UID. Empty when no pod existed at synthesis (any owned Ready pod then
// completes the task).
type RestartPodParams struct {
	NodeName        string    `json:"nodeName"`
	Namespace       string    `json:"namespace"`
	RestartedPodUID types.UID `json:"restartedPodUID,omitempty"`
}

type restartPodExecution struct {
	taskBase
	params RestartPodParams
	cfg    ExecutionConfig
}

func deserializeRestartPod(id string, params json.RawMessage, cfg ExecutionConfig) (TaskExecution, error) {
	var p RestartPodParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("deserializing restart-pod params: %w", err)
		}
	}
	return &restartPodExecution{
		taskBase: taskBase{id: id, status: ExecutionRunning},
		params:   p,
		cfg:      cfg,
	}, nil
}

// Execute deletes the target pod identified by RestartedPodUID. Pairs with the
// StatefulSet's OnDelete update strategy: pod lifecycle is the SeiNode
// controller's responsibility, and OnDelete recreates the pod at the same
// (unchanged) revision so seid re-reads config.toml on start.
//
// Idempotent and stateless across reconciles: the captured pod is deleted; a
// replacement pod (different UID), an empty UID (none captured), or a missing
// pod is a no-op. A missing StatefulSet/pod is a transient wait (apply-statefulset
// or scheduling may lag), not an error.
func (e *restartPodExecution) Execute(ctx context.Context) error {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return Terminal(err)
	}

	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}
	if err := e.cfg.APIReader.Get(ctx, key, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting statefulset: %w", err)
	}

	if sts.Spec.Selector == nil || len(sts.Spec.Selector.MatchLabels) == 0 {
		return Terminal(fmt.Errorf("statefulset %q has no selector; cannot identify owned pods", node.Name))
	}
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas > 1 {
		return Terminal(fmt.Errorf(
			"restart-pod does not support multi-replica StatefulSets (got replicas=%d); "+
				"add ordinal-aware deletion before scaling up", *sts.Spec.Replicas))
	}

	// Nothing to delete: no pod was captured at synthesis (StatefulSet was
	// still scheduling), so the first pod that appears is already the target.
	if e.params.RestartedPodUID == "" {
		return nil
	}

	pod, err := e.ownedPod(ctx, node, sts)
	if err != nil {
		return err
	}
	// Only delete the captured pod — a different UID is already the
	// replacement (idempotency across reconciles).
	if pod == nil || pod.UID != e.params.RestartedPodUID || pod.DeletionTimestamp != nil {
		return nil
	}
	if err := e.cfg.KubeClient.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting pod %q: %w", pod.Name, err)
	}
	return nil
}

// Status completes when an owned pod that is NOT the restarted pod is Ready.
// When no pod was captured (RestartedPodUID empty), any owned Ready pod
// completes. The original pod (matching UID), a terminating pod, or a missing
// pod reports Running so the executor re-polls.
func (e *restartPodExecution) Status(ctx context.Context) ExecutionStatus {
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
	if err := e.cfg.APIReader.Get(ctx, key, sts); err != nil {
		return ExecutionRunning
	}

	pod, err := e.ownedPod(ctx, node, sts)
	if err != nil || pod == nil {
		return ExecutionRunning
	}
	if pod.DeletionTimestamp != nil || pod.UID == e.params.RestartedPodUID {
		return ExecutionRunning
	}
	if !podReady(pod) {
		return ExecutionRunning
	}

	e.complete()
	return ExecutionComplete
}

// ownedPod returns the single pod owned by the StatefulSet (matching its
// selector and ownerReference), or nil if none is found.
func (e *restartPodExecution) ownedPod(ctx context.Context, node *seiv1alpha1.SeiNode, sts *appsv1.StatefulSet) (*corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := e.cfg.KubeClient.List(ctx, pods,
		client.InNamespace(node.Namespace),
		client.MatchingLabels(sts.Spec.Selector.MatchLabels),
	); err != nil {
		return nil, fmt.Errorf("listing pods for statefulset %q: %w", node.Name, err)
	}
	for i := range pods.Items {
		if ownedByStatefulSet(&pods.Items[i], sts) {
			return &pods.Items[i], nil
		}
	}
	return nil, nil
}

// podReady reports whether the pod's Ready condition is True.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
