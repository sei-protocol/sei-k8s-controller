package nodepool

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	ConditionTypeReady       = "Ready"
	ConditionTypeDegraded    = "Degraded"
	ConditionTypeProgressing = "Progressing"
)

const (
	ReasonReconcileSucceeded = "ReconcileSucceeded"
	ReasonReconcileFailed    = "ReconcileFailed"
	ReasonPodsNotReady       = "PodsNotReady"
	ReasonAllPodsReady       = "AllPodsReady"
	ReasonGenesisJobFailed   = "GenesisJobFailed"
	ReasonPrepJobFailed      = "PrepJobFailed"
)

func setCondition(sn *seiv1alpha1.SeiNodePool, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&sn.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		ObservedGeneration: sn.Generation,
		Message:            message,
	})
}

func (r *SeiNodePoolReconciler) patchStatus(ctx context.Context, sn *seiv1alpha1.SeiNodePool, patch client.Patch) error {
	return r.Status().Patch(ctx, sn, patch)
}
