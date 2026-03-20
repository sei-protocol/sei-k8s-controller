package nodegroup

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func (r *SeiNodeGroupReconciler) reconcileMonitoring(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	if group.Spec.Monitoring == nil || group.Spec.Monitoring.ServiceMonitor == nil {
		return nil
	}
	return r.reconcileServiceMonitor(ctx, group)
}

func (r *SeiNodeGroupReconciler) reconcileServiceMonitor(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	desired := generateServiceMonitor(group)
	if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on ServiceMonitor: %w", err)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(serviceMonitorGVK())
	err := r.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: group.Namespace}, existing)
	if meta.IsNoMatchError(err) {
		setCondition(group, seiv1alpha1.ConditionServiceMonitorReady, metav1.ConditionFalse,
			"CRDNotInstalled", "Prometheus Operator CRD (ServiceMonitor) is not installed")
		return nil
	}
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return err
		}
		setCondition(group, seiv1alpha1.ConditionServiceMonitorReady, metav1.ConditionTrue,
			"ServiceMonitorReady", "ServiceMonitor reconciled successfully")
		return nil
	}
	if err != nil {
		return err
	}

	desired.SetResourceVersion(existing.GetResourceVersion())
	if err := r.Update(ctx, desired); err != nil {
		return err
	}
	setCondition(group, seiv1alpha1.ConditionServiceMonitorReady, metav1.ConditionTrue,
		"ServiceMonitorReady", "ServiceMonitor reconciled successfully")
	return nil
}

func generateServiceMonitor(group *seiv1alpha1.SeiNodeGroup) *unstructured.Unstructured {
	cfg := group.Spec.Monitoring.ServiceMonitor

	interval := cfg.Interval
	if interval == "" {
		interval = "30s"
	}

	labels := make(map[string]any)
	for k, v := range cfg.Labels {
		labels[k] = v
	}
	for k, v := range resourceLabels(group) {
		labels[k] = v
	}

	sm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "monitoring.coreos.com/v1",
			"kind":       "ServiceMonitor",
			"metadata": map[string]any{
				"name":        group.Name,
				"namespace":   group.Namespace,
				"labels":      labels,
				"annotations": toStringInterfaceMap(managedByAnnotations()),
			},
			"spec": map[string]any{
				"selector": map[string]any{
					"matchLabels": toStringInterfaceMap(groupSelector(group)),
				},
				"endpoints": []any{
					map[string]any{
						"port":     "metrics",
						"interval": interval,
					},
				},
			},
		},
	}
	return sm
}

func serviceMonitorGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "ServiceMonitor",
	}
}
