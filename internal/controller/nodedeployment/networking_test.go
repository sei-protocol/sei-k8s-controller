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
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	svc := generateExternalService(group)

	g.Expect(svc.Name).To(Equal("pacific-1-wave-external"))
	g.Expect(svc.Namespace).To(Equal("pacific-1"))
	g.Expect(svc.Labels).To(HaveKeyWithValue(groupLabel, "pacific-1-wave"))
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(groupLabel, "pacific-1-wave"))
}

func TestGenerateExternalService_AllPortsForFullMode(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	svc := generateExternalService(group)
	g.Expect(svc.Spec.Ports).To(HaveLen(7))

	portNames := make([]string, len(svc.Spec.Ports))
	for i, p := range svc.Spec.Ports {
		portNames[i] = p.Name
	}
	g.Expect(portNames).To(ConsistOf("evm-rpc", "evm-ws", "grpc", "rest", "p2p", "rpc", "metrics"))
}

func TestGenerateExternalService_ValidatorModePorts(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-val", "pacific-1")
	group.Spec.Template.Spec.Validator = &seiv1alpha1.ValidatorSpec{}
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	svc := generateExternalService(group)
	g.Expect(svc.Spec.Ports).To(HaveLen(2))

	portNames := make([]string, len(svc.Spec.Ports))
	for i, p := range svc.Spec.Ports {
		portNames[i] = p.Name
	}
	g.Expect(portNames).To(ConsistOf("p2p", "metrics"))
}

func TestGenerateExternalService_GRPCAppProtocol(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	svc := generateExternalService(group)
	for _, p := range svc.Spec.Ports {
		if p.Name == "grpc" {
			g.Expect(p.AppProtocol).NotTo(BeNil())
			g.Expect(*p.AppProtocol).To(Equal("kubernetes.io/h2c"))
			return
		}
	}
	t.Fatal("grpc port not found")
}

func TestGenerateExternalService_AlwaysClusterIP(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	svc := generateExternalService(group)
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
	g.Expect(svc.Annotations).To(BeEmpty())
}

// --- Effective Routes ---

func TestResolveEffectiveRoutes_FullMode_FourRoutes(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	g.Expect(routes).To(HaveLen(4))

	type routeExpectation struct {
		Name      string
		Hostnames []string
		Port      int32
	}
	expected := []routeExpectation{
		{"pacific-1-wave-evm", []string{"pacific-1-wave-evm.prod.platform.sei.io", "pacific-1-wave-evm.pacific-1.platform.sei.io"}, 8545},
		{"pacific-1-wave-rpc", []string{"pacific-1-wave-rpc.prod.platform.sei.io", "pacific-1-wave-rpc.pacific-1.platform.sei.io"}, 26657},
		{"pacific-1-wave-rest", []string{"pacific-1-wave-rest.prod.platform.sei.io", "pacific-1-wave-rest.pacific-1.platform.sei.io"}, 1317},
		{"pacific-1-wave-grpc", []string{"pacific-1-wave-grpc.prod.platform.sei.io", "pacific-1-wave-grpc.pacific-1.platform.sei.io"}, 9090},
	}
	for i, exp := range expected {
		g.Expect(routes[i].Name).To(Equal(exp.Name))
		g.Expect(routes[i].Hostnames).To(Equal(exp.Hostnames))
		g.Expect(routes[i].Port).To(Equal(exp.Port))
	}
}

func TestResolveEffectiveRoutes_ArchiveMode_FourRoutes(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-archive", "pacific-1")
	group.Spec.Template.Spec.FullNode = nil
	group.Spec.Template.Spec.Archive = &seiv1alpha1.ArchiveSpec{}
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	g.Expect(routes).To(HaveLen(4))
}

func TestResolveEffectiveRoutes_ValidatorMode_NoRoutes(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-val", "pacific-1")
	group.Spec.Template.Spec.FullNode = nil
	group.Spec.Template.Spec.Validator = &seiv1alpha1.ValidatorSpec{}
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	g.Expect(routes).To(BeEmpty())
}

func TestResolveEffectiveRoutes_NoPublicDomain_SingleHostname(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "dev.platform.sei.io", "")
	g.Expect(routes).To(HaveLen(4))
	for _, r := range routes {
		g.Expect(r.Hostnames).To(HaveLen(1))
	}
	g.Expect(routes[0].Hostnames[0]).To(Equal("pacific-1-wave-evm.dev.platform.sei.io"))
}

// --- HTTPRoute Generation ---

func TestGenerateHTTPRoute_HostnamePattern(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	g.Expect(routes).NotTo(BeEmpty())

	for _, er := range routes {
		route := generateHTTPRoute(group, er, "sei-gateway", "istio-system")
		spec := route.Object["spec"].(map[string]any)
		hostnames := spec["hostnames"].([]any)
		g.Expect(hostnames).To(HaveLen(2))
		g.Expect(hostnames[0]).To(MatchRegexp(`^pacific-1-wave-\w+\.prod\.platform\.sei\.io$`))
		g.Expect(hostnames[1]).To(MatchRegexp(`^pacific-1-wave-\w+\.pacific-1\.platform\.sei\.io$`))
	}
}

