package nodegroup

import (
	"context"
	"fmt"
	"maps"

	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const portRest = int32(1317)

var seiProtocolRoutes = []struct {
	Prefix string
	Port   int32
}{
	{"rpc", seiconfig.PortRPC},
	{"rest", portRest},
	{"grpc", 9090},
	{"evm-rpc", 8545},
	{"evm-ws", 8546},
}

type effectiveRoute struct {
	Name      string
	Hostnames []string
	Port      int32
}

func (r *SeiNodeGroupReconciler) reconcileNetworking(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	if group.Spec.Networking == nil {
		removeCondition(group, seiv1alpha1.ConditionExternalServiceReady)
		removeCondition(group, seiv1alpha1.ConditionRouteReady)
		removeCondition(group, seiv1alpha1.ConditionIsolationReady)
		return r.deleteNetworkingResources(ctx, group)
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

func (r *SeiNodeGroupReconciler) reconcileExternalService(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	if group.Spec.Networking.Service == nil {
		removeCondition(group, seiv1alpha1.ConditionExternalServiceReady)
		return r.deleteExternalService(ctx, group)
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
	includeRest := group.Spec.Networking.Gateway != nil && group.Spec.Networking.Gateway.BaseDomain != ""

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalServiceName(group),
			Namespace: group.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcConfig.Type,
			Selector: groupSelector(group),
			Ports:    externalServicePorts(svcConfig.Ports, includeRest),
		},
	}
	if len(svcConfig.Annotations) > 0 {
		svc.Annotations = make(map[string]string, len(svcConfig.Annotations))
		maps.Copy(svc.Annotations, svcConfig.Annotations)
	}
	return svc
}

func externalServicePorts(portNames []seiv1alpha1.PortName, includeRest bool) []corev1.ServicePort {
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
		if includeRest {
			ports = append(ports, corev1.ServicePort{
				Name: "rest", Port: portRest,
				TargetPort: intstr.FromInt32(portRest),
				Protocol:   corev1.ProtocolTCP,
			})
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
	if wanted["rest"] {
		ports = append(ports, corev1.ServicePort{
			Name: "rest", Port: portRest,
			TargetPort: intstr.FromInt32(portRest),
			Protocol:   corev1.ProtocolTCP,
		})
	}
	return ports
}

func (r *SeiNodeGroupReconciler) reconcileRoute(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	if group.Spec.Networking.Gateway == nil {
		removeCondition(group, seiv1alpha1.ConditionRouteReady)
		return r.deleteHTTPRoutesByLabel(ctx, group)
	}
	return r.reconcileHTTPRoute(ctx, group)
}

func (r *SeiNodeGroupReconciler) deleteHTTPRoutesByLabel(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(httpRouteGVK())
	err := r.List(ctx, list, client.InNamespace(group.Namespace), client.MatchingLabels(resourceLabels(group)))
	if meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing HTTPRoutes for deletion: %w", err)
	}
	for i := range list.Items {
		if err := r.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting HTTPRoute %s: %w", list.Items[i].GetName(), err)
		}
	}
	return nil
}

func resolveEffectiveRoutes(group *seiv1alpha1.SeiNodeGroup) []effectiveRoute {
	cfg := group.Spec.Networking.Gateway
	if cfg.BaseDomain != "" {
		routes := make([]effectiveRoute, len(seiProtocolRoutes))
		for i, p := range seiProtocolRoutes {
			routes[i] = effectiveRoute{
				Name:      fmt.Sprintf("%s-%s", group.Name, p.Prefix),
				Hostnames: []string{fmt.Sprintf("%s.%s", p.Prefix, cfg.BaseDomain)},
				Port:      p.Port,
			}
		}
		return routes
	}
	return []effectiveRoute{{
		Name:      group.Name,
		Hostnames: cfg.Hostnames,
		Port:      seiconfig.PortRPC,
	}}
}

func (r *SeiNodeGroupReconciler) reconcileHTTPRoute(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	routes := resolveEffectiveRoutes(group)

	desiredNames := make(map[string]bool, len(routes))
	for _, er := range routes {
		desiredNames[er.Name] = true
	}

	for _, er := range routes {
		desired := generateHTTPRoute(group, er)
		if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
			return fmt.Errorf("setting owner reference on HTTPRoute %s: %w", er.Name, err)
		}

		//nolint:staticcheck // migrating unstructured SSA to typed ApplyConfiguration is a separate effort
		err := r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership)
		if meta.IsNoMatchError(err) {
			if !hasConditionReason(group, seiv1alpha1.ConditionRouteReady, "CRDNotInstalled") {
				r.Recorder.Event(group, corev1.EventTypeWarning, "CRDNotInstalled", "Gateway API CRD (HTTPRoute) is not installed; HTTPRoute will not be created")
			}
			setCondition(group, seiv1alpha1.ConditionRouteReady, metav1.ConditionFalse,
				"CRDNotInstalled", "Gateway API CRD (HTTPRoute) is not installed")
			return nil
		}
		if err != nil {
			return fmt.Errorf("applying HTTPRoute %s: %w", er.Name, err)
		}
	}

	if err := r.deleteOrphanedHTTPRoutes(ctx, group, desiredNames); err != nil {
		return fmt.Errorf("cleaning up orphaned HTTPRoutes: %w", err)
	}

	if !hasConditionReason(group, seiv1alpha1.ConditionRouteReady, "HTTPRouteReady") {
		r.Recorder.Event(group, corev1.EventTypeNormal, "HTTPRouteReady", "HTTPRoute reconciled successfully")
	}
	setCondition(group, seiv1alpha1.ConditionRouteReady, metav1.ConditionTrue,
		"HTTPRouteReady", fmt.Sprintf("%d HTTPRoute(s) reconciled successfully", len(routes)))
	return nil
}

