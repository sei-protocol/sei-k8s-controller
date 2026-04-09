package nodedeployment

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// --- External Service ---

func TestGenerateExternalService_BasicFields(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	svc := generateExternalService(group)

	g.Expect(svc.Name).To(Equal("archive-rpc-external"))
	g.Expect(svc.Namespace).To(Equal("sei"))
	g.Expect(svc.Labels).To(HaveKeyWithValue(groupLabel, "archive-rpc"))
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(groupLabel, "archive-rpc"))
}

func TestGenerateExternalService_AllPortsWhenEmpty(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
	}

	svc := generateExternalService(group)
	g.Expect(svc.Spec.Ports).To(HaveLen(7))
}

func TestGenerateExternalService_ValidatorModePorts(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-val", "sei")
	group.Spec.Template.Spec.Validator = &seiv1alpha1.ValidatorSpec{}
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
	}

	svc := generateExternalService(group)
	g.Expect(svc.Spec.Ports).To(HaveLen(2))

	portNames := make([]string, len(svc.Spec.Ports))
	for i, p := range svc.Spec.Ports {
		portNames[i] = p.Name
	}
	g.Expect(portNames).To(ConsistOf("p2p", "metrics"))
}

func TestGenerateExternalService_Annotations(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{
			Annotations: map[string]string{
				"service.beta.kubernetes.io/aws-load-balancer-type": "nlb",
			},
		},
	}

	svc := generateExternalService(group)
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-type", "nlb"))
}

func TestGenerateExternalService_LoadBalancer(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{
			Type: corev1.ServiceTypeLoadBalancer,
		},
	}

	svc := generateExternalService(group)
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
}

func TestGenerateExternalService_NoPublishNotReady(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
	}

	svc := generateExternalService(group)
	g.Expect(svc.Spec.PublishNotReadyAddresses).To(BeFalse())
}

func TestGenerateExternalService_FullModeIncludesAllPorts(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
	}

	svc := generateExternalService(group)
	portNames := make([]string, len(svc.Spec.Ports))
	for i, p := range svc.Spec.Ports {
		portNames[i] = p.Name
	}
	g.Expect(portNames).To(ConsistOf("evm-rpc", "evm-ws", "grpc", "rest", "p2p", "rpc", "metrics"))
}

// --- Effective Routes ---

func TestResolveEffectiveRoutes_FullMode_FourRoutes(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{},
	}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io")
	g.Expect(routes).To(HaveLen(4))

	type routeExpectation struct {
		Name     string
		Hostname string
		Port     int32
	}
	expected := []routeExpectation{
		{"pacific-1-rpc-evm", "pacific-1-rpc.evm.prod.platform.sei.io", 8545},
		{"pacific-1-rpc-rpc", "pacific-1-rpc.rpc.prod.platform.sei.io", 26657},
		{"pacific-1-rpc-rest", "pacific-1-rpc.rest.prod.platform.sei.io", 1317},
		{"pacific-1-rpc-grpc", "pacific-1-rpc.grpc.prod.platform.sei.io", 9090},
	}
	for i, exp := range expected {
		g.Expect(routes[i].Name).To(Equal(exp.Name))
		g.Expect(routes[i].Hostnames).To(Equal([]string{exp.Hostname}))
		g.Expect(routes[i].Port).To(Equal(exp.Port))
	}
}

func TestResolveEffectiveRoutes_ArchiveMode_FourRoutes(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-archive", "sei")
	group.Spec.Template.Spec.FullNode = nil
	group.Spec.Template.Spec.Archive = &seiv1alpha1.ArchiveSpec{}
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{},
	}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io")
	g.Expect(routes).To(HaveLen(4))
}

func TestResolveEffectiveRoutes_ValidatorMode_NoRoutes(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-val", "sei")
	group.Spec.Template.Spec.FullNode = nil
	group.Spec.Template.Spec.Validator = &seiv1alpha1.ValidatorSpec{}
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{},
	}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io")
	g.Expect(routes).To(BeEmpty())
}

func TestGenerateHTTPRoute_HostnamePattern(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{},
	}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io")
	g.Expect(routes).NotTo(BeEmpty())

	for _, er := range routes {
		route := generateHTTPRoute(group, er, "sei-gateway", "istio-system", "")
		spec := route.Object["spec"].(map[string]any)
		hostnames := spec["hostnames"].([]any)
		g.Expect(hostnames).To(HaveLen(1))
		g.Expect(hostnames[0]).To(MatchRegexp(`^pacific-1-rpc\.\w+\.prod\.platform\.sei\.io$`))
	}
}

