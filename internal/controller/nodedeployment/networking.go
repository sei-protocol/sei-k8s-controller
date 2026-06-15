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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

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

// routeHostnameResolvable reports whether the first HTTP route hostname
// resolves in DNS. Returns true when no HTTP routes are expected.
func (r *SeiNodeDeploymentReconciler) routeHostnameResolvable(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) bool {
	if !group.Spec.Networking.HTTPEnabled() {
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

// reconcileNetworking runs the HTTP (L7 Gateway) and TCP (per-pod L4 NLB)
// sub-reconcilers independently. Both write ConditionNetworkingReady; when
// both are enabled the TCP branch runs second and its outcome wins.
func (r *SeiNodeDeploymentReconciler) reconcileNetworking(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	if group.Spec.Networking == nil {
		setCondition(group, seiv1alpha1.ConditionNetworkingReady, metav1.ConditionFalse,
			"NetworkingDisabled", "spec.networking is unset")
		return r.deleteNetworkingResources(ctx, group)
	}

	httpActive := group.Spec.Networking.HTTPEnabled()
	tcpActive := group.Spec.Networking.TCPEnabled() && r.P2PEndpointDomain != ""

	if httpActive {
		if err := r.reconcileExternalService(ctx, group); err != nil {
			return fmt.Errorf("reconciling external service: %w", err)
		}
		if err := r.reconcileRoute(ctx, group); err != nil {
			return fmt.Errorf("reconciling route: %w", err)
		}
	} else {
		if err := r.deleteExternalService(ctx, group); err != nil {
			return fmt.Errorf("deleting external service: %w", err)
		}
		if err := r.deleteHTTPRoutesByLabel(ctx, group); err != nil {
			return fmt.Errorf("deleting HTTPRoutes: %w", err)
		}
	}

	if tcpActive {
		if err := r.reconcileP2PEndpoints(ctx, group); err != nil {
			return fmt.Errorf("reconciling P2P endpoints: %w", err)
		}
	} else {
		if err := r.deleteP2PEndpoints(ctx, group); err != nil {
			return fmt.Errorf("deleting P2P endpoints: %w", err)
		}
	}

	if !httpActive && !tcpActive {
		setCondition(group, seiv1alpha1.ConditionNetworkingReady, metav1.ConditionFalse,
			"NetworkingDisabled", "spec.networking has no active tier (no HTTP, and TCP requires SEI_P2P_ENDPOINT_DOMAIN)")
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
			Ports:    externalPortsForMode(groupMode(group)),
		},
	}
}

// externalPortsForMode returns the public-facing port set — portsForMode
// minus the `metrics` port, which is a private scrape endpoint. The
// platform-owned PodMonitor scrapes pods directly via the pod IP, so
// the metrics port has no reason to surface on the public LB.
//
// TODO(sei-protocol/sei-config#7): replace with seiconfig.ExternalServicePorts
// once that helper lands upstream. Keeping the filter local to the controller
// is a workaround; the "which ports belong on a public Service" rule should
// live next to the port definitions in sei-config.
func externalPortsForMode(mode seiconfig.NodeMode) []corev1.ServicePort {
	all := portsForMode(mode)
	out := make([]corev1.ServicePort, 0, len(all))
	for _, p := range all {
		if p.Name == "metrics" {
			continue
		}
		out = append(out, p)
	}
	return out
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
		setCondition(group, seiv1alpha1.ConditionNetworkingReady, metav1.ConditionFalse,
			"NoEffectiveRoutes", "node mode declares no externally-routable protocols")
		return r.deleteHTTPRoutesByLabel(ctx, group)
	}
	return r.reconcileHTTPRoutes(ctx, group, routes)
}

