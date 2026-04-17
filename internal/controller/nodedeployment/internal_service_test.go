package nodedeployment

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

func TestGenerateInternalRpcService_NameAndNamespace(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	svc := generateInternalRpcService(group)

	g.Expect(svc.Name).To(Equal("pacific-1-wave-rpc"))
	g.Expect(svc.Namespace).To(Equal("pacific-1"))
}

func TestGenerateInternalRpcService_ClusterIPType(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	svc := generateInternalRpcService(group)
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
}

func TestGenerateInternalRpcService_UnconditionalOnNetworking(t *testing.T) {
	g := NewWithT(t)

	// Networking nil — Service still produced.
	private := newTestGroup("private-wave", "ns")
	private.Spec.Networking = nil
	g.Expect(generateInternalRpcService(private).Name).To(Equal("private-wave-rpc"))

	// Networking set — Service still produced, identical shape.
	public := newTestGroup("public-wave", "ns")
	public.Spec.Networking = &seiv1alpha1.NetworkingConfig{}
	g.Expect(generateInternalRpcService(public).Name).To(Equal("public-wave-rpc"))
}

func TestGenerateInternalRpcService_FiveNamedPorts(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	svc := generateInternalRpcService(group)
	g.Expect(svc.Spec.Ports).To(HaveLen(5))

	byName := map[string]corev1.ServicePort{}
	for _, p := range svc.Spec.Ports {
		byName[p.Name] = p
	}

	// Port names match the milestone contract, not seiconfig.NodePorts()
	// (which uses "evm-rpc"). These names are part of the public interface.
	g.Expect(byName).To(HaveKey("rpc"))
	g.Expect(byName).To(HaveKey("evm-http"))
	g.Expect(byName).To(HaveKey("evm-ws"))
	g.Expect(byName).To(HaveKey("rest"))
	g.Expect(byName).To(HaveKey("grpc"))

	g.Expect(byName["rpc"].Port).To(Equal(seiconfig.PortRPC))
	g.Expect(byName["evm-http"].Port).To(Equal(seiconfig.PortEVMHTTP))
	g.Expect(byName["evm-ws"].Port).To(Equal(seiconfig.PortEVMWS))
	g.Expect(byName["rest"].Port).To(Equal(seiconfig.PortREST))
	g.Expect(byName["grpc"].Port).To(Equal(seiconfig.PortGRPC))

	for _, p := range svc.Spec.Ports {
		g.Expect(p.Protocol).To(Equal(corev1.ProtocolTCP))
		g.Expect(p.TargetPort.IntVal).To(Equal(p.Port))
	}
}

func TestGenerateInternalRpcService_GRPCAppProtocol(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	const grpcPortName = "grpc"
	svc := generateInternalRpcService(group)
	for _, p := range svc.Spec.Ports {
		if p.Name == grpcPortName {
			g.Expect(p.AppProtocol).NotTo(BeNil())
			g.Expect(*p.AppProtocol).To(Equal("kubernetes.io/h2c"))
			return
		}
	}
	t.Fatal("grpc port not found")
}

func TestGenerateInternalRpcService_SelectorMatchesPodLabel(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	svc := generateInternalRpcService(group)

	// The selector must match the label that the deployment places on every
	// child SeiNode's pod template via ensureSeiNode → generateSeiNode →
	// spec.PodLabels[groupLabel]. Verify the contract by constructing the
	// child node the same way the controller does and checking the pod labels.
	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{groupLabel: "pacific-1-wave"}))

	child := generateSeiNode(group, 0)
	g.Expect(child.Spec.PodLabels).To(HaveKeyWithValue(groupLabel, "pacific-1-wave"),
		"child pods must carry the label the RPC Service selects on")
}

func TestGenerateInternalRpcService_SelectorIgnoresRevision(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	// Active rollout — external service pins to revision, internal service must not.
	group.Status.Rollout = &seiv1alpha1.RolloutStatus{
		Strategy:          seiv1alpha1.UpdateStrategyBlueGreen,
		TargetHash:        "newhash",
		IncumbentRevision: "42",
	}

	svc := generateInternalRpcService(group)
	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{groupLabel: "pacific-1-wave"}),
		"internal RPC Service must track ready pods across revisions during rollouts")
}

func TestGenerateInternalRpcService_PublishNotReadyAddressesFalse(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	svc := generateInternalRpcService(group)
	g.Expect(svc.Spec.PublishNotReadyAddresses).To(BeFalse())
}

func TestGenerateInternalRpcService_Labels(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	svc := generateInternalRpcService(group)
	g.Expect(svc.Labels).To(HaveKeyWithValue(groupLabel, "pacific-1-wave"))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(managedByAnnotation, controllerName))
}

// --- Reconciler integration (fake client) ---

