package seinetwork

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	seiconfig "github.com/sei-protocol/sei-config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// --- Pure generator ---

func TestGenerateInternalService_NameAndNamespace(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testGroupLabelValue, testNamespace)

	svc := generateInternalService(network)

	g.Expect(svc.Name).To(Equal(testInternalSvcName))
	g.Expect(svc.Namespace).To(Equal(testNamespace))
}

func TestGenerateInternalService_ClusterIPType(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testGroupLabelValue, testNamespace)

	svc := generateInternalService(network)
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
}

func TestGenerateInternalService_StatelessPortsOnly(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testGroupLabelValue, testNamespace)

	svc := generateInternalService(network)

	// Only stateless HTTP request/response ports belong on a kube-proxy L4 LB.
	// Stateful protocols (evm-ws, grpc streaming) go via per-node headless Services.
	g.Expect(svc.Spec.Ports).To(HaveLen(3))

	byName := map[string]corev1.ServicePort{}
	for _, p := range svc.Spec.Ports {
		byName[p.Name] = p
	}

	g.Expect(byName).To(HaveKey("rpc"))
	g.Expect(byName).To(HaveKey("evm-http"))
	g.Expect(byName).To(HaveKey("rest"))
	g.Expect(byName).NotTo(HaveKey("evm-ws"), "WebSocket is stateful; must not be load-balanced")
	g.Expect(byName).NotTo(HaveKey("grpc"), "HTTP/2 gRPC pins per-connection at L4; must not be exposed here")

	g.Expect(byName["rpc"].Port).To(Equal(seiconfig.PortRPC))
	g.Expect(byName["evm-http"].Port).To(Equal(seiconfig.PortEVMHTTP))
	g.Expect(byName["rest"].Port).To(Equal(seiconfig.PortREST))

	for _, p := range svc.Spec.Ports {
		g.Expect(p.Protocol).To(Equal(corev1.ProtocolTCP))
		g.Expect(p.TargetPort.IntVal).To(Equal(p.Port))
	}
}

func TestGenerateInternalService_SelectorMatchesPodLabel(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testGroupLabelValue, testNamespace)

	svc := generateInternalService(network)

	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{groupLabel: testGroupLabelValue}))

	child := generateSeiNode(network, 0)
	g.Expect(child.Spec.PodLabels).To(HaveKeyWithValue(groupLabel, testGroupLabelValue),
		"child pods must carry the label the internal Service selects on")
}

func TestGenerateInternalService_SelectorIgnoresRevision(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testGroupLabelValue, testNamespace)

	// Active rollout — selector must not include the revision label so
	// kube-proxy keeps routing to whichever pods are Ready during the rollout.
	network.Status.Rollout = &seiv1alpha1.RolloutStatus{TargetHash: "newhash"}

	svc := generateInternalService(network)
	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{groupLabel: testGroupLabelValue}),
		"internal Service must track ready pods across revisions during rollouts")
	g.Expect(svc.Spec.Selector).NotTo(HaveKey(revisionLabel),
		"selector must not pin traffic by generation during an InPlace rollout")
}

func TestGenerateInternalService_PublishNotReadyAddressesFalse(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testGroupLabelValue, testNamespace)

	svc := generateInternalService(network)
	g.Expect(svc.Spec.PublishNotReadyAddresses).To(BeFalse())
}

func TestGenerateInternalService_Labels(t *testing.T) {
	g := NewWithT(t)
	network := newTestNetwork(testGroupLabelValue, testNamespace)

	svc := generateInternalService(network)
	g.Expect(svc.Labels).To(HaveKeyWithValue(groupLabel, testGroupLabelValue))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(managedByAnnotation, controllerName))
}

// --- Reconciler integration (fake client) ---

