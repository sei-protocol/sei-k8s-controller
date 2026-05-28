package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// newChildNodeWithSND returns a SeiNode that's owned by the given SND
// via controller owner-ref. Matches the shape the SND reconciler stamps
// at child creation time (see nodedeployment/nodes.go).
func newChildNodeWithSND(name, namespace string, snd *seiv1alpha1.SeiNodeDeployment) *seiv1alpha1.SeiNode { //nolint:unparam // test helper designed for reuse
	controller := true
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: seiv1alpha1.GroupVersion.String(),
				Kind:       "SeiNodeDeployment",
				Name:       snd.Name,
				UID:        snd.UID,
				Controller: &controller,
			}},
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "test-1",
			Image:    "sei:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
}

func newSNDWithTCP() *seiv1alpha1.SeiNodeDeployment {
	return &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "validators",
			Namespace: "default",
			UID:       types.UID("snd-uid-1"),
		},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Networking: &seiv1alpha1.NetworkingConfig{
				TCP: &seiv1alpha1.TCPConfig{},
			},
		},
	}
}

// publishableCondition fishes ConditionPublishableReady off the node.
func publishableCondition(node *seiv1alpha1.SeiNode) *metav1.Condition {
	return apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionPublishableReady)
}

// TestReconcileExternalAddress_Standalone_NoOwner asserts the
// standalone case: a SeiNode with no controller owner-ref opts out of
// publishability entirely and the gate stays open (ready=true).
func TestReconcileExternalAddress_Standalone_NoOwner(t *testing.T) {
	g := NewWithT(t)
	t.Setenv(envVPCCIDR, "10.0.0.0/16")

	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "standalone", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "test-1",
			Image:    "sei:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}

	r, _ := newNodeReconciler(t, node)
	ctx := context.Background()

	ready, err := r.reconcileExternalAddress(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue(), "standalone node opt-out: gate must be open")

	cond := publishableCondition(node)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonPublishableDisabled))
}

// TestReconcileExternalAddress_OptOut_NoTCP asserts the opt-out path:
// parent SND exists but Spec.Networking is nil. Gate opens, no Service.
func TestReconcileExternalAddress_OptOut_NoTCP(t *testing.T) {
	g := NewWithT(t)
	t.Setenv(envVPCCIDR, "10.0.0.0/16")

	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "fullnode", Namespace: "default", UID: types.UID("snd-uid-2")},
		Spec:       seiv1alpha1.SeiNodeDeploymentSpec{},
	}
	node := newChildNodeWithSND("fullnode-0", "default", snd)

	r, c := newNodeReconciler(t, node, snd)
	ctx := context.Background()

	ready, err := r.reconcileExternalAddress(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())

	// No Service should exist.
	svc := &corev1.Service{}
	err = c.Get(ctx, types.NamespacedName{Name: p2pServiceName(node), Namespace: "default"}, svc)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())

	cond := publishableCondition(node)
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonPublishableDisabled))
}

// TestReconcileExternalAddress_OptOut_ClearsStaleService asserts that
// transitioning tcp:{} → nil deletes a pre-existing per-pod Service
// and clears Status.ExternalAddress (the only path that may clear it).
func TestReconcileExternalAddress_OptOut_ClearsStaleService(t *testing.T) {
	g := NewWithT(t)
	t.Setenv(envVPCCIDR, "10.0.0.0/16")

	snd := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "fullnode", Namespace: "default", UID: types.UID("snd-uid-3")},
		Spec:       seiv1alpha1.SeiNodeDeploymentSpec{}, // TCP nil
	}
	node := newChildNodeWithSND("fullnode-0", "default", snd)
	node.Status.ExternalAddress = "stale.nlb.aws:26656"

	// Stale Service exists.
	staleSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p2pServiceName(node),
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Port: 26656, Protocol: corev1.ProtocolTCP}},
		},
	}

	r, c := newNodeReconciler(t, node, snd, staleSvc)
	ctx := context.Background()

	ready, err := r.reconcileExternalAddress(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue())

	// Service deleted.
	check := &corev1.Service{}
	err = c.Get(ctx, types.NamespacedName{Name: staleSvc.Name, Namespace: "default"}, check)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "stale Service must be deleted on opt-out")

	// ExternalAddress cleared.
	g.Expect(node.Status.ExternalAddress).To(BeEmpty(),
		"Status.ExternalAddress must clear on intentional opt-out")
}

// TestReconcileExternalAddress_VPCCIDRUnset_FailsClosed asserts the
// safety net: even with TCP requested, an unset SEI_VPC_CIDR holds the
// gate closed (ready=false) and stamps no Service.
func TestReconcileExternalAddress_VPCCIDRUnset_FailsClosed(t *testing.T) {
	g := NewWithT(t)
	// Explicitly unset for this test — t.Setenv("", "") panics, so use
	// the OS env directly via t.Setenv on a known-empty value.
	t.Setenv(envVPCCIDR, "")

	snd := newSNDWithTCP()
	node := newChildNodeWithSND("validators-0", "default", snd)

	r, c := newNodeReconciler(t, node, snd)
	ctx := context.Background()

	ready, err := r.reconcileExternalAddress(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "fail-closed: gate must hold while SEI_VPC_CIDR is unset")

	// No Service stamped.
	svc := &corev1.Service{}
	err = c.Get(ctx, types.NamespacedName{Name: p2pServiceName(node), Namespace: "default"}, svc)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "must not stamp Service when fail-closed")

	cond := publishableCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonVPCCIDRNotConfigured))
}

