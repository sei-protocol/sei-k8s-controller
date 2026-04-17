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

func TestGenerateInternalService_NameAndNamespace(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	svc := generateInternalService(group)

	g.Expect(svc.Name).To(Equal("pacific-1-wave-internal"))
	g.Expect(svc.Namespace).To(Equal("pacific-1"))
}

func TestGenerateInternalService_ClusterIPType(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	svc := generateInternalService(group)
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
}

func TestGenerateInternalService_UnconditionalOnNetworking(t *testing.T) {
	g := NewWithT(t)

	// Networking nil — Service still produced.
	private := newTestGroup("private-wave", "ns")
	private.Spec.Networking = nil
	g.Expect(generateInternalService(private).Name).To(Equal("private-wave-internal"))

	// Networking set — Service still produced, identical shape.
	public := newTestGroup("public-wave", "ns")
	public.Spec.Networking = &seiv1alpha1.NetworkingConfig{}
	g.Expect(generateInternalService(public).Name).To(Equal("public-wave-internal"))
}

func TestGenerateInternalService_StatelessPortsOnly(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	svc := generateInternalService(group)

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
	group := newTestGroup("pacific-1-wave", "pacific-1")

	svc := generateInternalService(group)

	// The selector must match the label that the deployment places on every
	// child SeiNode's pod template via ensureSeiNode → generateSeiNode →
	// spec.PodLabels[groupLabel]. Verify the contract by constructing the
	// child node the same way the controller does and checking the pod labels.
	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{groupLabel: "pacific-1-wave"}))

	child := generateSeiNode(group, 0)
	g.Expect(child.Spec.PodLabels).To(HaveKeyWithValue(groupLabel, "pacific-1-wave"),
		"child pods must carry the label the internal Service selects on")
}

func TestGenerateInternalService_SelectorIgnoresRevision(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	// Active rollout — external service pins to revision, internal service must not.
	group.Status.Rollout = &seiv1alpha1.RolloutStatus{
		Strategy:          seiv1alpha1.UpdateStrategyBlueGreen,
		TargetHash:        "newhash",
		IncumbentRevision: "42",
	}

	svc := generateInternalService(group)
	g.Expect(svc.Spec.Selector).To(Equal(map[string]string{groupLabel: "pacific-1-wave"}),
		"internal Service must track ready pods across revisions during rollouts")
}

func TestGenerateInternalService_PublishNotReadyAddressesFalse(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	svc := generateInternalService(group)
	g.Expect(svc.Spec.PublishNotReadyAddresses).To(BeFalse())
}

func TestGenerateInternalService_Labels(t *testing.T) {
	g := NewWithT(t)
	group := newTestGroup("pacific-1-wave", "pacific-1")

	svc := generateInternalService(group)
	g.Expect(svc.Labels).To(HaveKeyWithValue(groupLabel, "pacific-1-wave"))
	g.Expect(svc.Annotations).To(HaveKeyWithValue(managedByAnnotation, controllerName))
}

// --- Reconciler integration (fake client) ---

func TestReconcileInternalService_CreatesServiceAndStampsStatus(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.UID = "group-uid-1"
	group.Spec.Networking = nil // explicitly private — service still expected

	r := newPlanTestReconciler(t, group)

	err := r.reconcileInternalService(ctx, group)
	g.Expect(err).NotTo(HaveOccurred())

	// Status populated in memory (not yet flushed — caller owns that).
	g.Expect(group.Status.InternalService).NotTo(BeNil())
	g.Expect(group.Status.InternalService.Name).To(Equal("pacific-1-wave-internal"))
	g.Expect(group.Status.InternalService.Namespace).To(Equal("pacific-1"))
	g.Expect(group.Status.InternalService.Ports.Rpc).To(Equal(seiconfig.PortRPC))
	g.Expect(group.Status.InternalService.Ports.EvmHttp).To(Equal(seiconfig.PortEVMHTTP))
	g.Expect(group.Status.InternalService.Ports.Rest).To(Equal(seiconfig.PortREST))

	// Service was server-side applied.
	svc := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: "pacific-1-wave-internal", Namespace: "pacific-1"}, svc)
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

