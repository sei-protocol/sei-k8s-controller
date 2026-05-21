package nodedeployment

import (
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

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
	g.Expect(svc.Spec.Ports).To(HaveLen(6))

	portNames := make([]string, len(svc.Spec.Ports))
	for i, p := range svc.Spec.Ports {
		portNames[i] = p.Name
	}
	g.Expect(portNames).To(ConsistOf("evm-rpc", "evm-ws", "grpc", "rest", "p2p", "rpc"))
	g.Expect(portNames).NotTo(ContainElement("metrics"))
}

func TestGenerateExternalService_ValidatorModePorts(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-val", "pacific-1")
	group.Spec.Template.Spec.Validator = &seiv1alpha1.ValidatorSpec{}
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	svc := generateExternalService(group)
	g.Expect(svc.Spec.Ports).To(HaveLen(1))

	portNames := make([]string, len(svc.Spec.Ports))
	for i, p := range svc.Spec.Ports {
		portNames[i] = p.Name
	}
	g.Expect(portNames).To(ConsistOf("p2p"))
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
		g.Expect(route.Spec.Hostnames).To(HaveLen(2))
		g.Expect(string(route.Spec.Hostnames[0])).To(MatchRegexp(`^pacific-1-wave-\w+\.prod\.platform\.sei\.io$`))
		g.Expect(string(route.Spec.Hostnames[1])).To(MatchRegexp(`^pacific-1-wave-\w+\.pacific-1\.platform\.sei\.io$`))
	}
}

func TestGenerateHTTPRoute_BasicFields(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	g.Expect(routes).NotTo(BeEmpty())
	route := generateHTTPRoute(group, routes[0], "sei-gateway", "gateway")

	g.Expect(route.Namespace).To(Equal("pacific-1"))
	g.Expect(route.Spec.ParentRefs).To(HaveLen(1))

	ref := route.Spec.ParentRefs[0]
	g.Expect(string(ref.Name)).To(Equal("sei-gateway"))
	g.Expect(ref.Namespace).NotTo(BeNil())
	g.Expect(string(*ref.Namespace)).To(Equal("gateway"))
}

func TestGenerateHTTPRoute_ManagedByAnnotation(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	route := generateHTTPRoute(group, routes[0], "sei-gateway", "gateway")
	g.Expect(route.Annotations).To(HaveKeyWithValue("sei.io/managed-by", "seinodedeployment"))
}

// TestGenerateHTTPRoute_TypeMeta locks down the apiVersion/kind on the
// constructed object. The typed builder must populate TypeMeta so the SSA
// apply path serializes the same top-level keys the unstructured builder
// did pre-#316; an empty TypeMeta would silently break server-side apply.
func TestGenerateHTTPRoute_TypeMeta(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	route := generateHTTPRoute(group, routes[0], "sei-gateway", "gateway")

	g.Expect(route.APIVersion).To(Equal("gateway.networking.k8s.io/v1"))
	g.Expect(route.Kind).To(Equal("HTTPRoute"))
}

// TestGenerateHTTPRoute_BackendRefDefaultsServerSide is the load-bearing
// guard against accidental SSA field-ownership creep. The controller must
// leave BackendObjectReference.{Group,Kind} unset (nil) so the apiserver
// applies the documented defaults (core/Service) on its side. Setting
// them explicitly would cause field-manager "seinode-controller" to claim
// those paths, fighting any future operator who tries to retarget a
// backend via kubectl edit.
func TestGenerateHTTPRoute_BackendRefDefaultsServerSide(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	g.Expect(routes).NotTo(BeEmpty())

	for _, er := range routes {
		route := generateHTTPRoute(group, er, "sei-gateway", "gateway")
		g.Expect(route.Spec.Rules).NotTo(BeEmpty())
		for ri, rule := range route.Spec.Rules {
			for bi, b := range rule.BackendRefs {
				g.Expect(b.Group).To(BeNil(),
					"route %s rule[%d] backend[%d]: BackendRef.Group must be nil (server-defaulted to core)",
					er.Name, ri, bi)
				g.Expect(b.Kind).To(BeNil(),
					"route %s rule[%d] backend[%d]: BackendRef.Kind must be nil (server-defaulted to Service)",
					er.Name, ri, bi)
			}
		}
	}
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

	g.Expect(route.Spec.Rules).To(HaveLen(1))
	g.Expect(route.Spec.Rules[0].BackendRefs).To(HaveLen(1))
	g.Expect(string(route.Spec.Rules[0].BackendRefs[0].Name)).To(Equal("pacific-1-wave-external"))
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
	g.Expect(httpRoute.Spec.Hostnames).To(ConsistOf(
		gatewayv1.Hostname("pacific-1-wave-grpc.prod.platform.sei.io"),
		gatewayv1.Hostname("pacific-1-wave-grpc.pacific-1.platform.sei.io"),
	))

	g.Expect(httpRoute.Spec.Rules).To(HaveLen(1))
	backend := httpRoute.Spec.Rules[0].BackendRefs[0]
	g.Expect(backend.Port).NotTo(BeNil())
	g.Expect(*backend.Port).To(Equal(gatewayv1.PortNumber(9090)))
	g.Expect(string(backend.Name)).To(Equal("pacific-1-wave-external"))
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
	g.Expect(httpRoute.Spec.Rules).To(HaveLen(2))

	httpBackend := httpRoute.Spec.Rules[0].BackendRefs[0]
	g.Expect(httpBackend.Port).NotTo(BeNil())
	g.Expect(*httpBackend.Port).To(Equal(gatewayv1.PortNumber(8545)))

	wsRule := httpRoute.Spec.Rules[1]
	g.Expect(wsRule.Matches).To(HaveLen(1))
	g.Expect(wsRule.Matches[0].Headers).To(HaveLen(1))
	wsHeader := wsRule.Matches[0].Headers[0]
	g.Expect(string(wsHeader.Name)).To(Equal("Upgrade"))
	g.Expect(wsHeader.Value).To(Equal("websocket"))
	g.Expect(wsHeader.Type).NotTo(BeNil())
	g.Expect(*wsHeader.Type).To(Equal(gatewayv1.HeaderMatchExact))

	wsBackend := wsRule.BackendRefs[0]
	g.Expect(wsBackend.Port).NotTo(BeNil())
	g.Expect(*wsBackend.Port).To(Equal(gatewayv1.PortNumber(8546)))
}

func TestGenerateHTTPRoute_NonEVMRoute_SingleRule(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	routes := resolveEffectiveRoutes(group, "prod.platform.sei.io", "platform.sei.io")
	for _, r := range routes {
		if r.Name == "pacific-1-wave-rpc" {
			httpRoute := generateHTTPRoute(group, r, "sei-gateway", "gateway")
			g.Expect(httpRoute.Spec.Rules).To(HaveLen(1))
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
