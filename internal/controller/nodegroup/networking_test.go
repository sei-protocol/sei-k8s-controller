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

	spec := route.Object["spec"].(map[string]any)
	parentRefs := spec["parentRefs"].([]any)
	g.Expect(parentRefs).To(HaveLen(1))

	ref := parentRefs[0].(map[string]any)
	g.Expect(ref["name"]).To(Equal("istio-gateway"))
	g.Expect(ref["namespace"]).To(Equal("istio-system"))

	hostnames := spec["hostnames"].([]any)
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

	spec := route.Object["spec"].(map[string]any)
	parentRefs := spec["parentRefs"].([]any)
	ref := parentRefs[0].(map[string]any)
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

	spec := route.Object["spec"].(map[string]any)
	parentRefs := spec["parentRefs"].([]any)
	ref := parentRefs[0].(map[string]any)
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

	spec := route.Object["spec"].(map[string]any)
	rules := spec["rules"].([]any)
	g.Expect(rules).To(HaveLen(1))

	rule := rules[0].(map[string]any)
	backends := rule["backendRefs"].([]any)
	g.Expect(backends).To(HaveLen(1))

	backend := backends[0].(map[string]any)
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
