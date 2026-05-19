package nodetask

import (
	"context"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// seiNodeTargetHandler enqueues every SeiNodeTask in the same namespace
// that targets the changed SeiNode by name. Cheap: namespace-scoped list
// from the controller-runtime cache, O(tasks in ns).
type seiNodeTargetHandler struct {
	client client.Client
}

func (h *seiNodeTargetHandler) Create(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueueForNode(ctx, e.Object, q)
}
func (h *seiNodeTargetHandler) Update(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueueForNode(ctx, e.ObjectNew, q)
}
func (h *seiNodeTargetHandler) Delete(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueueForNode(ctx, e.Object, q)
}
func (h *seiNodeTargetHandler) Generic(ctx context.Context, e event.GenericEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	h.enqueueForNode(ctx, e.Object, q)
}

func (h *seiNodeTargetHandler) enqueueForNode(ctx context.Context, obj client.Object, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	node, ok := obj.(*seiv1alpha1.SeiNode)
	if !ok {
		return
	}
	list := &seiv1alpha1.SeiNodeTaskList{}
	if err := h.client.List(ctx, list, client.InNamespace(node.Namespace)); err != nil {
		return
	}
	for i := range list.Items {
		t := &list.Items[i]
		if t.Spec.Target.NodeRef.Name != node.Name {
			continue
		}
		q.Add(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: t.Namespace, Name: t.Name}})
	}
}

var _ handler.EventHandler = (*seiNodeTargetHandler)(nil)