func TestReconcileInternalService_CreatesServiceAndStampsStatus(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testGroupLabelValue, testNamespace)
	network.UID = "network-uid-1"

	r := newPlanTestReconciler(t, network)

	err := r.reconcileInternalService(ctx, network)
	g.Expect(err).NotTo(HaveOccurred())

	// Status populated in memory (not yet flushed — caller owns that).
	g.Expect(network.Status.InternalService).NotTo(BeNil())
	g.Expect(network.Status.InternalService.Name).To(Equal(testInternalSvcName))
	g.Expect(network.Status.InternalService.Namespace).To(Equal(testNamespace))
	g.Expect(network.Status.InternalService.Ports.Rpc).To(Equal(seiconfig.PortRPC))
	g.Expect(network.Status.InternalService.Ports.EvmHttp).To(Equal(seiconfig.PortEVMHTTP))
	g.Expect(network.Status.InternalService.Ports.Rest).To(Equal(seiconfig.PortREST))

	// Service was server-side applied.
	svc := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: testInternalSvcName, Namespace: testNamespace}, svc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(groupLabel, testGroupLabelValue))

	// Owner reference points at the network for cascade deletion.
	g.Expect(svc.OwnerReferences).To(HaveLen(1))
	g.Expect(svc.OwnerReferences[0].UID).To(Equal(network.UID))
	g.Expect(svc.OwnerReferences[0].Kind).To(Equal(testKind))
	g.Expect(svc.OwnerReferences[0].Controller).NotTo(BeNil())
	g.Expect(*svc.OwnerReferences[0].Controller).To(BeTrue())
}

func TestReconcileInternalService_IdempotentOnRepeatApply(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testGroupLabelValue, testNamespace)
	network.UID = "network-uid-1"

	r := newPlanTestReconciler(t, network)

	for range 3 {
		err := r.reconcileInternalService(ctx, network)
		g.Expect(err).NotTo(HaveOccurred())
	}

	list := &corev1.ServiceList{}
	err := r.List(ctx, list, client.InNamespace(testNamespace))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(list.Items).To(HaveLen(1))
	g.Expect(list.Items[0].Name).To(Equal(testInternalSvcName))
}

func TestReconcileInternalService_CascadeDeleteViaOwnerRef(t *testing.T) {
	// The fake client does not implement Kubernetes garbage collection;
	// we instead assert the OwnerReference is correctly configured for
	// GC to kick in against a real API server.

	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testGroupLabelValue, testNamespace)
	network.UID = "network-uid-cascade"

	r := newPlanTestReconciler(t, network)
	g.Expect(r.reconcileInternalService(ctx, network)).To(Succeed())

	svc := &corev1.Service{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: testInternalSvcName, Namespace: testNamespace}, svc)).To(Succeed())

	g.Expect(svc.OwnerReferences).To(ContainElement(And(
		HaveField("APIVersion", testAPIVersion),
		HaveField("Kind", testKind),
		HaveField("Name", testGroupLabelValue),
		HaveField("UID", network.UID),
		HaveField("Controller", gomegaPtrBool(true)),
		HaveField("BlockOwnerDeletion", gomegaPtrBool(true)),
	)))
}

// --- Orphan helper (Retain deletion policy) ---

func TestOrphanInternalService_RemovesOwnerRef(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testGroupLabelValue, testNamespace)
	network.UID = "network-uid-orphan"

	internalSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      internalServiceName(network),
			Namespace: network.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: testAPIVersion,
				Kind:       testKind,
				Name:       network.Name,
				UID:        network.UID,
				Controller: new(true),
			}},
		},
	}

	r := newPlanTestReconciler(t, network, internalSvc)

	g.Expect(r.orphanInternalService(ctx, network)).To(Succeed())

	got := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: internalSvc.Name, Namespace: internalSvc.Namespace}, got)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.OwnerReferences).To(BeEmpty())
}

func TestOrphanInternalService_MissingServiceIsNoOp(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	network := newTestNetwork(testGroupLabelValue, testNamespace)
	network.UID = "network-uid-missing"

	r := newPlanTestReconciler(t, network)

	g.Expect(r.orphanInternalService(ctx, network)).To(Succeed())

	got := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: internalServiceName(network), Namespace: network.Namespace}, got)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

// gomegaPtrBool returns a matcher that checks a *bool equals the given value.
func gomegaPtrBool(expected bool) OmegaMatcher {
	return WithTransform(func(p *bool) bool {
		return p != nil && *p == expected
	}, BeTrue())
}
