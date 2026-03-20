package nodegroup

import (
	"context"
	"fmt"
	"maps"

	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func (r *SeiNodeGroupReconciler) reconcileNetworking(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	if group.Spec.Networking == nil {
		return nil
	}

	if err := r.reconcileExternalService(ctx, group); err != nil {
		return fmt.Errorf("reconciling external service: %w", err)
	}
	if err := r.reconcileRoute(ctx, group); err != nil {
		return fmt.Errorf("reconciling route: %w", err)
	}
	if err := r.reconcileIsolation(ctx, group); err != nil {
		return fmt.Errorf("reconciling isolation: %w", err)
	}
	return nil
}

// --- External Service ---

func (r *SeiNodeGroupReconciler) reconcileExternalService(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	if group.Spec.Networking.Service == nil {
		return nil
	}

	desired := generateExternalService(group)
	desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	return r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership)
}

func generateExternalService(group *seiv1alpha1.SeiNodeGroup) *corev1.Service {
	svcConfig := group.Spec.Networking.Service
	labels := resourceLabels(group)

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalServiceName(group),
			Namespace: group.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcConfig.Type,
			Selector: groupSelector(group),
			Ports:    externalServicePorts(svcConfig.Ports),
		},
	}
	if len(svcConfig.Annotations) > 0 {
		svc.Annotations = make(map[string]string, len(svcConfig.Annotations))
		maps.Copy(svc.Annotations, svcConfig.Annotations)
	}
	return svc
}

func externalServicePorts(portNames []seiv1alpha1.PortName) []corev1.ServicePort {
	allPorts := seiconfig.NodePorts()

	if len(portNames) == 0 {
		ports := make([]corev1.ServicePort, len(allPorts))
		for i, p := range allPorts {
			ports[i] = corev1.ServicePort{
				Name: p.Name, Port: p.Port,
				TargetPort: intstr.FromInt32(p.Port),
				Protocol:   corev1.ProtocolTCP,
			}
		}
		return ports
	}

	wanted := make(map[string]bool, len(portNames))
	for _, pn := range portNames {
		wanted[string(pn)] = true
	}

	var ports []corev1.ServicePort
	for _, p := range allPorts {
		if wanted[p.Name] {
			ports = append(ports, corev1.ServicePort{
				Name: p.Name, Port: p.Port,
				TargetPort: intstr.FromInt32(p.Port),
				Protocol:   corev1.ProtocolTCP,
			})
		}
	}
	return ports
}

// --- Ingress ---

func (r *SeiNodeGroupReconciler) reconcileRoute(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	if group.Spec.Networking.Ingress != nil {
		r.deleteStaleHTTPRoute(ctx, group)
		return r.reconcileIngress(ctx, group)
	}
	if group.Spec.Networking.Gateway != nil {
		r.deleteStaleIngress(ctx, group)
		return r.reconcileHTTPRoute(ctx, group)
	}
	return nil
}

func (r *SeiNodeGroupReconciler) deleteStaleHTTPRoute(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(httpRouteGVK())
	obj.SetName(group.Name)
	obj.SetNamespace(group.Namespace)
	_ = client.IgnoreNotFound(r.Delete(ctx, obj))
}

func (r *SeiNodeGroupReconciler) deleteStaleIngress(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) {
	ing := &networkingv1.Ingress{}
	if err := r.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: group.Namespace}, ing); err == nil {
		_ = client.IgnoreNotFound(r.Delete(ctx, ing))
	}
}

func (r *SeiNodeGroupReconciler) reconcileIngress(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	desired := generateIngress(group)
	if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}

	existing := &networkingv1.Ingress{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return err
		}
		setCondition(group, seiv1alpha1.ConditionRouteReady, metav1.ConditionTrue,
			"IngressReady", "Ingress reconciled successfully")
		return nil
	}
	if err != nil {
		return err
	}

	existing.Spec = desired.Spec
	existing.Labels = desired.Labels
	existing.Annotations = desired.Annotations
	if err := r.Update(ctx, existing); err != nil {
		return err
	}
	setCondition(group, seiv1alpha1.ConditionRouteReady, metav1.ConditionTrue,
		"IngressReady", "Ingress reconciled successfully")
	return nil
}

