package nodegroup

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
	g.Expect(svc.Spec.Ports).To(HaveLen(6))
}

func TestGenerateExternalService_FilteredPorts(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{
			Ports: []seiv1alpha1.PortName{"rpc", "evm-rpc"},
		},
	}

	svc := generateExternalService(group)
	g.Expect(svc.Spec.Ports).To(HaveLen(2))

	portNames := make([]string, len(svc.Spec.Ports))
	for i, p := range svc.Spec.Ports {
		portNames[i] = p.Name
	}
	g.Expect(portNames).To(ConsistOf("rpc", "evm-rpc"))
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

// --- Ingress ---

func TestGenerateIngress_BasicFields(t *testing.T) {
	g := NewWithT(t)
	className := "alb"
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Ingress: &seiv1alpha1.IngressConfig{
			ClassName: &className,
			Host:      "rpc.pacific-1.sei.io",
		},
	}

	ing := generateIngress(group)

	g.Expect(ing.Name).To(Equal("archive-rpc"))
	g.Expect(ing.Namespace).To(Equal("sei"))
	g.Expect(ing.Labels).To(HaveKeyWithValue(groupLabel, "archive-rpc"))
	g.Expect(*ing.Spec.IngressClassName).To(Equal("alb"))
	g.Expect(ing.Spec.Rules).To(HaveLen(1))
	g.Expect(ing.Spec.Rules[0].Host).To(Equal("rpc.pacific-1.sei.io"))
}

func TestGenerateIngress_DefaultPath(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Ingress: &seiv1alpha1.IngressConfig{
			Host: "rpc.pacific-1.sei.io",
		},
	}

	ing := generateIngress(group)

	paths := ing.Spec.Rules[0].HTTP.Paths
	g.Expect(paths).To(HaveLen(1))
	g.Expect(paths[0].Path).To(Equal("/"))
	g.Expect(paths[0].Backend.Service.Name).To(Equal("archive-rpc-external"))
	g.Expect(paths[0].Backend.Service.Port.Name).To(Equal("rpc"))
}

func TestGenerateIngress_TLS(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	secretName := "tls-cert"
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Ingress: &seiv1alpha1.IngressConfig{
			Host: "rpc.pacific-1.sei.io",
			TLS: &seiv1alpha1.IngressTLS{
				Enabled:    true,
				SecretName: &secretName,
			},
		},
	}

	ing := generateIngress(group)

	g.Expect(ing.Spec.TLS).To(HaveLen(1))
	g.Expect(ing.Spec.TLS[0].Hosts).To(ConsistOf("rpc.pacific-1.sei.io"))
	g.Expect(ing.Spec.TLS[0].SecretName).To(Equal("tls-cert"))
}

func TestGenerateIngress_CustomPaths(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Ingress: &seiv1alpha1.IngressConfig{
			Host: "rpc.pacific-1.sei.io",
			Paths: []seiv1alpha1.IngressPath{
				{Path: "/rpc", PortName: "rpc"},
				{Path: "/evm", PortName: "evm-rpc"},
			},
		},
	}

	ing := generateIngress(group)
	paths := ing.Spec.Rules[0].HTTP.Paths
	g.Expect(paths).To(HaveLen(2))
	g.Expect(paths[0].Path).To(Equal("/rpc"))
	g.Expect(paths[0].Backend.Service.Port.Name).To(Equal("rpc"))
	g.Expect(paths[1].Path).To(Equal("/evm"))
	g.Expect(paths[1].Backend.Service.Port.Name).To(Equal("evm-rpc"))
}

// --- HTTPRoute ---

func TestGenerateHTTPRoute_BasicFields(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{
			ParentRef: seiv1alpha1.GatewayParentRef{
				Name:      "istio-gateway",
				Namespace: "istio-system",
			},
			Hostnames: []string{"rpc.pacific-1.sei.io"},
		},
	}

	route := generateHTTPRoute(group)

	g.Expect(route.GetName()).To(Equal("archive-rpc"))
	g.Expect(route.GetNamespace()).To(Equal("sei"))

	spec := route.Object["spec"].(map[string]interface{})
	parentRefs := spec["parentRefs"].([]interface{})
	g.Expect(parentRefs).To(HaveLen(1))

	ref := parentRefs[0].(map[string]interface{})
	g.Expect(ref["name"]).To(Equal("istio-gateway"))
	g.Expect(ref["namespace"]).To(Equal("istio-system"))

	hostnames := spec["hostnames"].([]interface{})
	g.Expect(hostnames).To(ConsistOf("rpc.pacific-1.sei.io"))
}

