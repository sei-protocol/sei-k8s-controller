package nodedeployment

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestRouteHostnameResolvable_NoRoutes(t *testing.T) {
	g := NewWithT(t)
	r := &SeiNodeDeploymentReconciler{GatewayDomain: "prod.platform.sei.io"}

	group := newTestGroup("pacific-1-val", "pacific-1")
	group.Spec.Template.Spec.FullNode = nil
	group.Spec.Template.Spec.Validator = &seiv1alpha1.ValidatorSpec{}
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	g.Expect(r.routeHostnameResolvable(context.Background(), group)).To(BeTrue(),
		"validator mode has no routes, should be immediately resolvable")
}

func TestRouteHostnameResolvable_NilNetworking(t *testing.T) {
	g := NewWithT(t)
	r := &SeiNodeDeploymentReconciler{GatewayDomain: "prod.platform.sei.io"}

	group := newTestGroup("pacific-1-wave", "pacific-1")

	g.Expect(r.routeHostnameResolvable(context.Background(), group)).To(BeTrue(),
		"no networking means private, should be immediately resolvable")
}
