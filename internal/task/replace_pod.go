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
	cfg    ExecutionConfig
	params ReplacePodParams
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
		cfg:      cfg,
		params:   p,
	}, nil
}

// Execute deletes pods at the StatefulSet's old revision. Pairs with
// the StatefulSet's OnDelete update strategy: pod lifecycle is the
// SeiNode controller's responsibility, not the StatefulSet controller's.
//
// Revision-gated and readiness-blind: it completes as soon as old-revision
// pods are deleted, without waiting for the replacement to become Ready. The
// image-rollout path depends on this — seid may be intentionally unready (e.g.
// halted at a chain-upgrade height).
func (e *replacePodExecution) Execute(ctx context.Context) error {
	node, sts, err := e.fetchStatefulSet(ctx)
	if err != nil {
		return err
	}
	if sts == nil {
		return nil
	}

	// Revision gate runs before the selector/replica guards: a not-yet-observed
	// or not-yet-populated revision is a transient wait, and an already-rolled
	// StatefulSet is a complete no-op — neither should reach pod deletion.
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

	if err := guardSelectorAndReplicas(node, sts); err != nil {
		return err
	}

	pods, err := e.ownedPods(ctx, node, sts)
	if err != nil {
		return err
	}

	updateRev := sts.Status.UpdateRevision
	for i := range pods {
		pod := &pods[i]
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
		if err := e.deletePod(ctx, pod); err != nil {
			return err
		}
	}

	e.complete()
	return nil
}

// Status returns the cached execution status. replace-pod completes
// synchronously within Execute (no readiness wait).
func (e *replacePodExecution) Status(_ context.Context) ExecutionStatus {
	return e.DefaultStatus()
}

// fetchStatefulSet reads the SeiNode's StatefulSet via the APIReader (bypassing
// the controller-runtime cache, since apply-statefulset may have written it in
// the same reconcile). A missing StatefulSet is a transient wait, signalled by
// (nil, nil): apply-statefulset precedes this task and the controller retries
// on the next reconcile.
func (e *replacePodExecution) fetchStatefulSet(ctx context.Context) (*seiv1alpha1.SeiNode, *appsv1.StatefulSet, error) {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](e.cfg)
	if err != nil {
		return nil, nil, Terminal(err)
	}

	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}
	if err := e.cfg.APIReader.Get(ctx, key, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return node, nil, nil
		}
		return node, nil, fmt.Errorf("getting statefulset: %w", err)
	}
	return node, sts, nil
}

// ownedPods lists the pods that match the StatefulSet's selector AND carry an
// ownerReference back to it. The label match alone is insufficient: a
// manually-applied pod can share the selector labels without being owned.
func (e *replacePodExecution) ownedPods(ctx context.Context, node *seiv1alpha1.SeiNode, sts *appsv1.StatefulSet) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := e.cfg.KubeClient.List(ctx, pods,
		client.InNamespace(node.Namespace),
		client.MatchingLabels(sts.Spec.Selector.MatchLabels),
	); err != nil {
		return nil, fmt.Errorf("listing pods for statefulset %q: %w", node.Name, err)
	}
	owned := pods.Items[:0]
	for i := range pods.Items {
		if ownedByStatefulSet(&pods.Items[i], sts) {
			owned = append(owned, pods.Items[i])
		}
	}
	return owned, nil
}

// deletePod deletes the pod, tolerating an already-gone pod (NotFound) so the
// task is idempotent across reconciles.
func (e *replacePodExecution) deletePod(ctx context.Context, pod *corev1.Pod) error {
	if err := e.cfg.KubeClient.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting pod %q: %w", pod.Name, err)
	}
	return nil
}

// guardSelectorAndReplicas rejects StatefulSets this task cannot safely operate
// on: one with no selector (owned pods are unidentifiable) and one with more
// than a single replica (reverse-ordinal deletion is unimplemented). Both are
// Terminal — failing loud beats silently violating rolling-update semantics.
func guardSelectorAndReplicas(node *seiv1alpha1.SeiNode, sts *appsv1.StatefulSet) error {
	if sts.Spec.Selector == nil || len(sts.Spec.Selector.MatchLabels) == 0 {
		return Terminal(fmt.Errorf("statefulset %q has no selector; cannot identify owned pods", node.Name))
	}
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas > 1 {
		return Terminal(fmt.Errorf(
			"%s does not support multi-replica StatefulSets (got replicas=%d); "+
				"add ordinal-aware deletion before scaling up", TaskTypeReplacePod, *sts.Spec.Replicas))
	}
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