func TestReconcileInternalService_IdempotentOnRepeatApply(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.UID = "group-uid-1"

	r := newPlanTestReconciler(t, group)

	for range 3 {
		err := r.reconcileInternalService(ctx, group)
		g.Expect(err).NotTo(HaveOccurred())
	}

	list := &corev1.ServiceList{}
	err := r.List(ctx, list, client.InNamespace("pacific-1"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(list.Items).To(HaveLen(1))
	g.Expect(list.Items[0].Name).To(Equal("pacific-1-wave-internal"))
}

func TestReconcileInternalService_CascadeDeleteViaOwnerRef(t *testing.T) {
	// The fake client does not implement Kubernetes garbage collection;
	// we instead assert the OwnerReference is correctly configured for
	// GC to kick in against a real API server.

	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.UID = "group-uid-cascade"

	r := newPlanTestReconciler(t, group)
	g.Expect(r.reconcileInternalService(ctx, group)).To(Succeed())

	svc := &corev1.Service{}
	g.Expect(r.Get(ctx, types.NamespacedName{Name: "pacific-1-wave-internal", Namespace: "pacific-1"}, svc)).To(Succeed())

	g.Expect(svc.OwnerReferences).To(ContainElement(And(
		HaveField("APIVersion", "sei.io/v1alpha1"),
		HaveField("Kind", "SeiNodeDeployment"),
		HaveField("Name", "pacific-1-wave"),
		HaveField("UID", group.UID),
		HaveField("Controller", gomegaPtrBool(true)),
		HaveField("BlockOwnerDeletion", gomegaPtrBool(true)),
	)))
}

// --- Orphan helper (Retain deletion policy) ---

func TestOrphanInternalService_RemovesOwnerRef(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.UID = "group-uid-orphan"

	internalSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      internalServiceName(group),
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

	g.Expect(r.orphanInternalService(ctx, group)).To(Succeed())

	got := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: internalSvc.Name, Namespace: internalSvc.Namespace}, got)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(got.OwnerReferences).To(BeEmpty())
}

func TestOrphanInternalService_MissingServiceIsNoOp(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.UID = "group-uid-missing"

	r := newPlanTestReconciler(t, group)

	g.Expect(r.orphanInternalService(ctx, group)).To(Succeed())

	got := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: internalServiceName(group), Namespace: group.Namespace}, got)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
}

// --- Lifecycle: internal Service is independent of Spec.Networking transitions ---

// TestInternalService_SurvivesNetworkingTeardown confirms the internal
// Service is NOT deleted or orphaned when .spec.networking transitions to
// nil. Its lifecycle is unconditional and owned by reconcileInternalService
// + the parent SeiNodeDeployment deletion (via OwnerRef cascade), not by
// the networking-resources teardown path.
func TestInternalService_SurvivesNetworkingTeardown(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	group := newTestGroup("pacific-1-wave", "pacific-1")
	group.UID = "group-uid-transition"
	group.Spec.Networking = &seiv1alpha1.NetworkingConfig{}

	r := newPlanTestReconciler(t, group)

	// Initial reconcile with Networking set — internal Service exists.
	g.Expect(r.reconcileInternalService(ctx, group)).To(Succeed())

	svc := &corev1.Service{}
	g.Expect(r.Get(ctx, types.NamespacedName{
		Name:      internalServiceName(group),
		Namespace: group.Namespace,
	}, svc)).To(Succeed())

	// Simulate the transition: Networking → nil, which triggers
	// deleteNetworkingResources on the next reconcile. The internal Service
	// must NOT be touched by that path.
	group.Spec.Networking = nil
	g.Expect(r.deleteNetworkingResources(ctx, group)).To(Succeed())

	// Internal Service still present with its owner reference intact.
	after := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{
		Name:      internalServiceName(group),
		Namespace: group.Namespace,
	}, after)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(after.OwnerReferences).NotTo(BeEmpty())
	g.Expect(after.OwnerReferences[0].UID).To(Equal(group.UID))
}

// gomegaPtrBool returns a matcher that checks a *bool equals the given value.
func gomegaPtrBool(expected bool) OmegaMatcher {
	return WithTransform(func(p *bool) bool {
		return p != nil && *p == expected
	}, BeTrue())
}