func TestGenerateHTTPRoute_SectionName(t *testing.T) {
	g := NewWithT(t)
	section := "https"
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{
			ParentRef: seiv1alpha1.GatewayParentRef{
				Name:        "sei-gateway",
				Namespace:   "istio-system",
				SectionName: &section,
			},
			Hostnames: []string{"rpc.pacific-1.sei.io"},
		},
	}

	route := generateHTTPRoute(group)

	spec := route.Object["spec"].(map[string]interface{})
	parentRefs := spec["parentRefs"].([]interface{})
	ref := parentRefs[0].(map[string]interface{})
	g.Expect(ref["sectionName"]).To(Equal("https"))
}

func TestGenerateHTTPRoute_NoSectionName(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{
			ParentRef: seiv1alpha1.GatewayParentRef{
				Name:      "sei-gateway",
				Namespace: "istio-system",
			},
			Hostnames: []string{"rpc.pacific-1.sei.io"},
		},
	}

	route := generateHTTPRoute(group)

	spec := route.Object["spec"].(map[string]interface{})
	parentRefs := spec["parentRefs"].([]interface{})
	ref := parentRefs[0].(map[string]interface{})
	_, hasSectionName := ref["sectionName"]
	g.Expect(hasSectionName).To(BeFalse())
}

func TestGenerateHTTPRoute_ManagedByAnnotation(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{
			ParentRef: seiv1alpha1.GatewayParentRef{
				Name:      "gw",
				Namespace: "istio-system",
			},
			Hostnames: []string{"rpc.sei.io"},
		},
	}

	route := generateHTTPRoute(group)
	g.Expect(route.GetAnnotations()).To(HaveKeyWithValue("sei.io/managed-by", "seinodegroup"))
}

func TestGenerateHTTPRoute_BackendRef(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("archive-rpc", "sei")
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{
		Service: &seiv1alpha1.ExternalServiceConfig{},
		Gateway: &seiv1alpha1.GatewayRouteConfig{
			ParentRef: seiv1alpha1.GatewayParentRef{
				Name:      "gw",
				Namespace: "istio-system",
			},
			Hostnames: []string{"rpc.sei.io"},
		},
	}

	route := generateHTTPRoute(group)

	spec := route.Object["spec"].(map[string]interface{})
	rules := spec["rules"].([]interface{})
	g.Expect(rules).To(HaveLen(1))

	rule := rules[0].(map[string]interface{})
	backends := rule["backendRefs"].([]interface{})
	g.Expect(backends).To(HaveLen(1))

	backend := backends[0].(map[string]interface{})
	g.Expect(backend["name"]).To(Equal("archive-rpc-external"))
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

	spec := policy.Object["spec"].(map[string]interface{})
	g.Expect(spec["action"]).To(Equal("ALLOW"))

	selector := spec["selector"].(map[string]interface{})
	matchLabels := selector["matchLabels"].(map[string]interface{})
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

	spec := policy.Object["spec"].(map[string]interface{})
	rules := spec["rules"].([]interface{})
	g.Expect(rules).To(HaveLen(2), "should have user source + injected controller SA")

	lastRule := rules[1].(map[string]interface{})
	from := lastRule["from"].([]interface{})
	source := from[0].(map[string]interface{})["source"].(map[string]interface{})
	principals := source["principals"].([]interface{})
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

	spec := policy.Object["spec"].(map[string]interface{})
	rules := spec["rules"].([]interface{})
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

	spec := policy.Object["spec"].(map[string]interface{})
	rules := spec["rules"].([]interface{})
	g.Expect(rules).To(HaveLen(1))

	rule := rules[0].(map[string]interface{})
	from := rule["from"].([]interface{})
	source := from[0].(map[string]interface{})["source"].(map[string]interface{})
	namespaces := source["namespaces"].([]interface{})
	g.Expect(namespaces).To(ConsistOf("monitoring", "sei-system"))
}