func TestGenerateHTTPRoute_EVMMerged(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{},
	}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io")

	evmCount := 0
	for _, r := range routes {
		if r.Name == "pacific-1-rpc-evm" {
			evmCount++
			g.Expect(r.Port).To(Equal(int32(8545)))
		}
	}
	g.Expect(evmCount).To(Equal(1), "expected exactly one merged EVM route")

	for _, r := range routes {
		g.Expect(r.Name).NotTo(ContainSubstring("evm-rpc"))
		g.Expect(r.Name).NotTo(ContainSubstring("evm-ws"))
	}
}

// --- HTTPRoute Generation ---

func TestGenerateHTTPRoute_BasicFields(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{},
	}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io")
	g.Expect(routes).NotTo(BeEmpty())
	route := generateHTTPRoute(group, routes[0], "sei-gateway", "istio-system", "")

	g.Expect(route.GetNamespace()).To(Equal("sei"))

	spec := route.Object["spec"].(map[string]any)
	parentRefs := spec["parentRefs"].([]any)
	g.Expect(parentRefs).To(HaveLen(1))

	ref := parentRefs[0].(map[string]any)
	g.Expect(ref["name"]).To(Equal("sei-gateway"))
	g.Expect(ref["namespace"]).To(Equal("istio-system"))
}

func TestGenerateHTTPRoute_ManagedByAnnotation(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{},
	}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io")
	route := generateHTTPRoute(group, routes[0], "sei-gateway", "istio-system", "")
	g.Expect(route.GetAnnotations()).To(HaveKeyWithValue("sei.io/managed-by", "seinodedeployment"))
}

func TestGenerateHTTPRoute_BackendRef(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{},
	}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io")
	route := generateHTTPRoute(group, routes[0], "sei-gateway", "istio-system", "")

	spec := route.Object["spec"].(map[string]any)
	rules := spec["rules"].([]any)
	g.Expect(rules).To(HaveLen(1))

	rule := rules[0].(map[string]any)
	backends := rule["backendRefs"].([]any)
	g.Expect(backends).To(HaveLen(1))

	backend := backends[0].(map[string]any)
	g.Expect(backend["name"]).To(Equal("archive-rpc-external"))
}

func TestGenerateHTTPRoute_GRPCRoutePort(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{},
	}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io")

	var grpcRoute effectiveRoute
	for _, r := range routes {
		if r.Name == "pacific-1-rpc-grpc" {
			grpcRoute = r
			break
		}
	}
	g.Expect(grpcRoute.Name).NotTo(BeEmpty(), "grpc route should exist")

	httpRoute := generateHTTPRoute(group, grpcRoute, "sei-gateway", "istio-system", "")
	spec := httpRoute.Object["spec"].(map[string]any)
	hostnames := spec["hostnames"].([]any)
	g.Expect(hostnames).To(ConsistOf("pacific-1-rpc.grpc.prod.platform.sei.io"))

	rules := spec["rules"].([]any)
	backend := rules[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)
	g.Expect(backend["port"]).To(Equal(int64(9090)))
	g.Expect(backend["name"]).To(Equal("pacific-1-rpc-external"))
}

// --- isProtocolActiveForMode ---

func TestIsProtocolActiveForMode_EVMMapping(t *testing.T) {
	g := NewWithT(t)
	activePorts := map[string]bool{"evm-rpc": true, "evm-ws": true, "rpc": true}

	g.Expect(isProtocolActiveForMode("evm", activePorts)).To(BeTrue())
	g.Expect(isProtocolActiveForMode("rpc", activePorts)).To(BeTrue())
	g.Expect(isProtocolActiveForMode("grpc", activePorts)).To(BeFalse())
}

// --- Gateway sectionName ---

func TestGenerateHTTPRoute_SectionName_IncludedWhenSet(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
	}
	routes := resolveEffectiveRoutes(group, "test.platform.sei.io")
	route := generateHTTPRoute(group, routes[0], "sei-gateway", "gateway", "https")

	spec := route.Object["spec"].(map[string]any)
	parentRefs := spec["parentRefs"].([]any)
	ref := parentRefs[0].(map[string]any)
	g.Expect(ref["sectionName"]).To(Equal("https"))
}