func TestGenerateHTTPRoute_BasicFields(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	g.Expect(routes).NotTo(BeEmpty())
	route := generateHTTPRoute(group, routes[0], "sei-gateway", "gateway")

	g.Expect(route.GetNamespace()).To(Equal("pacific-1"))

	spec := route.Object["spec"].(map[string]any)
	parentRefs := spec["parentRefs"].([]any)
	g.Expect(parentRefs).To(HaveLen(1))

	ref := parentRefs[0].(map[string]any)
	g.Expect(ref["name"]).To(Equal("sei-gateway"))
	g.Expect(ref["namespace"]).To(Equal("gateway"))
}

func TestGenerateHTTPRoute_ManagedByAnnotation(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	route := generateHTTPRoute(group, routes[0], "sei-gateway", "gateway")
	g.Expect(route.GetAnnotations()).To(HaveKeyWithValue("sei.io/managed-by", "seinodedeployment"))
}

func TestGenerateHTTPRoute_BackendRef(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	var rpcRoute effectiveRoute
	for _, r := range routes {
		if r.Name == "pacific-1-wave-rpc" {
			rpcRoute = r
			break
		}
	}
	route := generateHTTPRoute(group, rpcRoute, "sei-gateway", "gateway")

	spec := route.Object["spec"].(map[string]any)
	rules := spec["rules"].([]any)
	g.Expect(rules).To(HaveLen(1))

	backend := rules[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)
	g.Expect(backend["name"]).To(Equal("pacific-1-wave-external"))
}

func TestGenerateHTTPRoute_GRPCRoutePort(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")

	var grpcRoute effectiveRoute
	for _, r := range routes {
		if r.Name == "pacific-1-wave-grpc" {
			grpcRoute = r
			break
		}
	}
	g.Expect(grpcRoute.Name).NotTo(BeEmpty())

	httpRoute := generateHTTPRoute(group, grpcRoute, "sei-gateway", "gateway")
	spec := httpRoute.Object["spec"].(map[string]any)
	hostnames := spec["hostnames"].([]any)
	g.Expect(hostnames).To(ConsistOf("pacific-1-wave-grpc.prod.platform.sei.io", "pacific-1-wave-grpc.pacific-1.platform.sei.io"))

	rules := spec["rules"].([]any)
	backend := rules[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)
	g.Expect(backend["port"]).To(Equal(int64(9090)))
	g.Expect(backend["name"]).To(Equal("pacific-1-wave-external"))
}

func TestGenerateHTTPRoute_EVMMerged(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")

	var evmRoute effectiveRoute
	evmCount := 0
	for _, r := range routes {
		if r.Name == "pacific-1-wave-evm" {
			evmCount++
			evmRoute = r
		}
	}
	g.Expect(evmCount).To(Equal(1))
	g.Expect(evmRoute.Port).To(Equal(int32(8545)))
	g.Expect(evmRoute.WSPort).To(Equal(int32(8546)))
}

func TestGenerateHTTPRoute_EVMWebSocketRule(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	var evmRoute effectiveRoute
	for _, r := range routes {
		if r.Name == "pacific-1-wave-evm" {
			evmRoute = r
			break
		}
	}

	httpRoute := generateHTTPRoute(group, evmRoute, "sei-gateway", "gateway")
	spec := httpRoute.Object["spec"].(map[string]any)
	rules := spec["rules"].([]any)
	g.Expect(rules).To(HaveLen(2))

	httpBackend := rules[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)
	g.Expect(httpBackend["port"]).To(Equal(int64(8545)))

	wsRule := rules[1].(map[string]any)
	wsHeader := wsRule["matches"].([]any)[0].(map[string]any)["headers"].([]any)[0].(map[string]any)
	g.Expect(wsHeader["name"]).To(Equal("Upgrade"))
	g.Expect(wsHeader["value"]).To(Equal("websocket"))
	g.Expect(wsRule["backendRefs"].([]any)[0].(map[string]any)["port"]).To(Equal(int64(8546)))
}

func TestGenerateHTTPRoute_NonEVMRoute_SingleRule(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	for _, r := range routes {
		if r.Name == "pacific-1-wave-rpc" {
			httpRoute := generateHTTPRoute(group, r, "sei-gateway", "gateway")
			spec := httpRoute.Object["spec"].(map[string]any)
			rules := spec["rules"].([]any)
			g.Expect(rules).To(HaveLen(1))
			g.Expect(r.WSPort).To(Equal(int32(0)))
			return
		}
	}
	t.Fatal("rpc route not found")
}

// --- isProtocolActiveForMode ---

func TestIsProtocolActiveForMode_EVMMapping(t *testing.T) {
	g := NewWithT(t)
	activePorts := map[string]bool{"evm-rpc": true, "evm-ws": true, "rpc": true}

	g.Expect(isProtocolActiveForMode("evm", activePorts)).To(BeTrue())
	g.Expect(isProtocolActiveForMode("rpc", activePorts)).To(BeTrue())
	g.Expect(isProtocolActiveForMode("grpc", activePorts)).To(BeFalse())
}