func generateIngress(group *seiv1alpha1.SeiNodeGroup) *networkingv1.Ingress {
	cfg := group.Spec.Networking.Ingress
	labels := resourceLabels(group)

	svcName := externalServiceName(group)
	paths := buildIngressPaths(cfg, svcName)

	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      group.Name,
			Namespace: group.Namespace,
			Labels:    labels,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: cfg.ClassName,
			Rules: []networkingv1.IngressRule{{
				Host: cfg.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: paths,
					},
				},
			}},
		},
	}

	if len(cfg.Annotations) > 0 {
		ing.Annotations = make(map[string]string, len(cfg.Annotations))
		maps.Copy(ing.Annotations, cfg.Annotations)
	}

	if cfg.TLS != nil && cfg.TLS.Enabled {
		tls := networkingv1.IngressTLS{Hosts: []string{cfg.Host}}
		if cfg.TLS.SecretName != nil {
			tls.SecretName = *cfg.TLS.SecretName
		}
		ing.Spec.TLS = []networkingv1.IngressTLS{tls}
	}

	return ing
}

func buildIngressPaths(cfg *seiv1alpha1.IngressConfig, svcName string) []networkingv1.HTTPIngressPath {
	if len(cfg.Paths) > 0 {
		paths := make([]networkingv1.HTTPIngressPath, len(cfg.Paths))
		for i, p := range cfg.Paths {
			paths[i] = networkingv1.HTTPIngressPath{
				Path:     p.Path,
				PathType: &p.PathType,
				Backend: networkingv1.IngressBackend{
					Service: &networkingv1.IngressServiceBackend{
						Name: svcName,
						Port: networkingv1.ServiceBackendPort{Name: string(p.PortName)},
					},
				},
			}
		}
		return paths
	}

	prefix := networkingv1.PathTypePrefix
	return []networkingv1.HTTPIngressPath{{
		Path:     "/",
		PathType: &prefix,
		Backend: networkingv1.IngressBackend{
			Service: &networkingv1.IngressServiceBackend{
				Name: svcName,
				Port: networkingv1.ServiceBackendPort{Name: "rpc"},
			},
		},
	}}
}

// --- HTTPRoute (unstructured) ---

func (r *SeiNodeGroupReconciler) reconcileHTTPRoute(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	desired := generateHTTPRoute(group)
	if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on HTTPRoute: %w", err)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(httpRouteGVK())
	err := r.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: group.Namespace}, existing)
	if meta.IsNoMatchError(err) {
		setCondition(group, seiv1alpha1.ConditionRouteReady, metav1.ConditionFalse,
			"CRDNotInstalled", "Gateway API CRD (HTTPRoute) is not installed")
		return nil
	}
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return err
		}
		setCondition(group, seiv1alpha1.ConditionRouteReady, metav1.ConditionTrue,
			"HTTPRouteReady", "HTTPRoute reconciled successfully")
		return nil
	}
	if err != nil {
		return err
	}

	desired.SetResourceVersion(existing.GetResourceVersion())
	if err := r.Update(ctx, desired); err != nil {
		return err
	}
	setCondition(group, seiv1alpha1.ConditionRouteReady, metav1.ConditionTrue,
		"HTTPRouteReady", "HTTPRoute reconciled successfully")
	return nil
}