func TestGenerateHTTPRoute_SectionName_OmittedWhenEmpty(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
	}
	routes := resolveEffectiveRoutes(group, "test.platform.sei.io")
	route := generateHTTPRoute(group, routes[0], "sei-gateway", "gateway", "")

	spec := route.Object["spec"].(map[string]any)
	parentRefs := spec["parentRefs"].([]any)
	ref := parentRefs[0].(map[string]any)
	_, hasSectionName := ref["sectionName"]
	g.Expect(hasSectionName).To(BeFalse())
}

// --- AuthorizationPolicy ---

func TestGenerateAuthorizationPolicy_BasicStructure(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Isolation: &seiv1alpha1.NetworkIsolationConfig{
			AuthorizationPolicy: &seiv1alpha1.AuthorizationPolicyConfig{
				AllowedSources: []seiv1alpha1.TrafficSource{{
					Principals: []string{"cluster.local/ns/istio-system/sa/istio-ingressgateway"},
				}},
			},
		},
	}

	policy := generateAuthorizationPolicy(group, "")

	g.Expect(policy.GetName()).To(Equal("archive-rpc"))
	g.Expect(policy.GetNamespace()).To(Equal("sei"))

	spec := policy.Object["spec"].(map[string]any)
	g.Expect(spec["action"]).To(Equal("ALLOW"))

	selector := spec["selector"].(map[string]any)
	matchLabels := selector["matchLabels"].(map[string]any)
	g.Expect(matchLabels[groupLabel]).To(Equal("archive-rpc"))
}

func TestGenerateAuthorizationPolicy_InjectsControllerSA(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Isolation: &seiv1alpha1.NetworkIsolationConfig{
			AuthorizationPolicy: &seiv1alpha1.AuthorizationPolicyConfig{
				AllowedSources: []seiv1alpha1.TrafficSource{{
					Principals: []string{"cluster.local/ns/istio-system/sa/istio-ingressgateway"},
				}},
			},
		},
	}

	controllerSA := "cluster.local/ns/sei-system/sa/sei-controller"
	policy := generateAuthorizationPolicy(group, controllerSA)

	spec := policy.Object["spec"].(map[string]any)
	rules := spec["rules"].([]any)
	g.Expect(rules).To(HaveLen(2), "should have user source + injected controller SA")

	lastRule := rules[1].(map[string]any)
	from := lastRule["from"].([]any)
	source := from[0].(map[string]any)["source"].(map[string]any)
	principals := source["principals"].([]any)
	g.Expect(principals).To(ConsistOf(controllerSA))
}

func TestGenerateAuthorizationPolicy_NoControllerSA(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Isolation: &seiv1alpha1.NetworkIsolationConfig{
			AuthorizationPolicy: &seiv1alpha1.AuthorizationPolicyConfig{
				AllowedSources: []seiv1alpha1.TrafficSource{{
					Principals: []string{"cluster.local/ns/istio-system/sa/istio-ingressgateway"},
				}},
			},
		},
	}

	policy := generateAuthorizationPolicy(group, "")

	spec := policy.Object["spec"].(map[string]any)
	rules := spec["rules"].([]any)
	g.Expect(rules).To(HaveLen(1), "only user sources when controller SA is empty")
}

func TestGenerateAuthorizationPolicy_NamespaceSource(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Isolation: &seiv1alpha1.NetworkIsolationConfig{
			AuthorizationPolicy: &seiv1alpha1.AuthorizationPolicyConfig{
				AllowedSources: []seiv1alpha1.TrafficSource{{
					Namespaces: []string{"monitoring", "sei-system"},
				}},
			},
		},
	}

	policy := generateAuthorizationPolicy(group, "")

	spec := policy.Object["spec"].(map[string]any)
	rules := spec["rules"].([]any)
	g.Expect(rules).To(HaveLen(1))

	rule := rules[0].(map[string]any)
	from := rule["from"].([]any)
	source := from[0].(map[string]any)["source"].(map[string]any)
	namespaces := source["namespaces"].([]any)
	g.Expect(namespaces).To(ConsistOf("monitoring", "sei-system"))
}