func (r *SeiNodeGroupReconciler) deleteOrphanedHTTPRoutes(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, desiredNames map[string]bool) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(httpRouteGVK())
	err := r.List(ctx, list, client.InNamespace(group.Namespace), client.MatchingLabels(resourceLabels(group)))
	if meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing HTTPRoutes: %w", err)
	}
	for i := range list.Items {
		route := &list.Items[i]
		if !desiredNames[route.GetName()] {
			if err := r.Delete(ctx, route); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting orphaned HTTPRoute %s: %w", route.GetName(), err)
			}
		}
	}
	return nil
}

func generateHTTPRoute(group *seiv1alpha1.SeiNodeGroup, er effectiveRoute) *unstructured.Unstructured {
	cfg := group.Spec.Networking.Gateway
	svcName := externalServiceName(group)

	hostnames := make([]any, len(er.Hostnames))
	for i, h := range er.Hostnames {
		hostnames[i] = h
	}

	parentRef := map[string]any{
		"name":      cfg.ParentRef.Name,
		"namespace": cfg.ParentRef.Namespace,
	}
	if cfg.ParentRef.SectionName != nil {
		parentRef["sectionName"] = *cfg.ParentRef.SectionName
	}

	route := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "gateway.networking.k8s.io/v1",
			"kind":       "HTTPRoute",
			"metadata": map[string]any{
				"name":        er.Name,
				"namespace":   group.Namespace,
				"labels":      toStringInterfaceMap(resourceLabels(group)),
				"annotations": toStringInterfaceMap(managedByAnnotations()),
			},
			"spec": map[string]any{
				"parentRefs": []any{parentRef},
				"hostnames":  hostnames,
				"rules": []any{
					map[string]any{
						"backendRefs": []any{
							map[string]any{
								"name": svcName,
								"port": int64(er.Port),
							},
						},
					},
				},
			},
		},
	}

	if len(cfg.Annotations) > 0 {
		metadata := route.Object["metadata"].(map[string]any)
		annotations := metadata["annotations"].(map[string]any)
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
		removeCondition(group, seiv1alpha1.ConditionIsolationReady)
		return r.deleteUnstructured(ctx, group, authPolicyGVK())
	}

	if r.ControllerSA == "" {
		if !hasConditionReason(group, seiv1alpha1.ConditionIsolationReady, "ControllerSAMissing") {
			r.Recorder.Event(group, corev1.EventTypeWarning, "ControllerSAMissing", "SEI_CONTROLLER_SA_PRINCIPAL is not set; AuthorizationPolicy will not include controller SA, sidecar communication may be blocked")
		}
		setCondition(group, seiv1alpha1.ConditionIsolationReady, metav1.ConditionFalse,
			"ControllerSAMissing", "SEI_CONTROLLER_SA_PRINCIPAL env var is not set; controller SA will not be injected into AuthorizationPolicy")
	}

	desired := generateAuthorizationPolicy(group, r.ControllerSA)
	if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on AuthorizationPolicy: %w", err)
	}

	//nolint:staticcheck // migrating unstructured SSA to typed ApplyConfiguration is a separate effort
	err := r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership)
	if meta.IsNoMatchError(err) {
		if !hasConditionReason(group, seiv1alpha1.ConditionIsolationReady, "CRDNotInstalled") {
			r.Recorder.Event(group, corev1.EventTypeWarning, "CRDNotInstalled", "Istio CRD (AuthorizationPolicy) is not installed; isolation will not be enforced")
		}
		setCondition(group, seiv1alpha1.ConditionIsolationReady, metav1.ConditionFalse,
			"CRDNotInstalled", "Istio CRD (AuthorizationPolicy) is not installed")
		return nil
	}
	if err != nil {
		return err
	}
	if r.ControllerSA != "" {
		if !hasConditionReason(group, seiv1alpha1.ConditionIsolationReady, "AuthorizationPolicyReady") {
			r.Recorder.Event(group, corev1.EventTypeNormal, "AuthorizationPolicyReady", "AuthorizationPolicy reconciled successfully")
		}
		setCondition(group, seiv1alpha1.ConditionIsolationReady, metav1.ConditionTrue,
			"AuthorizationPolicyReady", "AuthorizationPolicy reconciled successfully")
	}
	return nil
}

