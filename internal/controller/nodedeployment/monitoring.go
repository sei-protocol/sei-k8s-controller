package nodedeployment

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func (r *SeiNodeDeploymentReconciler) reconcileMonitoring(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	if group.Spec.Monitoring == nil || group.Spec.Monitoring.ServiceMonitor == nil {
		removeCondition(group, seiv1alpha1.ConditionServiceMonitorReady)
		return r.deleteUnstructured(ctx, group, serviceMonitorGVK())
	}
	return r.reconcileServiceMonitor(ctx, group)
}

func (r *SeiNodeDeploymentReconciler) reconcileServiceMonitor(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	desired := generateServiceMonitor(group)
	if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on ServiceMonitor: %w", err)
	}

	//nolint:staticcheck // migrating unstructured SSA to typed ApplyConfiguration is a separate effort
	err := r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership)
	if meta.IsNoMatchError(err) {
		if !hasConditionReason(group, seiv1alpha1.ConditionServiceMonitorReady, "CRDNotInstalled") {
			r.Recorder.Event(group, corev1.EventTypeWarning, "CRDNotInstalled", "Prometheus Operator CRD (ServiceMonitor) is not installed; monitoring will not be configured")
		}
		setCondition(group, seiv1alpha1.ConditionServiceMonitorReady, metav1.ConditionFalse,
			"CRDNotInstalled", "Prometheus Operator CRD (ServiceMonitor) is not installed")
		return nil
	}
	if err != nil {
		return err
	}
	if !hasConditionReason(group, seiv1alpha1.ConditionServiceMonitorReady, "ServiceMonitorReady") {
		r.Recorder.Event(group, corev1.EventTypeNormal, "ServiceMonitorReady", "ServiceMonitor reconciled successfully")
	}
	setCondition(group, seiv1alpha1.ConditionServiceMonitorReady, metav1.ConditionTrue,
		"ServiceMonitorReady", "ServiceMonitor reconciled successfully")
	return nil
}

func generateServiceMonitor(group *seiv1alpha1.SeiNodeDeployment) *unstructured.Unstructured {
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
				"endpoints": []any{endpointSpec(group, interval)},
			},
		},
	}
	return sm
}

func endpointSpec(group *seiv1alpha1.SeiNodeDeployment, interval string) map[string]any {
	ep := map[string]any{
		"port":     "metrics",
		"interval": interval,
	}
	var relabelings []any
	if component := deriveComponent(&group.Spec.Template.Spec); component != "" {
		relabelings = append(relabelings, map[string]any{
			"action":      "replace",
			"regex":       ".*",
			"replacement": component,
			"targetLabel": "component",
		})
	}
	if chainID := group.Spec.Template.Spec.ChainID; chainID != "" {
		relabelings = append(relabelings, map[string]any{
			"action":      "replace",
			"regex":       ".*",
			"replacement": chainID,
			"targetLabel": "chain_id",
		})
	}
	if len(relabelings) > 0 {
		ep["metricRelabelings"] = relabelings
	}
	return ep
}

// deriveComponent returns the `component` label value for the SND's role,
// or "" if no role is set (caller omits the relabeling in that case).
func deriveComponent(spec *seiv1alpha1.SeiNodeSpec) string {
	switch {
	case spec.Validator != nil:
		return "validator"
	case spec.Archive != nil:
		return "archive"
	case spec.Replayer != nil:
		return "replayer"
	case spec.FullNode != nil:
		return "node"
	}
	return ""
}

func serviceMonitorGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "monitoring.coreos.com",
		Version: "v1",
		Kind:    "ServiceMonitor",
	}
}