func generateHTTPRoute(group *seiv1alpha1.SeiNodeGroup) *unstructured.Unstructured {
	cfg := group.Spec.Networking.Gateway
	svcName := externalServiceName(group)

	hostnames := make([]interface{}, len(cfg.Hostnames))
	for i, h := range cfg.Hostnames {
		hostnames[i] = h
	}

	parentRef := map[string]interface{}{
		"name":      cfg.ParentRef.Name,
		"namespace": cfg.ParentRef.Namespace,
	}
	if cfg.ParentRef.SectionName != nil {
		parentRef["sectionName"] = *cfg.ParentRef.SectionName
	}

	route := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]interface{}{
				"name":        group.Name,
				"namespace":   group.Namespace,
				"labels":      toStringInterfaceMap(resourceLabels(group)),
				"annotations": toStringInterfaceMap(managedByAnnotations()),
			},
			"spec": map[string]interface{}{
				"parentRefs": []interface{}{parentRef},
				"hostnames": hostnames,
				"rules": []interface{}{
					map[string]interface{}{
						"backendRefs": []interface{}{
							map[string]interface{}{
								"name": svcName,
								"port": int64(seiconfig.PortRPC),
							},
						},
					},
				},
			},
		},
	}

	if len(cfg.Annotations) > 0 {
		metadata := route.Object["metadata"].(map[string]interface{})
		annotations := metadata["annotations"].(map[string]interface{})
		for k, v := range cfg.Annotations {
			annotations[k] = v
		}
	}

	return route
}

func httpRouteGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1",
		Kind:    "HTTPRoute",
	}
}

// --- AuthorizationPolicy (unstructured) ---

func (r *SeiNodeGroupReconciler) reconcileIsolation(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	if group.Spec.Networking.Isolation == nil || group.Spec.Networking.Isolation.AuthorizationPolicy == nil {
		return nil
	}

	if r.ControllerSA == "" {
		log.FromContext(ctx).Info("WARNING: SEI_CONTROLLER_SA_PRINCIPAL is not set; AuthorizationPolicy will not include controller SA, sidecar communication may be blocked")
		setCondition(group, seiv1alpha1.ConditionIsolationReady, metav1.ConditionFalse,
			"ControllerSAMissing", "SEI_CONTROLLER_SA_PRINCIPAL env var is not set; controller SA will not be injected into AuthorizationPolicy")
	}

	desired := generateAuthorizationPolicy(group, r.ControllerSA)
	if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on AuthorizationPolicy: %w", err)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(authPolicyGVK())
	err := r.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: group.Namespace}, existing)
	if meta.IsNoMatchError(err) {
		setCondition(group, seiv1alpha1.ConditionIsolationReady, metav1.ConditionFalse,
			"CRDNotInstalled", "Istio CRD (AuthorizationPolicy) is not installed")
		return nil
	}
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return err
		}
		if r.ControllerSA != "" {
			setCondition(group, seiv1alpha1.ConditionIsolationReady, metav1.ConditionTrue,
				"AuthorizationPolicyReady", "AuthorizationPolicy reconciled successfully")
		}
		return nil
	}
	if err != nil {
		return err
	}

	desired.SetResourceVersion(existing.GetResourceVersion())
	if err := r.Update(ctx, desired); err != nil {
		return err
	}
	if r.ControllerSA != "" {
		setCondition(group, seiv1alpha1.ConditionIsolationReady, metav1.ConditionTrue,
			"AuthorizationPolicyReady", "AuthorizationPolicy reconciled successfully")
	}
	return nil
}

