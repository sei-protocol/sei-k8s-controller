//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest/fixtures"
)

// suite_test.go wires the test controller with these gateway settings;
// the hostname / parentRef assertions below depend on them.
const (
	testGatewayName      = "sei-gateway"
	testGatewayNamespace = "gateway"
	testGatewayDomain    = "test.local"
)

// TestNetworking_FullNode_PublishesHTTPRoutes exercises the SND networking
// reconciler against the typed gateway-api client. A full-node SND with
// spec.networking set must:
//
//  1. Provision a ClusterIP external Service named "{snd}-external"
//  2. Publish exactly four HTTPRoutes — one per externally-routable
//     protocol (evm, rpc, rest, grpc)
//  3. Each HTTPRoute has the correct ParentRef (gateway/sei-gateway),
//     hostname pattern ("{snd}-{protocol}.test.local"), and BackendRef
//     pointing at the external Service.
//  4. The EVM route carries a second rule with a WebSocket header match,
//     routing to the EVM-WS port — the only protocol with multi-rule
//     shape.
func TestNetworking_FullNode_PublishesHTTPRoutes(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "fullnode-net",
		fixtures.WithReplicas(1),
		fixtures.WithNetworking(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())
	key := client.ObjectKeyFromObject(snd)
	_ = key // status assertions below re-fetch via getSND

	// 1. External Service appears.
	extSvcName := snd.Name + "-external"
	waitFor(t, func() bool {
		svc := &corev1.Service{}
		err := testCli.Get(testCtx, types.NamespacedName{Name: extSvcName, Namespace: ns}, svc)
		if err != nil {
			return false
		}
		return svc.Spec.Type == corev1.ServiceTypeClusterIP
	}, "external ClusterIP Service "+extSvcName+" created")

	// 2. Four HTTPRoutes appear, one per protocol.
	expectedProtocols := []string{"evm", "rpc", "rest", "grpc"}
	waitFor(t, func() bool {
		return len(listHTTPRoutes(t, snd)) == len(expectedProtocols)
	}, "4 HTTPRoutes published")

	routes := listHTTPRoutes(t, snd)
	g.Expect(routes).To(HaveLen(4))

	// Index by route name for per-protocol assertions.
	byName := make(map[string]gatewayv1.HTTPRoute, len(routes))
	for i := range routes {
		byName[routes[i].Name] = routes[i]
	}

	// 3. Per-route shape.
	for _, proto := range expectedProtocols {
		routeName := snd.Name + "-" + proto
		r, ok := byName[routeName]
		g.Expect(ok).To(BeTrue(), "HTTPRoute %s not found", routeName)

		g.Expect(r.APIVersion).To(Equal("gateway.networking.k8s.io/v1"), "route %s apiVersion", routeName)
		g.Expect(r.Kind).To(Equal("HTTPRoute"), "route %s kind", routeName)

		// ParentRef points at the test gateway. Group and Kind are
		// server-defaulted on read-back regardless of client-sent shape.
		g.Expect(r.Spec.ParentRefs).To(HaveLen(1), "route %s parentRefs len", routeName)
		pr := r.Spec.ParentRefs[0]
		g.Expect(string(pr.Name)).To(Equal(testGatewayName), "route %s parentRef.name", routeName)
		g.Expect(pr.Namespace).NotTo(BeNil(), "route %s parentRef.namespace pointer", routeName)
		g.Expect(string(*pr.Namespace)).To(Equal(testGatewayNamespace), "route %s parentRef.namespace", routeName)

		// Hostnames: single entry (suite's GatewayPublicDomain is "").
		g.Expect(r.Spec.Hostnames).To(HaveLen(1), "route %s hostnames len", routeName)
		g.Expect(string(r.Spec.Hostnames[0])).To(Equal(routeName+"."+testGatewayDomain), "route %s hostname", routeName)

		// Rules: every route has at least one BackendRef pointing at the
		// external Service. The EVM route has an additional rule for the
		// WebSocket upgrade match.
		g.Expect(r.Spec.Rules).NotTo(BeEmpty(), "route %s has at least one rule", routeName)
		for ri, rule := range r.Spec.Rules {
			g.Expect(rule.BackendRefs).NotTo(BeEmpty(), "route %s rule[%d] backendRefs", routeName, ri)
			for bi, b := range rule.BackendRefs {
				g.Expect(string(b.Name)).To(Equal(extSvcName), "route %s rule[%d] backend[%d].name", routeName, ri, bi)
				g.Expect(b.Port).NotTo(BeNil(), "route %s rule[%d] backend[%d].port pointer", routeName, ri, bi)
			}
		}

		// Owner reference points at the SND with controller=true so a
		// future GC controller (or our own finalizer path) reaps the
		// route on SND deletion.
		refs := r.GetOwnerReferences()
		g.Expect(refs).NotTo(BeEmpty(), "route %s has owner reference", routeName)
		g.Expect(refs[0].UID).To(Equal(snd.UID), "route %s owner UID", routeName)
		g.Expect(refs[0].Controller).NotTo(BeNil(), "route %s controller pointer", routeName)
		g.Expect(*refs[0].Controller).To(BeTrue(), "route %s controller=true", routeName)
	}

	// 4. EVM route specifically carries a second rule for the WebSocket
	//    upgrade-header match, routing to the EVM-WS port (8546). This
	//    is the only protocol with multi-rule shape and the most likely
	//    locus of a hand-coded regression.
	evmRoute := byName[snd.Name+"-evm"]
	g.Expect(evmRoute.Spec.Rules).To(HaveLen(2), "EVM route has HTTP + WebSocket rules")

	// The HTTP rule's Matches array is server-defaulted to a PathPrefix=/
	// entry; the meaningful invariant is that it routes to 8545 and
	// carries no header-based filtering.
	httpRule := evmRoute.Spec.Rules[0]
	for mi, m := range httpRule.Matches {
		g.Expect(m.Headers).To(BeEmpty(), "EVM HTTP rule match[%d] has no header filter", mi)
	}
	g.Expect(*httpRule.BackendRefs[0].Port).To(Equal(gatewayv1.PortNumber(8545)), "EVM HTTP rule routes to 8545")

	wsRule := evmRoute.Spec.Rules[1]
	g.Expect(wsRule.Matches).To(HaveLen(1), "EVM WS rule has one header-match")
	g.Expect(wsRule.Matches[0].Headers).To(HaveLen(1), "EVM WS match has one header")
	wsHeader := wsRule.Matches[0].Headers[0]
	g.Expect(string(wsHeader.Name)).To(Equal("Upgrade"), "EVM WS match header name")
	g.Expect(wsHeader.Value).To(Equal("websocket"), "EVM WS match header value")
	g.Expect(wsHeader.Type).NotTo(BeNil(), "EVM WS match type pointer")
	g.Expect(*wsHeader.Type).To(Equal(gatewayv1.HeaderMatchExact), "EVM WS match type=Exact")
	g.Expect(*wsRule.BackendRefs[0].Port).To(Equal(gatewayv1.PortNumber(8546)), "EVM WS rule routes to 8546")

	// 5. ConditionNetworkingReady lands at False with reason DNSPending:
	//    the controller publishes HTTPRoutes via SSA, then gates the
	//    operator-visible "ready" signal on the external-DNS pipeline
	//    resolving the route's hostname. envtest has no DNS publisher,
	//    so test.local never resolves and the gate stays pending —
	//    the same signal an operator sees when external-DNS is lagging
	//    in a real cluster.
	waitForStatus(t, client.ObjectKeyFromObject(snd), func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		c := apimeta.FindStatusCondition(latest.Status.Conditions, seiv1alpha1.ConditionNetworkingReady)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == "DNSPending"
	}, "ConditionNetworkingReady=False/DNSPending after HTTPRoute publication")
}

