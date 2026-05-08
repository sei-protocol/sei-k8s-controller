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

// Execute deletes pods at the StatefulSet's old revision. Pairs with
// the StatefulSet's OnDelete update strategy: pod lifecycle is the
// SeiNode controller's responsibility, not the StatefulSet controller's.
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

	if sts.Status.ObservedGeneration < sts.Generation {
		return nil
	}
	if sts.Status.UpdateRevision == "" {
		return nil
	}
	if sts.Status.CurrentRevision == sts.Status.UpdateRevision {
		e.complete()
		return nil
	}

	if sts.Spec.Selector == nil || len(sts.Spec.Selector.MatchLabels) == 0 {
		return Terminal(fmt.Errorf("statefulset %q has no selector; cannot identify owned pods", node.Name))
	}

	// Multi-replica needs ordinal-aware deletion (reverse-ordinal). Fail
	// loud rather than silently violate StatefulSet rolling-update semantics.
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas > 1 {
		return Terminal(fmt.Errorf(
			"replace-pod does not support multi-replica StatefulSets (got replicas=%d); "+
				"add ordinal-aware deletion before scaling up", *sts.Spec.Replicas))
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
		if !ownedByStatefulSet(pod, sts) {
			continue
		}
		hash, hasHash := pod.Labels[appsv1.ControllerRevisionHashLabelKey]
		if !hasHash {
			continue
		}
		if hash == updateRev {
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

func ownedByStatefulSet(pod *corev1.Pod, sts *appsv1.StatefulSet) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "StatefulSet" && ref.UID == sts.UID {
			return true
		}
	}
	return false
}

// Status returns the cached execution status.
func (e *replacePodExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}
