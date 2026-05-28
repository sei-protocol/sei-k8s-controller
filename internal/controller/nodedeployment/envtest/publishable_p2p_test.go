//go:build envtest

package envtest_test

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest/fixtures"
)

// testVPCCIDR is the VPC CIDR the publishable-p2p tests configure via
// SEI_VPC_CIDR. The fake-pod IPs the tests stamp must fall inside it
// to clear the routability gate. envtest has no kube-controller-manager
// so we set podIP manually after the controller creates the STS.
const testVPCCIDR = "10.0.0.0/16"

// TestPublishableP2P_HoldsGateUntilHostnamePopulates is the bootstrap-
// gate happy path. With Networking.TCP set:
//
//  1. The SeiNode controller stamps a per-pod LB Service named
//     "<seinode>-p2p" — but does NOT create a StatefulSet yet.
//  2. After we patch the Service's status.loadBalancer.ingress with a
//     fake NLB hostname, the next reconcile writes Status.ExternalAddress
//     in bare host:port form and the StatefulSet appears.
//  3. Removing Networking.TCP from the SND clears Status.ExternalAddress
//     and deletes the per-pod Service (opt-out cleanup).
//
// The test isolates SEI_VPC_CIDR to itself via t.Setenv so unrelated
// envtests aren't affected by the fail-closed env var.
func TestPublishableP2P_HoldsGateUntilHostnamePopulates(t *testing.T) {
	g := NewWithT(t)
	t.Setenv("SEI_VPC_CIDR", testVPCCIDR)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "validators",
		fixtures.WithReplicas(1),
		fixtures.WithValidator(),
		fixtures.WithPublishableP2P(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	// 1. Child SeiNode appears.
	var child *seiv1alpha1.SeiNode
	waitFor(t, func() bool {
		kids := listChildren(t, snd)
		if len(kids) != 1 {
			return false
		}
		child = &kids[0]
		return true
	}, "child SeiNode created")

	// 2. The per-pod LB Service exists with the publishable shape.
	p2pSvcName := child.Name + "-p2p"
	waitFor(t, func() bool {
		svc := &corev1.Service{}
		return testCli.Get(testCtx, types.NamespacedName{
			Name:      p2pSvcName,
			Namespace: ns,
		}, svc) == nil
	}, "per-pod p2p Service "+p2pSvcName+" created")

	svc := &corev1.Service{}
	g.Expect(testCli.Get(testCtx, types.NamespacedName{Name: p2pSvcName, Namespace: ns}, svc)).To(Succeed())
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
	g.Expect(svc.Spec.ExternalTrafficPolicy).To(Equal(corev1.ServiceExternalTrafficPolicyLocal))
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(
		"statefulset.kubernetes.io/pod-name", child.Name+"-0"))
	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(26656)))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-type", "nlb"))

	// Owner-ref points at the SeiNode (controller=true) so the existing
	// Owns(&Service{}) watch fires on status updates.
	refs := svc.GetOwnerReferences()
	g.Expect(refs).To(HaveLen(1))
	g.Expect(refs[0].UID).To(Equal(child.UID))
	g.Expect(refs[0].Controller).NotTo(BeNil())
	g.Expect(*refs[0].Controller).To(BeTrue())

	// 3. PublishableReady condition surfaces ServiceProvisioning while
	//    the LB hostname is unpopulated.
	waitFor(t, func() bool {
		latest := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, client.ObjectKeyFromObject(child), latest); err != nil {
			return false
		}
		c := apimeta.FindStatusCondition(latest.Status.Conditions, seiv1alpha1.ConditionPublishableReady)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == seiv1alpha1.ReasonServiceProvisioning
	}, "PublishableReady=False/ServiceProvisioning while gate held")

	// 4. CRITICAL: no StatefulSet exists yet. The gate must hold STS
	//    creation until the NLB hostname populates.
	g.Consistently(func() bool {
		sts := &appsv1.StatefulSet{}
		err := testCli.Get(testCtx, types.NamespacedName{Name: child.Name, Namespace: ns}, sts)
		return apierrors.IsNotFound(err)
	}, "2s", "200ms").Should(BeTrue(),
		"StatefulSet must NOT be created while publishable gate is closed")

	// 5. Patch the Service's loadBalancer.ingress with a fake NLB
	//    hostname. controller-runtime envtest gives us a real apiserver
	//    that honors a /status subresource patch on Services.
	patchBase := svc.DeepCopy()
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{
		Hostname: "abc.nlb.us-east-1.amazonaws.com",
	}}
	g.Expect(testCli.Status().Patch(testCtx, svc, client.MergeFrom(patchBase))).To(Succeed())

	// 6. Status.ExternalAddress populates with bare host:port form.
	waitForChildExternalAddress(t, child, "abc.nlb.us-east-1.amazonaws.com:26656")

	// 7. PublishableReady transitions to True/ExternalAddressReady.
	waitFor(t, func() bool {
		latest := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, client.ObjectKeyFromObject(child), latest); err != nil {
			return false
		}
		c := apimeta.FindStatusCondition(latest.Status.Conditions, seiv1alpha1.ConditionPublishableReady)
		return c != nil && c.Status == metav1.ConditionTrue && c.Reason == seiv1alpha1.ReasonExternalAddressReady
	}, "PublishableReady=True/ExternalAddressReady after hostname populates")

	// 8. StatefulSet now appears — gate is open.
	waitFor(t, func() bool {
		sts := &appsv1.StatefulSet{}
		return testCli.Get(testCtx, types.NamespacedName{Name: child.Name, Namespace: ns}, sts) == nil
	}, "StatefulSet created after gate opens")

	// 9. Toggle Networking.TCP off — opt-out cleanup must remove the
	//    per-pod Service and clear Status.ExternalAddress.
	latestSND := getSND(t, client.ObjectKeyFromObject(snd))
	sndPatch := client.MergeFrom(latestSND.DeepCopy())
	latestSND.Spec.Networking.TCP = nil
	g.Expect(testCli.Patch(testCtx, latestSND, sndPatch)).To(Succeed())

	waitFor(t, func() bool {
		latest := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, client.ObjectKeyFromObject(child), latest); err != nil {
			return false
		}
		return latest.Status.ExternalAddress == ""
	}, "Status.ExternalAddress clears on opt-out")

	waitFor(t, func() bool {
		check := &corev1.Service{}
		err := testCli.Get(testCtx, types.NamespacedName{Name: p2pSvcName, Namespace: ns}, check)
		return apierrors.IsNotFound(err)
	}, "per-pod p2p Service deleted on opt-out")

	// 10. PublishableReady stays present (always-present discipline)
	//     and transitions to False/PublishableDisabled.
	latestChild := &seiv1alpha1.SeiNode{}
	g.Expect(testCli.Get(testCtx, client.ObjectKeyFromObject(child), latestChild)).To(Succeed())
	cond := apimeta.FindStatusCondition(latestChild.Status.Conditions, seiv1alpha1.ConditionPublishableReady)
	g.Expect(cond).NotTo(BeNil(), "PublishableReady must remain present after opt-out")
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonPublishableDisabled))
}

// waitForChildExternalAddress polls until the child SeiNode's
// Status.ExternalAddress equals want. Distinct from generic
// waitForStatus so the failure message names the field directly.
func waitForChildExternalAddress(t *testing.T, child *seiv1alpha1.SeiNode, want string) {
	t.Helper()
	waitFor(t, func() bool {
		latest := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, client.ObjectKeyFromObject(child), latest); err != nil {
			return false
		}
		return strings.EqualFold(latest.Status.ExternalAddress, want)
	}, "Status.ExternalAddress == "+want)
}