func generateAuthorizationPolicy(group *seiv1alpha1.SeiNodeGroup, controllerSA string) *unstructured.Unstructured {
	cfg := group.Spec.Networking.Isolation.AuthorizationPolicy

	var rules []interface{}
	for _, src := range cfg.AllowedSources {
		rule := map[string]interface{}{}
		from := map[string]interface{}{}
		source := map[string]interface{}{}
		if len(src.Principals) > 0 {
			principals := make([]interface{}, len(src.Principals))
			for i, p := range src.Principals {
				principals[i] = p
			}
			source["principals"] = principals
		}
		if len(src.Namespaces) > 0 {
			namespaces := make([]interface{}, len(src.Namespaces))
			for i, n := range src.Namespaces {
				namespaces[i] = n
			}
			source["namespaces"] = namespaces
		}
		from["source"] = source
		rule["from"] = []interface{}{from}
		rules = append(rules, rule)
	}

	// Auto-inject the controller's SA so sidecar communication is never blocked
	if controllerSA != "" {
		rules = append(rules, map[string]interface{}{
			"from": []interface{}{
				map[string]interface{}{
					"source": map[string]interface{}{
						"principals": []interface{}{controllerSA},
					},
				},
			},
		})
	}

	policy := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "security.istio.io/v1",
			"kind":       "AuthorizationPolicy",
			"metadata": map[string]interface{}{
				"name":        group.Name,
				"namespace":   group.Namespace,
				"labels":      toStringInterfaceMap(resourceLabels(group)),
				"annotations": toStringInterfaceMap(managedByAnnotations()),
			},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{
					"matchLabels": toStringInterfaceMap(groupSelector(group)),
				},
				"action": "ALLOW",
				"rules":  rules,
			},
		},
	}
	return policy
}

func authPolicyGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "security.istio.io",
		Version: "v1",
		Kind:    "AuthorizationPolicy",
	}
}

// --- Deletion policy helpers ---

func (r *SeiNodeGroupReconciler) deleteNetworkingResources(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: externalServiceName(group), Namespace: group.Namespace}, svc); err == nil {
		if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	ing := &networkingv1.Ingress{}
	if err := r.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: group.Namespace}, ing); err == nil {
		if err := r.Delete(ctx, ing); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}

	r.deleteUnstructured(ctx, group, httpRouteGVK())   //nolint:errcheck // best-effort
	r.deleteUnstructured(ctx, group, authPolicyGVK())   //nolint:errcheck // best-effort
	r.deleteUnstructured(ctx, group, serviceMonitorGVK()) //nolint:errcheck // best-effort

	return nil
}

func (r *SeiNodeGroupReconciler) deleteUnstructured(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, gvk schema.GroupVersionKind) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(group.Name)
	obj.SetNamespace(group.Namespace)
	return client.IgnoreNotFound(r.Delete(ctx, obj))
}

func (r *SeiNodeGroupReconciler) orphanNetworkingResources(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: externalServiceName(group), Namespace: group.Namespace}, svc); err == nil {
		if err := r.removeOwnerRef(ctx, svc, group); err != nil {
			return fmt.Errorf("orphaning external Service: %w", err)
		}
	}

	ing := &networkingv1.Ingress{}
	if err := r.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: group.Namespace}, ing); err == nil {
		if err := r.removeOwnerRef(ctx, ing, group); err != nil {
			return fmt.Errorf("orphaning Ingress: %w", err)
		}
	}

	for _, gvk := range []schema.GroupVersionKind{httpRouteGVK(), authPolicyGVK(), serviceMonitorGVK()} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := r.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: group.Namespace}, obj); err == nil {
			if err := r.removeOwnerRef(ctx, obj, group); err != nil {
				return fmt.Errorf("orphaning %s: %w", gvk.Kind, err)
			}
		}
	}
	return nil
}

func (r *SeiNodeGroupReconciler) removeOwnerRef(ctx context.Context, obj client.Object, owner *seiv1alpha1.SeiNodeGroup) error {
	refs := obj.GetOwnerReferences()
	filtered := make([]metav1.OwnerReference, 0, len(refs))
	for _, ref := range refs {
		if ref.UID != owner.UID {
			filtered = append(filtered, ref)
		}
	}
	if len(filtered) == len(refs) {
		return nil
	}
	obj.SetOwnerReferences(filtered)
	return r.Update(ctx, obj)
}

func (r *SeiNodeGroupReconciler) orphanChildSeiNodes(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	nodes, err := r.listChildSeiNodes(ctx, group)
	if err != nil {
		return err
	}
	for i := range nodes {
		node := &nodes[i]
		if err := r.removeOwnerRef(ctx, node, group); err != nil {
			return fmt.Errorf("orphaning SeiNode %s: %w", node.Name, err)
		}
	}
	return nil
}

func toStringInterfaceMap(m map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
