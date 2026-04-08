package nodedeployment

import (
	"context"
	"fmt"
	"maps"
	"net"

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
	"sigs.k8s.io/controller-runtime/pkg/log"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

var seiProtocolRoutes = []struct {
	Prefix string
	Port   int32
}{
	{"rpc", seiconfig.PortRPC},
	{"rest", seiconfig.PortREST},
	{"grpc", seiconfig.PortGRPC},
	{"evm-rpc", seiconfig.PortEVMHTTP},
	{"evm-ws", seiconfig.PortEVMWS},
}

type effectiveRoute struct {
	Name      string
	Hostnames []string
	Port      int32
}

// hasExternalService returns true when the deployment has an external Service configured.
func (r *SeiNodeDeploymentReconciler) hasExternalService(group *seiv1alpha1.SeiNodeDeployment) bool {
	return group.Spec.Networking != nil && group.Spec.Networking.Service != nil
}

func (r *SeiNodeDeploymentReconciler) reconcileNetworking(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	if group.Spec.Networking == nil {
		removeCondition(group, seiv1alpha1.ConditionExternalServiceReady)
		removeCondition(group, seiv1alpha1.ConditionRouteReady)
		removeCondition(group, seiv1alpha1.ConditionIsolationReady)
		return r.deleteNetworkingResources(ctx, group)
	}

	if err := r.reconcileExternalService(ctx, group); err != nil {
		return fmt.Errorf("reconciling external service: %w", err)
	}
	if err := r.reconcileExternalAddress(ctx, group); err != nil {
		return fmt.Errorf("reconciling external P2P address: %w", err)
	}
	if err := r.reconcileRoute(ctx, group); err != nil {
		return fmt.Errorf("reconciling route: %w", err)
	}
	if err := r.reconcileIsolation(ctx, group); err != nil {
		return fmt.Errorf("reconciling isolation: %w", err)
	}
	return nil
}

// reconcileExternalAddress propagates the external P2P address from the
// LoadBalancer Service to child SeiNode status so that the planner can
// inject p2p.external_address into CometBFT config at plan build time.
func (r *SeiNodeDeploymentReconciler) reconcileExternalAddress(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	addr := r.resolveExternalP2PAddress(ctx, group)
	if addr == "" {
		return nil
	}

	nodes, err := r.listChildSeiNodes(ctx, group)
	if err != nil {
		return fmt.Errorf("listing child SeiNodes: %w", err)
	}

	for i := range nodes {
		node := &nodes[i]
		if node.Status.ExternalAddress == addr {
			continue
		}
		patch := client.MergeFrom(node.DeepCopy())
		node.Status.ExternalAddress = addr
		if err := r.Status().Patch(ctx, node, patch); err != nil {
			return fmt.Errorf("patching external address status on SeiNode %s: %w", node.Name, err)
		}
		log.FromContext(ctx).Info("set P2P external address on status", "node", node.Name, "address", addr)
	}
	return nil
}

// resolveExternalP2PAddress returns the routable P2P address for this
// deployment's nodes, or "" if not yet available. Returns empty if the
// hostname is assigned but not yet resolvable in DNS.
func (r *SeiNodeDeploymentReconciler) resolveExternalP2PAddress(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) string {
	svc, err := r.fetchExternalService(ctx, group)
	if err != nil {
		return ""
	}
	addr := externalAddressFromService(svc)
	if addr == "" {
		return ""
	}
	// Verify the hostname resolves before using it — NLB hostnames are
	// assigned immediately by AWS but DNS propagation takes a moment.
	host, _, _ := net.SplitHostPort(addr)
	if net.ParseIP(host) == nil {
		if _, err := net.DefaultResolver.LookupHost(ctx, host); err != nil {
			log.FromContext(ctx).Info("external address not yet resolvable", "host", host)
			return ""
		}
	}
	return addr
}

// externalAddressFromService extracts the P2P address from a Service's
// LoadBalancer ingress. Prefers hostname (DNS) over IP.
func externalAddressFromService(svc *corev1.Service) string {
	if svc == nil {
		return ""
	}
	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		host := ingress.Hostname
		if host == "" {
			host = ingress.IP
		}
		if host != "" {
			return fmt.Sprintf("%s:%d", host, seiconfig.PortP2P)
		}
	}
	return ""
}

