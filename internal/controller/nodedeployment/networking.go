package nodedeployment

import (
	"context"
	"fmt"
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
	{"evm", seiconfig.PortEVMHTTP},
	{"rpc", seiconfig.PortRPC},
	{"rest", seiconfig.PortREST},
	{"grpc", seiconfig.PortGRPC},
}

type effectiveRoute struct {
	Name      string
	Protocol  string
	Hostnames []string
	Port      int32
	WSPort    int32
}

// routeHostnameResolvable returns true when the deployment's first public
// hostname resolves in DNS, indicating the HTTPRoute + External-DNS pipeline
// is ready. Returns true when no routes are expected (private deployments,
// validator mode).
func (r *SeiNodeDeploymentReconciler) routeHostnameResolvable(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) bool {
	if group.Spec.Networking == nil {
		return true
	}
	routes := resolveEffectiveRoutes(group, r.GatewayDomain, r.GatewayPublicDomain)
	if len(routes) == 0 {
		return true
	}
	hostname := routes[0].Hostnames[0]
	if _, err := net.DefaultResolver.LookupHost(ctx, hostname); err != nil {
		log.FromContext(ctx).Info("route hostname not yet resolvable", "hostname", hostname)
		return false
	}
	return true
}

func (r *SeiNodeDeploymentReconciler) reconcileNetworking(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	if group.Spec.Networking == nil {
		removeCondition(group, seiv1alpha1.ConditionRouteReady)
		return r.deleteNetworkingResources(ctx, group)
	}

	if err := r.reconcileExternalService(ctx, group); err != nil {
		return fmt.Errorf("reconciling external service: %w", err)
	}
	if err := r.reconcileRoute(ctx, group); err != nil {
		return fmt.Errorf("reconciling route: %w", err)
	}
	return nil
}

func (r *SeiNodeDeploymentReconciler) reconcileExternalService(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	desired := generateExternalService(group)
	desired.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
	if err := ctrl.SetControllerReference(group, desired, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	//nolint:staticcheck // migrating to typed ApplyConfiguration is a separate effort
	return r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership)
}

func generateExternalService(group *seiv1alpha1.SeiNodeDeployment) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      externalServiceName(group),
			Namespace: group.Namespace,
			Labels:    resourceLabels(group),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: groupSelector(group),
			Ports:    portsForMode(groupMode(group)),
		},
	}
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
		if p.Name == "grpc" {
			h2c := "kubernetes.io/h2c"
			ports[i].AppProtocol = &h2c
		}
	}
	return ports
}

func (r *SeiNodeDeploymentReconciler) reconcileRoute(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	routes := resolveEffectiveRoutes(group, r.GatewayDomain, r.GatewayPublicDomain)
	if len(routes) == 0 {
		removeCondition(group, seiv1alpha1.ConditionRouteReady)
		return r.deleteHTTPRoutesByLabel(ctx, group)
	}
	return r.reconcileHTTPRoutes(ctx, group, routes)
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

func resolveEffectiveRoutes(group *seiv1alpha1.SeiNodeDeployment, domain, publicDomain string) []effectiveRoute {
	modePorts := seiconfig.NodePortsForMode(groupMode(group))

	activePorts := make(map[string]bool, len(modePorts))
	for _, p := range modePorts {
		activePorts[p.Name] = true
	}

	var routes []effectiveRoute
	for _, proto := range seiProtocolRoutes {
		if !isProtocolActiveForMode(proto.Prefix, activePorts) {
			continue
		}
		subdomain := fmt.Sprintf("%s-%s", group.Name, proto.Prefix)
		hostnames := []string{
			fmt.Sprintf("%s.%s", subdomain, domain),
		}
		if publicDomain != "" {
			hostnames = append(hostnames,
				fmt.Sprintf("%s.%s.%s", subdomain, group.Namespace, publicDomain),
			)
		}
		er := effectiveRoute{
			Name:      subdomain,
			Protocol:  proto.Prefix,
			Hostnames: hostnames,
			Port:      proto.Port,
		}
		if proto.Prefix == "evm" && activePorts["evm-ws"] {
			er.WSPort = seiconfig.PortEVMWS
		}
		routes = append(routes, er)
	}
	return routes
}

func isProtocolActiveForMode(prefix string, activePorts map[string]bool) bool {
	if prefix == "evm" {
		return activePorts["evm-rpc"]
	}
	return activePorts[prefix]
}

func (r *SeiNodeDeploymentReconciler) reconcileHTTPRoutes(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, routes []effectiveRoute) error {
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

	rules := []any{
		map[string]any{
			"backendRefs": []any{
				map[string]any{
					"name": svcName,
					"port": int64(er.Port),
				},
			},
		},
	}
	if er.WSPort != 0 {
		rules = append(rules, map[string]any{
			"matches": []any{
				map[string]any{
					"headers": []any{
						map[string]any{
							"type":  "Exact",
							"name":  "Upgrade",
							"value": "websocket",
						},
					},
				},
			},
			"backendRefs": []any{
				map[string]any{
					"name": svcName,
					"port": int64(er.WSPort),
				},
			},
		})
	}

	return &unstructured.Unstructured{
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
				"rules":      rules,
			},
		},
	}
}

func httpRouteGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   "gateway.networking.k8s.io",
		Version: "v1",
		Kind:    "HTTPRoute",
	}
}

// --- Deletion helpers ---

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
	return nil
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

	for _, gvk := range []schema.GroupVersionKind{httpRouteGVK(), serviceMonitorGVK()} {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)
		listErr := r.List(ctx, list, client.InNamespace(group.Namespace), client.MatchingLabels(resourceLabels(group)))
		if listErr != nil && !meta.IsNoMatchError(listErr) {
			return fmt.Errorf("listing %s for orphan: %w", gvk.Kind, listErr)
		}
		if listErr == nil {
			for i := range list.Items {
				if err := r.removeOwnerRef(ctx, &list.Items[i], group); err != nil {
					return fmt.Errorf("orphaning %s %s: %w", gvk.Kind, list.Items[i].GetName(), err)
				}
			}
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