// TestReconcileExternalAddress_PodOutsideCIDR_FailsClosed asserts that
// once the pod exists with an IP outside the VPC CIDR, the gate stays
// closed and no Service is stamped — the harbor-CGNAT safety net.
func TestReconcileExternalAddress_PodOutsideCIDR_FailsClosed(t *testing.T) {
	g := NewWithT(t)
	t.Setenv(envVPCCIDR, "10.0.0.0/16")

	snd := newSNDWithTCP()
	node := newChildNodeWithSND("validators-0", "default", snd)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "validators-0-0", Namespace: "default"},
		Status:     corev1.PodStatus{PodIP: "100.64.5.5"}, // CGNAT space, outside 10.0.0.0/16
	}

	r, c := newNodeReconciler(t, node, snd, pod)
	ctx := context.Background()

	ready, err := r.reconcileExternalAddress(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())

	svc := &corev1.Service{}
	err = c.Get(ctx, types.NamespacedName{Name: p2pServiceName(node), Namespace: "default"}, svc)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "must not stamp Service when pod IP is outside VPC CIDR")

	cond := publishableCondition(node)
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonPodOutsideVPCCIDR))
}

// TestReconcileExternalAddress_StampsServiceAndHoldsGate covers the
// first-reconcile shape: TCP requested, no pod yet (STS hasn't run).
// Controller stamps the Service (which has no LB hostname populated
// in fake client), and holds the gate closed pending hostname.
func TestReconcileExternalAddress_StampsServiceAndHoldsGate(t *testing.T) {
	g := NewWithT(t)
	t.Setenv(envVPCCIDR, "10.0.0.0/16")

	snd := newSNDWithTCP()
	node := newChildNodeWithSND("validators-0", "default", snd)

	r, c := newNodeReconciler(t, node, snd)
	ctx := context.Background()

	ready, err := r.reconcileExternalAddress(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "gate held closed until NLB hostname populates")

	// Service stamped with correct shape.
	svc := &corev1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: p2pServiceName(node), Namespace: "default"}, svc)).To(Succeed())
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
	g.Expect(svc.Spec.ExternalTrafficPolicy).To(Equal(corev1.ServiceExternalTrafficPolicyLocal))
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(podNameLabel, "validators-0-0"))
	g.Expect(svc.Spec.Ports).To(HaveLen(1))
	g.Expect(svc.Spec.Ports[0].Port).To(Equal(int32(26656)))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-type", "nlb"))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-scheme", "internet-facing"))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-nlb-target-type", "ip"))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(
		"service.beta.kubernetes.io/aws-load-balancer-cross-zone-load-balancing-enabled", "true"))

	// OwnerRef controller=true to the SeiNode.
	refs := svc.GetOwnerReferences()
	g.Expect(refs).To(HaveLen(1))
	g.Expect(refs[0].UID).To(Equal(node.UID))
	g.Expect(refs[0].Controller).NotTo(BeNil())
	g.Expect(*refs[0].Controller).To(BeTrue())

	// Condition surfaces ServiceProvisioning.
	cond := publishableCondition(node)
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonServiceProvisioning))

	// ExternalAddress not written yet.
	g.Expect(node.Status.ExternalAddress).To(BeEmpty())
}

// TestReconcileExternalAddress_HostnamePopulated_OpensGate covers the
// happy path: Service already exists with a populated LB ingress
// hostname. Controller writes Status.ExternalAddress in bare host:port
// form and opens the gate.
func TestReconcileExternalAddress_HostnamePopulated_OpensGate(t *testing.T) {
	g := NewWithT(t)
	t.Setenv(envVPCCIDR, "10.0.0.0/16")

	snd := newSNDWithTCP()
	node := newChildNodeWithSND("validators-0", "default", snd)

	// Pre-existing Service with populated NLB hostname.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p2pServiceName(node),
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Port: 26656, Protocol: corev1.ProtocolTCP}},
		},
		Status: corev1.ServiceStatus{
			LoadBalancer: corev1.LoadBalancerStatus{
				Ingress: []corev1.LoadBalancerIngress{
					{Hostname: "abc123.elb.us-east-1.amazonaws.com"},
				},
			},
		},
	}

	r, _ := newNodeReconciler(t, node, snd, svc)
	ctx := context.Background()

	ready, err := r.reconcileExternalAddress(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue(), "gate opens once NLB hostname populates")
	g.Expect(node.Status.ExternalAddress).To(Equal("abc123.elb.us-east-1.amazonaws.com:26656"))

	cond := publishableCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonExternalAddressReady))
}