func (r *SeiNodeDeploymentReconciler) deleteHTTPRoutesByLabel(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	list := &gatewayv1.HTTPRouteList{}
	err := r.List(ctx, list, client.InNamespace(group.Namespace), client.MatchingLabels(resourceLabels(group)))
	if meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing HTTPRoutes for deletion: %w", err)
	}
	for i := range list.Items {
		// Skip what we no longer own: orphaned to GitOps.
		if !metav1.IsControlledBy(&list.Items[i], group) {
			continue
		}
		if err := r.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting HTTPRoute %s: %w", list.Items[i].Name, err)
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

		//nolint:staticcheck // typed ApplyConfiguration migration is a separate effort
		err := r.Patch(ctx, desired, client.Apply, fieldOwner, client.ForceOwnership)
		if meta.IsNoMatchError(err) {
			if !hasConditionReason(group, seiv1alpha1.ConditionNetworkingReady, "CRDNotInstalled") {
				r.Recorder.Event(group, corev1.EventTypeWarning, "CRDNotInstalled", "Gateway API CRD (HTTPRoute) is not installed; HTTPRoute will not be created")
			}
			setCondition(group, seiv1alpha1.ConditionNetworkingReady, metav1.ConditionFalse,
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

	if !hasConditionReason(group, seiv1alpha1.ConditionNetworkingReady, "HTTPRoutesPublished") {
		r.Recorder.Event(group, corev1.EventTypeNormal, "HTTPRoutesPublished", "HTTPRoutes reconciled successfully")
	}
	setCondition(group, seiv1alpha1.ConditionNetworkingReady, metav1.ConditionTrue,
		"HTTPRoutesPublished", fmt.Sprintf("%d HTTPRoute(s) reconciled successfully", len(routes)))
	return nil
}

func (r *SeiNodeDeploymentReconciler) deleteOrphanedHTTPRoutes(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment, desiredNames map[string]bool) error {
	list := &gatewayv1.HTTPRouteList{}
	err := r.List(ctx, list, client.InNamespace(group.Namespace), client.MatchingLabels(resourceLabels(group)))
	if meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing HTTPRoutes: %w", err)
	}
	for i := range list.Items {
		route := &list.Items[i]
		if !desiredNames[route.Name] {
			if err := r.Delete(ctx, route); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting orphaned HTTPRoute %s: %w", route.Name, err)
			}
		}
	}
	return nil
}

func generateHTTPRoute(group *seiv1alpha1.SeiNodeDeployment, er effectiveRoute, gatewayName, gatewayNamespace string) *gatewayv1.HTTPRoute {
	svcName := externalServiceName(group)

	hostnames := make([]gatewayv1.Hostname, len(er.Hostnames))
	for i, h := range er.Hostnames {
		hostnames[i] = gatewayv1.Hostname(h)
	}

	ns := gatewayv1.Namespace(gatewayNamespace)
	parentRef := gatewayv1.ParentReference{
		Name:      gatewayv1.ObjectName(gatewayName),
		Namespace: &ns,
	}

	backendForPort := func(port int32) gatewayv1.HTTPBackendRef {
		p := gatewayv1.PortNumber(port) //nolint:unconvert // distinct named type over int32
		return gatewayv1.HTTPBackendRef{
			BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{
					Name: gatewayv1.ObjectName(svcName),
					Port: &p,
				},
			},
		}
	}

	rules := []gatewayv1.HTTPRouteRule{
		{BackendRefs: []gatewayv1.HTTPBackendRef{backendForPort(er.Port)}},
	}
	if er.WSPort != 0 {
		matchType := gatewayv1.HeaderMatchExact
		rules = append(rules, gatewayv1.HTTPRouteRule{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Headers: []gatewayv1.HTTPHeaderMatch{{
					Type:  &matchType,
					Name:  "Upgrade",
					Value: "websocket",
				}},
			}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backendForPort(er.WSPort)},
		})
	}

	return &gatewayv1.HTTPRoute{
		TypeMeta: metav1.TypeMeta{
			APIVersion: gatewayv1.GroupVersion.String(),
			Kind:       "HTTPRoute",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        er.Name,
			Namespace:   group.Namespace,
			Labels:      resourceLabels(group),
			Annotations: managedByAnnotations(),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{parentRef},
			},
			Hostnames: hostnames,
			Rules:     rules,
		},
	}
}

// --- Deletion helpers ---

func (r *SeiNodeDeploymentReconciler) deleteExternalService(ctx context.Context, group *seiv1alpha1.SeiNodeDeployment) error {
	svc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: externalServiceName(group), Namespace: group.Namespace}, svc)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fetching external Service for deletion: %w", err)
	}
	// Skip what we no longer own: orphaned to GitOps.
	if !metav1.IsControlledBy(svc, group) {
		return nil
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
	if err := r.deleteP2PEndpoints(ctx, group); err != nil {
		return fmt.Errorf("deleting P2P endpoint Services: %w", err)
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

	list := &gatewayv1.HTTPRouteList{}
	listErr := r.List(ctx, list, client.InNamespace(group.Namespace), client.MatchingLabels(resourceLabels(group)))
	if listErr != nil && !meta.IsNoMatchError(listErr) {
		return fmt.Errorf("listing HTTPRoutes for orphan: %w", listErr)
	}
	if listErr == nil {
		for i := range list.Items {
			if err := r.removeOwnerRef(ctx, &list.Items[i], group); err != nil {
				return fmt.Errorf("orphaning HTTPRoute %s: %w", list.Items[i].Name, err)
			}
		}
	}

	if err := r.orphanP2PEndpoints(ctx, group); err != nil {
		return err
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
