package nodedeployment

import (
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// childPhaseChangedPredicate filters Update events from owned SeiNodes to
// only pass through phase transitions (e.g. Initializing → Running). This
// prevents the high-frequency task retry status patches on child nodes from
// triggering group reconciliation, which would bypass the executor's
// exponential backoff on retried group-level tasks.
//
// Create, Delete, and Generic events always pass through. Update events
// also pass through if the child's spec changed (generation bump).
func childPhaseChangedPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(_ event.CreateEvent) bool { return true },
		DeleteFunc:  func(_ event.DeleteEvent) bool { return true },
		GenericFunc: func(_ event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, ok := e.ObjectOld.(*seiv1alpha1.SeiNode)
			if !ok {
				return true
			}
			newNode, ok := e.ObjectNew.(*seiv1alpha1.SeiNode)
			if !ok {
				return true
			}
			if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
				return true
			}
			return oldNode.Status.Phase != newNode.Status.Phase
		},
	}
}