func generateAuthorizationPolicy(group *seiv1alpha1.SeiNodeGroup, controllerSA string) *unstructured.Unstructured {
	cfg := group.Spec.Networking.Isolation.AuthorizationPolicy

	var rules []any
	for _, src := range cfg.AllowedSources {
		rule := map[string]any{}
		from := map[string]any{}
		source := map[string]any{}
		if len(src.Principals) > 0 {
			principals := make([]any, len(src.Principals))
			for i, p := range src.Principals {
				principals[i] = p
			}
			source["principals"] = principals
		}
		if len(src.Namespaces) > 0 {
			namespaces := make([]any, len(src.Namespaces))
			for i, n := range src.Namespaces {
				namespaces[i] = n
			}
			source["namespaces"] = namespaces
		}
		from["source"] = source
		rule["from"] = []any{from}
		rules = append(rules, rule)
	}

	// Auto-inject the controller's SA so sidecar communication is never blocked
	if controllerSA != "" {
		rules = append(rules, map[string]any{
			"from": []any{
				map[string]any{
					"source": map[string]any{
						"principals": []any{controllerSA},
					},
				},
			},
		})
	}

	policy := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "security.istio.io/v1",
			"kind":       "AuthorizationPolicy",
			"metadata": map[string]any{
				"name":        group.Name,
				"namespace":   group.Namespace,
				"labels":      toStringInterfaceMap(resourceLabels(group)),
				"annotations": toStringInterfaceMap(managedByAnnotations()),
			},
			"spec": map[string]any{
				"selector": map[string]any{
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

func (r *SeiNodeGroupReconciler) deleteExternalService(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: externalServiceName(group), Namespace: group.Namespace}, svc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetching external Service for deletion: %w", err)
	}
	if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *SeiNodeGroupReconciler) deleteNetworkingResources(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	if err := r.deleteExternalService(ctx, group); err != nil {
		return err
	}

	if err := r.deleteHTTPRoutesByLabel(ctx, group); err != nil {
		return fmt.Errorf("deleting HTTPRoutes: %w", err)
	}
	for _, gvk := range []schema.GroupVersionKind{authPolicyGVK(), serviceMonitorGVK()} {
		if err := r.deleteUnstructured(ctx, group, gvk); err != nil {
			return fmt.Errorf("deleting %s: %w", gvk.Kind, err)
		}
	}
	return nil
}

func (r *SeiNodeGroupReconciler) deleteUnstructured(ctx context.Context, group *seiv1alpha1.SeiNodeGroup, gvk schema.GroupVersionKind) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetName(group.Name)
	obj.SetNamespace(group.Namespace)
	err := r.Delete(ctx, obj)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	return err
}

func (r *SeiNodeGroupReconciler) orphanNetworkingResources(ctx context.Context, group *seiv1alpha1.SeiNodeGroup) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: externalServiceName(group), Namespace: group.Namespace}, svc)
	if err == nil {
		if err := r.removeOwnerRef(ctx, svc, group); err != nil {
			return fmt.Errorf("orphaning external Service: %w", err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("fetching external Service for orphan: %w", err)
	}

	httpRoutes := &unstructured.UnstructuredList{}
	httpRoutes.SetGroupVersionKind(httpRouteGVK())
	listErr := r.List(ctx, httpRoutes, client.InNamespace(group.Namespace), client.MatchingLabels(resourceLabels(group)))
	if listErr != nil && !meta.IsNoMatchError(listErr) {
		return fmt.Errorf("listing HTTPRoutes for orphan: %w", listErr)
	}
	if listErr == nil {
		for i := range httpRoutes.Items {
			if err := r.removeOwnerRef(ctx, &httpRoutes.Items[i], group); err != nil {
				return fmt.Errorf("orphaning HTTPRoute %s: %w", httpRoutes.Items[i].GetName(), err)
			}
		}
	}

	for _, gvk := range []schema.GroupVersionKind{authPolicyGVK(), serviceMonitorGVK()} {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		err := r.Get(ctx, types.NamespacedName{Name: group.Name, Namespace: group.Namespace}, obj)
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("fetching %s for orphan: %w", gvk.Kind, err)
		}
		if err := r.removeOwnerRef(ctx, obj, group); err != nil {
			return fmt.Errorf("orphaning %s: %w", gvk.Kind, err)
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
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	obj.SetOwnerReferences(filtered)
	return r.Patch(ctx, obj, patch)
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

func toStringInterfaceMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
