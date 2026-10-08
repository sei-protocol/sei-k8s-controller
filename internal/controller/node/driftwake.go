package node

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

// A drifted node that waits for a roll slot re-checks on its status poll. The
// watch below wakes it as soon as a slot frees instead, so the next node in
// slot order starts without waiting out the poll interval.

// slotReleased passes a SeiNode event that can free a drift-roll slot or add
// one: the node leaves NodeUpdateInProgress=True, its pause flips, its phase
// changes (the slot count counts Running, unpaused nodes), or it is deleted.
var slotReleased = predicate.Funcs{
	CreateFunc: func(event.CreateEvent) bool { return false },
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldNode, ok := e.ObjectOld.(*seiv1alpha1.SeiNode)
		if !ok {
			return false
		}
		newNode, ok := e.ObjectNew.(*seiv1alpha1.SeiNode)
		if !ok {
			return false
		}
		return (nodeUpdating(oldNode) && !nodeUpdating(newNode)) ||
			oldNode.Spec.Paused != newNode.Spec.Paused ||
			oldNode.Status.Phase != newNode.Status.Phase
	},
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	GenericFunc: func(event.GenericEvent) bool { return false },
}

func nodeUpdating(node *seiv1alpha1.SeiNode) bool {
	return meta.IsStatusConditionTrue(node.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
}

// deferredPeers maps a node that freed its slot to every node in its namespace
// that reports UpdateDeferred. Each woken node runs the slot decision again
// with an uncached read, so a stale cache here costs at most one extra
// reconcile.
func (r *SeiNodeReconciler) deferredPeers(ctx context.Context, obj client.Object) []reconcile.Request {
	var nodes seiv1alpha1.SeiNodeList
	if err := r.List(ctx, &nodes, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing nodes waiting for a drift-roll slot", "namespace", obj.GetNamespace())
		return nil
	}
	var reqs []reconcile.Request
	for i := range nodes.Items {
		n := &nodes.Items[i]
		c := meta.FindStatusCondition(n.Status.Conditions, seiv1alpha1.ConditionNodeUpdateInProgress)
		if c != nil && c.Reason == planner.ReasonUpdateDeferred {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(n)})
		}
	}
	return reqs
}