// TestNetworking_Validator_NoHTTPRoutes asserts the negative case: when
// the SND template selects validator mode, no protocol is externally
// routable and the controller publishes zero HTTPRoutes — even when
// spec.networking is set. The external Service is still provisioned
// (reconcileExternalService runs unconditionally when Networking != nil),
// which we double-check so a future regression that conflates
// "no routes" with "no external networking at all" surfaces.
func TestNetworking_Validator_NoHTTPRoutes(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "validator-net",
		fixtures.WithReplicas(1),
		fixtures.WithValidator(),
		fixtures.WithNetworking(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	// External Service still appears (Networking != nil is the trigger,
	// independent of validator mode).
	extSvcName := snd.Name + "-external"
	waitFor(t, func() bool {
		svc := &corev1.Service{}
		err := testCli.Get(testCtx, types.NamespacedName{Name: extSvcName, Namespace: ns}, svc)
		return err == nil
	}, "external ClusterIP Service "+extSvcName+" created for validator")

	// Negative wait: poll for a short window asserting no HTTPRoutes
	// appear. We rely on the reconcile loop having a chance to converge
	// (the external Service appearing gives a positive signal that
	// reconcileNetworking ran at least once).
	g.Consistently(func() int {
		return len(listHTTPRoutes(t, snd))
	}, "2s", "200ms").Should(Equal(0),
		"validator-mode SND with networking must publish zero HTTPRoutes")

	// The controller surfaces a False condition with a stable reason
	// rather than removing it. Validator mode declares no
	// externally-routable protocols, so the reason is NoEffectiveRoutes.
	latest := getSND(t, client.ObjectKeyFromObject(snd))
	cond := apimeta.FindStatusCondition(latest.Status.Conditions, seiv1alpha1.ConditionNetworkingReady)
	g.Expect(cond).NotTo(BeNil(), "ConditionNetworkingReady must be set for validator-mode SND")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse),
		"ConditionNetworkingReady must be False for validator-mode SND")
	g.Expect(cond.Reason).To(Equal("NoEffectiveRoutes"),
		"reason surfaces the no-routable-protocols intent")

	// Sanity: deleting the SND cleans up the external Service so the
	// next test on a fresh namespace doesn't see a leak across the
	// envtest cluster. (makeNamespace's t.Cleanup deletes the namespace,
	// but envtest's apiserver-only fake lacks GC; we delete explicitly.)
	g.Expect(testCli.Delete(testCtx, latest)).To(Succeed())
	waitFor(t, func() bool {
		latest := &seiv1alpha1.SeiNodeDeployment{}
		err := testCli.Get(testCtx, client.ObjectKeyFromObject(snd), latest)
		return apierrors.IsNotFound(err)
	}, "validator SND removed")
}