// TestReconcileExternalAddress_EmptyHostGuard_PreservesAddress asserts
// that a transient absence of the LB hostname (Service still exists, no
// ingress entries) does NOT clear an existing Status.ExternalAddress.
// This is the empty-host guard — only intentional opt-out clears it.
func TestReconcileExternalAddress_EmptyHostGuard_PreservesAddress(t *testing.T) {
	g := NewWithT(t)
	t.Setenv(envVPCCIDR, "10.0.0.0/16")

	snd := newSNDWithTCP()
	node := newChildNodeWithSND("validators-0", "default", snd)
	node.Status.ExternalAddress = "previous.nlb.aws:26656"

	// Service exists with NO ingress entries (transient absence).
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p2pServiceName(node),
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{{Port: 26656, Protocol: corev1.ProtocolTCP}},
		},
	}

	r, _ := newNodeReconciler(t, node, snd, svc)
	ctx := context.Background()

	ready, err := r.reconcileExternalAddress(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse())
	g.Expect(node.Status.ExternalAddress).To(Equal("previous.nlb.aws:26656"),
		"empty-host guard must preserve previous ExternalAddress on transient absence")
}

// TestReconcileExternalAddress_PodInsideCIDR_Stamps confirms the
// affirmative-routability case: pod IP inside VPC CIDR → Service stamped.
func TestReconcileExternalAddress_PodInsideCIDR_Stamps(t *testing.T) {
	g := NewWithT(t)
	t.Setenv(envVPCCIDR, "10.0.0.0/16")

	snd := newSNDWithTCP()
	node := newChildNodeWithSND("validators-0", "default", snd)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "validators-0-0", Namespace: "default"},
		Status:     corev1.PodStatus{PodIP: "10.0.5.5"}, // inside 10.0.0.0/16
	}

	r, c := newNodeReconciler(t, node, snd, pod)
	ctx := context.Background()

	ready, err := r.reconcileExternalAddress(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeFalse(), "hostname not yet populated")

	svc := &corev1.Service{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: p2pServiceName(node), Namespace: "default"}, svc)).To(Succeed())
}

// TestReconcileExternalAddress_ParentSNDNotFound covers a race: the
// child SeiNode persists with an owner-ref to an SND that no longer
// exists (e.g., GC lag). Controller treats this as standalone (opt-out).
func TestReconcileExternalAddress_ParentSNDNotFound(t *testing.T) {
	g := NewWithT(t)
	t.Setenv(envVPCCIDR, "10.0.0.0/16")

	snd := newSNDWithTCP()
	node := newChildNodeWithSND("orphan-0", "default", snd)
	// Do NOT add snd to the fake client — simulate the parent gone.

	r, c := newNodeReconciler(t, node)
	ctx := context.Background()

	ready, err := r.reconcileExternalAddress(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ready).To(BeTrue(),
		"parent SND missing: behave like standalone (opt-out), gate open")

	// No Service.
	svc := &corev1.Service{}
	err = c.Get(ctx, types.NamespacedName{Name: p2pServiceName(node), Namespace: "default"}, svc)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

// TestMapSNDToChildSeiNodes asserts the SND→child mapper enqueues only
// SeiNodes whose controller owner-ref UID matches the changed SND.
func TestMapSNDToChildSeiNodes(t *testing.T) {
	g := NewWithT(t)

	snd := newSNDWithTCP()
	otherSND := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "others", Namespace: "default", UID: types.UID("other-uid")},
	}

	childA := newChildNodeWithSND("validators-0", "default", snd)
	childB := newChildNodeWithSND("validators-1", "default", snd)
	otherChild := newChildNodeWithSND("others-0", "default", otherSND)
	standalone := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "standalone", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-1", Image: "sei:latest", FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}

	r, _ := newNodeReconciler(t, snd, otherSND, childA, childB, otherChild, standalone)

	reqs := r.mapSNDToChildSeiNodes(context.Background(), snd)
	g.Expect(reqs).To(HaveLen(2))

	names := make([]string, 0, len(reqs))
	for _, req := range reqs {
		names = append(names, req.Name)
	}
	g.Expect(names).To(ConsistOf("validators-0", "validators-1"))
}

// Defensive: confirm the mapper handles non-SND objects without
// panicking. The handler.EnqueueRequestsFromMapFunc signature accepts
// any client.Object so a misconfigured Watches could feed something
// unexpected; we silently no-op rather than crashing the controller.
func TestMapSNDToChildSeiNodes_NonSNDObject(t *testing.T) {
	g := NewWithT(t)
	r, _ := newNodeReconciler(t)
	reqs := r.mapSNDToChildSeiNodes(context.Background(), &corev1.ConfigMap{})
	g.Expect(reqs).To(BeNil())
}

// Compile-time sanity: client typing of generateP2PService matches what
// the reconciler's SetControllerReference + Patch path expects.
var _ client.Object = &corev1.Service{}
