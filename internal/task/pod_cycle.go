package task

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// podCycle is the shared plumbing for the controller-side pod-lifecycle tasks
// (currently replace-pod). It deletes StatefulSet-owned pods to drive the
// StatefulSet's OnDelete update strategy — pod lifecycle is the SeiNode
// controller's responsibility, not the StatefulSet controller's. The common
// core is: fetch the StatefulSet (cache-bypassing), guard against an absent
// selector and multi-replica sets, list owned pods, and delete them
// NotFound-tolerantly. Each task supplies only its distinct bits (which
// revisions/UIDs to delete, and its completion/readiness semantics).
type podCycle struct {
	cfg ExecutionConfig
}

// fetchStatefulSet reads the SeiNode's StatefulSet via the APIReader (bypassing
// the controller-runtime cache, since apply-statefulset may have written it in
// the same reconcile). A missing StatefulSet is a transient wait, signalled by
// (nil, nil): apply-statefulset precedes these tasks and the controller retries
// on the next reconcile.
func (p podCycle) fetchStatefulSet(ctx context.Context) (*seiv1alpha1.SeiNode, *appsv1.StatefulSet, error) {
	node, err := ResourceAs[*seiv1alpha1.SeiNode](p.cfg)
	if err != nil {
		return nil, nil, Terminal(err)
	}

	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Name: node.Name, Namespace: node.Namespace}
	if err := p.cfg.APIReader.Get(ctx, key, sts); err != nil {
		if apierrors.IsNotFound(err) {
			return node, nil, nil
		}
		return node, nil, fmt.Errorf("getting statefulset: %w", err)
	}
	return node, sts, nil
}

// guardSelectorAndReplicas rejects StatefulSets these tasks cannot safely
// operate on: one with no selector (owned pods are unidentifiable) and one with
// more than a single replica (reverse-ordinal deletion is unimplemented). Both
// are Terminal — failing loud beats silently violating rolling-update
// semantics. taskName is interpolated into the multi-replica error.
func guardSelectorAndReplicas(node *seiv1alpha1.SeiNode, sts *appsv1.StatefulSet, taskName string) error {
	if sts.Spec.Selector == nil || len(sts.Spec.Selector.MatchLabels) == 0 {
		return Terminal(fmt.Errorf("statefulset %q has no selector; cannot identify owned pods", node.Name))
	}
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas > 1 {
		return Terminal(fmt.Errorf(
			"%s does not support multi-replica StatefulSets (got replicas=%d); "+
				"add ordinal-aware deletion before scaling up", taskName, *sts.Spec.Replicas))
	}
	return nil
}

// ownedPods lists the pods that match the StatefulSet's selector AND carry an
// ownerReference back to it. The label match alone is insufficient: a
// manually-applied pod can share the selector labels without being owned.
func (p podCycle) ownedPods(ctx context.Context, node *seiv1alpha1.SeiNode, sts *appsv1.StatefulSet) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := p.cfg.KubeClient.List(ctx, pods,
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
func (p podCycle) deletePod(ctx context.Context, pod *corev1.Pod) error {
	if err := p.cfg.KubeClient.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting pod %q: %w", pod.Name, err)
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

// podReady reports whether the pod's Ready condition is True.
func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