func TestReconcileInternalRpcService_CreatesServiceAndStampsStatus(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.UID = "group-uid-1"
	group.Spec.Networking = nil // explicitly private — service still expected

	r := newPlanTestReconciler(t, group)

	err := r.reconcileInternalRpcService(ctx, group)
	g.Expect(err).NotTo(HaveOccurred())

	// Status populated in memory (not yet flushed — caller owns that).
	g.Expect(group.Status.RpcService).NotTo(BeNil())
	g.Expect(group.Status.RpcService.Name).To(Equal("pacific-1-wave-rpc"))
	g.Expect(group.Status.RpcService.Namespace).To(Equal("pacific-1"))
	g.Expect(group.Status.RpcService.Ports.Rpc).To(Equal(seiconfig.PortRPC))
	g.Expect(group.Status.RpcService.Ports.EvmHttp).To(Equal(seiconfig.PortEVMHTTP))
	g.Expect(group.Status.RpcService.Ports.EvmWs).To(Equal(seiconfig.PortEVMWS))
	g.Expect(group.Status.RpcService.Ports.Rest).To(Equal(seiconfig.PortREST))
	g.Expect(group.Status.RpcService.Ports.Grpc).To(Equal(seiconfig.PortGRPC))

	// Service was server-side applied.
	svc := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: "pacific-1-wave-rpc", Namespace: "pacific-1"}, svc)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
	g.Expect(svc.Spec.Selector).To(HaveKeyWithValue(groupLabel, "pacific-1-wave"))

	// Owner reference points at the deployment for cascade deletion.
	g.Expect(svc.OwnerReferences).To(HaveLen(1))
	g.Expect(svc.OwnerReferences[0].UID).To(Equal(group.UID))
	g.Expect(svc.OwnerReferences[0].Kind).To(Equal("SeiNodeDeployment"))
	g.Expect(svc.OwnerReferences[0].Controller).NotTo(BeNil())
	g.Expect(*svc.OwnerReferences[0].Controller).To(BeTrue())
}

func TestReconcileInternalRpcService_IdempotentOnRepeatApply(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.UID = "group-uid-1"

	r := newPlanTestReconciler(t, group)

	for range 3 {
		err := r.reconcileInternalRpcService(ctx, group)
		g.Expect(err).NotTo(HaveOccurred())
	}

	list := &corev1.ServiceList{}
	err := r.List(ctx, list, client.InNamespace("pacific-1"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(list.Items).To(HaveLen(1))
	g.Expect(list.Items[0].Name).To(Equal("pacific-1-wave-rpc"))
}

func TestReconcileInternalRpcService_CascadeDeleteViaOwnerRef(t *testing.T) {
	// The fake client does not implement Kubernetes garbage collection;
	// we instead assert the OwnerReference is correctly configured for
	// GC to kick in against a real API server. A manual Delete against
	// the parent, followed by apply-via-owner-cascade, is outside the
	// fake client's ability to simulate. We verify the ownerRef shape
	// here, which is the contract the apiserver enforces.

	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.UID = "group-uid-cascade"

	r := newPlanTestReconciler(t, group)
	g.Expect(r.reconcileInternalRpcService(ctx, group)).To(Succeed())

	svc := &corev1.Service{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: "pacific-1-wave-rpc", Namespace: "pacific-1"}, svc)).To(Succeed())

	g.Expect(svc.OwnerReferences).To(ContainElement(And(
		HaveField("APIVersion", "sei.io/v1alpha1"),
		HaveField("Kind", "SeiNodeDeployment"),
		HaveField("Name", "pacific-1-wave"),
		HaveField("UID", group.UID),
		HaveField("Controller", gomegaPtrBool(true)),
		HaveField("BlockOwnerDeletion", gomegaPtrBool(true)),
	)))
}

func TestOrphanNetworkingResources_RemovesInternalServiceOwnerRef(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.UID = "group-uid-orphan"

	// Pre-create both services with the group as controller.
	internalSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      internalRpcServiceName(group),
			Namespace: group.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "sei.io/v1alpha1",
				Kind:       "SeiNodeDeployment",
				Name:       group.Name,
				UID:        group.UID,
				Controller: boolPtr(true),
			}},
		},
	}

	r := newPlanTestReconciler(t, group, internalSvc)

	g.Expect(r.orphanNetworkingResources(ctx, group)).To(Succeed())

	got := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: internalSvc.Name, Namespace: internalSvc.Namespace}, got)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.OwnerReferences).To(BeEmpty())
}

func TestOrphanNetworkingResources_NoInternalServiceIsNoOp(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.UID = "group-uid-missing"

	r := newPlanTestReconciler(t, group)

	err := r.orphanNetworkingResources(ctx, group)
	g.Expect(err).NotTo(HaveOccurred())

	// Confirm the service really is absent.
	got := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: internalRpcServiceName(group), Namespace: group.Namespace}, got)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

// gomegaPtrBool returns a matcher that checks a *bool equals the given value.
func gomegaPtrBool(expected bool) OmegaMatcher {
	return WithTransform(func(p *bool) bool {
		return p != nil && *p == expected
	}, BeTrue())
}