func (r *SeiNodeDeploymentReconciler) reconcileExternalService(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
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

func generateExternalService(group *seiv1alpha1.SeiNodeDeployment) *corev1.Service {
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
			Ports:    portsForMode(groupMode(group)),
		},
	}
	if len(svcConfig.Annotations) > 0 {
		svc.Annotations = make(map[string]string, len(svcConfig.Annotations))
		maps.Copy(svc.Annotations, svcConfig.Annotations)
	}
	return svc
}

func groupMode(group *seiv1alpha1.SeiNodeDeployment) seiconfig.NodeMode {
	spec := group.Spec.Template.Spec
	switch {
	case spec.Archive != nil:
		return seiconfig.ModeArchive
	case spec.Validator != nil:
		return seiconfig.ModeValidator
	default:
		return seiconfig.ModeFull
	}
}

func portsForMode(mode seiconfig.NodeMode) []corev1.ServicePort {
	np := seiconfig.NodePortsForMode(mode)
	ports := make([]corev1.ServicePort, len(np))
	for i, p := range np {
		ports[i] = corev1.ServicePort{
			Name: p.Name, Port: p.Port,
			TargetPort: intstr.FromInt32(p.Port),
			Protocol:   corev1.ProtocolTCP,
		}
	}
	return ports
}

func (r *SeiNodeDeploymentReconciler) reconcileRoute(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	if group.Spec.Networking.Gateway == nil {
		removeCondition(group, seiv1alpha1.ConditionRouteReady)
		return r.deleteHTTPRoutesByLabel(ctx, group)
	}
	return r.reconcileHTTPRoute(ctx, group)
}

func (r *SeiNodeDeploymentReconciler) deleteHTTPRoutesByLabel(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
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

func resolveEffectiveRoutes(group *seiv1alpha1.SeiNodeDeployment) []effectiveRoute {
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

func (r *SeiNodeDeploymentReconciler) reconcileHTTPRoute(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	routes := resolveEffectiveRoutes(group)

	desiredNames := make(map[string]bool, len(routes))
	for _, er := range routes {
		desiredNames[er.Name] = true
	}

	for _, er := range routes {
		desired := generateHTTPRoute(group, er, r.GatewayName, r.GatewayNamespace)
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

func (r *SeiNodeDeploymentReconciler) deleteOrphanedHTTPRoutes(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, desiredNames map[string]bool) error {
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

func generateHTTPRoute(group *seiv1alpha1.SeiNodeDeployment, er effectiveRoute, gatewayName, gatewayNamespace string) *unstructured.Unstructured {
	svcName := externalServiceName(group)

	hostnames := make([]any, len(er.Hostnames))
	for i, h := range er.Hostnames {
		hostnames[i] = h
	}

	parentRef := map[string]any{
		"name":      gatewayName,
		"namespace": gatewayNamespace,
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

	if gw := group.Spec.Networking.Gateway; gw != nil && len(gw.Annotations) > 0 {
		metadata := route.Object["metadata"].(map[string]any)
		annotations := metadata["annotations"].(map[string]any)
		for k, v := range gw.Annotations {
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

func (r *SeiNodeDeploymentReconciler) reconcileIsolation(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
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

func generateAuthorizationPolicy(group *seiv1alpha1.SeiNodeDeployment, controllerSA string) *unstructured.Unstructured {
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

func (r *SeiNodeDeploymentReconciler) deleteExternalService(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
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

func (r *SeiNodeDeploymentReconciler) deleteNetworkingResources(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
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

func (r *SeiNodeDeploymentReconciler) deleteUnstructured(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, gvk schema.GroupVersionKind) error {
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

func (r *SeiNodeDeploymentReconciler) orphanNetworkingResources(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
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

func (r *SeiNodeDeploymentReconciler) removeOwnerRef(ctx context.Context, obj client.Object, owner *seiv1alpha1.SeiNodeDeployment) error {
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

func (r *SeiNodeDeploymentReconciler) orphanChildSeiNodes(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
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
