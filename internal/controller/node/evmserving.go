package node

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/noderesource"
)

// reconcileEvmServing resolves the always-present ConditionEvmServing. It
// mutates node.Status in-memory only; the caller's single status patch flushes
// it. Run before the Failed/Paused early-returns so the condition is seeded on
// every path, and before composeNodeEndpoints, which publishes the EVM-only
// endpoint off this condition.
//
// The serving signal is the pod's Ready condition. For an EVM-only node with
// its listener enabled, noderesource.UpCheckForNode points the readiness probe
// at GET / on the EVM HTTP port, so pod Ready is the kubelet's own observation
// that the listener answers — the controller does not dial the pod itself. A
// missing pod, an unscheduled pod, or a listener that refuses all resolve to
// False/ListenerRefused; the message carries which.
func (r *SeiNodeReconciler) reconcileEvmServing(ctx context.Context, node *seiv1alpha1.SeiNode) {
	engine := node.Spec.EffectiveExecutionEngine()
	if !engine.IsEvmOnly() {
		setEvmServing(node, metav1.ConditionFalse, seiv1alpha1.ReasonEvmNotApplicable,
			"execution engine is not EvmOnly; EVM serving is a property of the seid mode, not this condition")
		return
	}
	if !engine.EvmHTTPEnabled() {
		setEvmServing(node, metav1.ConditionFalse, seiv1alpha1.ReasonEvmHttpDisabled,
			"spec.executionEngine.evmOnly.httpEnabled is false; the EVM JSON-RPC listener is not started")
		return
	}
	if node.Status.StatefulSet == nil {
		setEvmServing(node, metav1.ConditionFalse, seiv1alpha1.ReasonEvmListenerRefused,
			"no StatefulSet yet; the EVM JSON-RPC listener has not started")
		return
	}

	pod := &corev1.Pod{}
	key := types.NamespacedName{Namespace: node.Namespace, Name: node.Status.StatefulSet.Name + "-0"}
	switch err := r.Get(ctx, key, pod); {
	case apierrors.IsNotFound(err):
		setEvmServing(node, metav1.ConditionFalse, seiv1alpha1.ReasonEvmListenerRefused,
			fmt.Sprintf("pod %s not found; the EVM JSON-RPC listener is not reachable", key.Name))
	case err != nil:
		setEvmServing(node, metav1.ConditionFalse, seiv1alpha1.ReasonEvmListenerRefused,
			fmt.Sprintf("reading pod %s: %v", key.Name, err))
	case podReady(pod):
		setEvmServing(node, metav1.ConditionTrue, seiv1alpha1.ReasonEvmServing,
			fmt.Sprintf("pod %s passes readiness on the EVM JSON-RPC listener", key.Name))
	default:
		setEvmServing(node, metav1.ConditionFalse, seiv1alpha1.ReasonEvmListenerRefused,
			fmt.Sprintf("pod %s is not Ready; the EVM JSON-RPC listener refuses or has not started", key.Name))
	}
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// setEvmServing sets ConditionEvmServing with ObservedGeneration stamped,
// following the always-present condition discipline.
func setEvmServing(node *seiv1alpha1.SeiNode, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               seiv1alpha1.ConditionEvmServing,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: node.Generation,
	})
}

// podReadyChanged admits pod events only when the Ready condition flips, so
// the seid pod's readiness drives EvmServing and the published endpoint on the
// transition rather than on the next statusPollInterval tick. Creates and
// deletes always pass: a new pod starts unready and a deleted one stops
// serving.
var podReadyChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldPod, ok := e.ObjectOld.(*corev1.Pod)
		if !ok {
			return false
		}
		newPod, ok := e.ObjectNew.(*corev1.Pod)
		if !ok {
			return false
		}
		return podReady(oldPod) != podReady(newPod)
	},
	GenericFunc: func(event.GenericEvent) bool { return false },
}

// podToSeiNode maps a seid pod to its owning SeiNode by the noderesource
// NodeLabel the StatefulSet template stamps on every pod.
func podToSeiNode(_ context.Context, obj client.Object) []reconcile.Request {
	name, ok := obj.GetLabels()[noderesource.NodeLabel]
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}
