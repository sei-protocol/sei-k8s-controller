//go:build envtest

package envtest_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest/fixtures"
)

const networkingOrphanedAnnotation = "sei.io/networking-orphaned"

// withNetworkingOrphaned stamps the orphan annotation on a fixtures-built SND.
func withNetworkingOrphaned() fixtures.Option {
	return func(snd *seiv1alpha1.SeiNodeDeployment) {
		if snd.Annotations == nil {
			snd.Annotations = map[string]string{}
		}
		snd.Annotations[networkingOrphanedAnnotation] = "true"
	}
}

// TestNetworking_Orphaned_FreezesExternalButKeepsExternalAddress is the
// PLT-451 Phase-0 controller gate. An SND annotated networking-orphaned hands
// the external Service / HTTPRoutes / per-pod P2P LB to GitOps: the controller
// stops applying them. The load-bearing guarantee is that the children's
// Spec.ExternalAddress write is NOT gated by the annotation — a blanket freeze
// would blank ExternalAddress and partition P2P. The fixture sets both TCP and
// a non-empty P2PEndpointDomain so the ungated child-address path is exercised.
func TestNetworking_Orphaned_FreezesExternalButKeepsExternalAddress(t *testing.T) {
	g := NewWithT(t)
	withP2PEndpointDomain(t, p2pEndpointTestDomain)
	ns := makeNamespace(t)

	// Created already orphaned: the controller must never apply external
	// networking for this group, so no per-pod LB is ever created.
	snd := fixtures.NewSND(ns, "orphan-create",
		fixtures.WithReplicas(1),
		withTCP(),
		withNetworkingOrphaned(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	const chainID = "pacific-1" // fixtures default
	wantAddr := expectedP2PEndpointAddr(snd.Name, chainID, 0)

	// (b) Child still gets Spec.ExternalAddress — ensureSeiNode /
	//     p2pEndpointAddressForChild are not gated by the annotation.
	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress == wantAddr
	}, "orphaned SND still stamps child Spec.ExternalAddress")

	// (c) ConditionNetworkingReady=False/NetworkingOrphaned (always-present;
	//     a stable reason, not a removed condition).
	waitForStatus(t, client.ObjectKeyFromObject(snd), func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		c := apimeta.FindStatusCondition(latest.Status.Conditions, seiv1alpha1.ConditionNetworkingReady)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == "NetworkingOrphaned"
	}, "ConditionNetworkingReady=False/NetworkingOrphaned for orphaned SND")

	// (a) reconcileNetworking did not run: no per-pod P2P LB Service exists.
	//     The condition + child address above confirm the reconcile reached
	//     reconcileSeiNodes, so this is a true negative (the reconcile ran but
	//     skipped the external sub-reconcilers), not a not-yet-converged read.
	svcKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}
	g.Consistently(func() bool {
		err := testCli.Get(testCtx, svcKey, &corev1.Service{})
		return err != nil
	}, 2*time.Second, 200*time.Millisecond).Should(BeTrue(),
		"orphaned SND must not create a per-pod P2P LB Service")
}

// TestNetworking_Orphaned_StripsOwnerRefs covers the orphan transition on a
// live group: an SND converges with the controller owning its per-pod P2P LB,
// then gets the orphan annotation. The controller strips its owner-ref
// (handing the object to Flux for adopt-in-place) without deleting it, and
// the child keeps its ExternalAddress throughout.
func TestNetworking_Orphaned_StripsOwnerRefs(t *testing.T) {
	g := NewWithT(t)
	withP2PEndpointDomain(t, p2pEndpointTestDomain)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "orphan-transition",
		fixtures.WithReplicas(1),
		withTCP(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	const chainID = "pacific-1"
	wantAddr := expectedP2PEndpointAddr(snd.Name, chainID, 0)
	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}
	svcKey := types.NamespacedName{Name: snd.Name + "-0-p2p", Namespace: ns}

	// Converge: child address set and per-pod LB owned by the SND.
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.ExternalAddress == wantAddr
	}, "child has publishable address before orphan")
	waitFor(t, func() bool {
		svc := &corev1.Service{}
		if err := testCli.Get(testCtx, svcKey, svc); err != nil {
			return false
		}
		return metav1.IsControlledBy(svc, snd)
	}, "per-pod P2P LB owned by SND before orphan")

	// Orphan: stamp the annotation. The SND watch uses
	// GenerationChangedPredicate, so an annotation-only edit isn't picked up
	// until the next statusPollInterval requeue (~30s) — operationally fine
	// (the design gates on a multi-interval freeze) but slow for a test. Bump
	// generation with a benign template-metadata edit so a reconcile fires
	// promptly; this is orthogonal to the orphan gate under test.
	latest := getSND(t, client.ObjectKeyFromObject(snd))
	patch := client.MergeFrom(latest.DeepCopy())
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	latest.Annotations[networkingOrphanedAnnotation] = "true"
	if latest.Spec.Template.Metadata == nil {
		latest.Spec.Template.Metadata = &seiv1alpha1.SeiNodeTemplateMeta{}
	}
	if latest.Spec.Template.Metadata.Labels == nil {
		latest.Spec.Template.Metadata.Labels = map[string]string{}
	}
	latest.Spec.Template.Metadata.Labels["test.sei.io/orphan-retick"] = "1"
	g.Expect(testCli.Patch(testCtx, latest, patch)).To(Succeed())

	// Owner-ref is stripped — the Service survives, now GitOps-owned.
	waitFor(t, func() bool {
		svc := &corev1.Service{}
		if err := testCli.Get(testCtx, svcKey, svc); err != nil {
			return false
		}
		return len(svc.GetOwnerReferences()) == 0
	}, "per-pod P2P LB owner-ref stripped after orphan")

	// Service is not deleted (orphan is metadata-only, not a delete).
	svc := &corev1.Service{}
	g.Expect(testCli.Get(testCtx, svcKey, svc)).To(Succeed())
	g.Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))

	// Child keeps its ExternalAddress across the transition.
	g.Consistently(func() string {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return ""
		}
		return child.Spec.ExternalAddress
	}, 2*time.Second, 200*time.Millisecond).Should(Equal(wantAddr),
		"child ExternalAddress preserved across orphan transition")

	// Condition reflects the orphaned state.
	waitForStatus(t, client.ObjectKeyFromObject(snd), func(latest *seiv1alpha1.SeiNodeDeployment) bool {
		c := apimeta.FindStatusCondition(latest.Status.Conditions, seiv1alpha1.ConditionNetworkingReady)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == "NetworkingOrphaned"
	}, "ConditionNetworkingReady=False/NetworkingOrphaned after orphan transition")
}

// TestNetworking_Delete_SkipsOrphanedObjects is the rollback/footgun guard.
// Once networking is orphaned (owner-refs stripped, objects handed to GitOps),
// the controller's delete path must skip those objects. Otherwise unsetting the
// annotation, a re-rendered SND, or a rollback to a gate-less controller would
// reach the spec.networking==nil delete branch and delete live GitOps-owned
// networking. The per-pod P2P LB already had this guard; the external Service
// and HTTPRoutes did not — this exercises both.
func TestNetworking_Delete_SkipsOrphanedObjects(t *testing.T) {
	g := NewWithT(t)
	withP2PEndpointDomain(t, p2pEndpointTestDomain)
	ns := makeNamespace(t)

	snd := fixtures.NewSND(ns, "orphan-delete-guard",
		fixtures.WithReplicas(1),
		fixtures.WithNetworking(),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	extSvcKey := types.NamespacedName{Name: snd.Name + "-external", Namespace: ns}

	// Converge: external Service + 4 HTTPRoutes created and owned by the SND.
	waitFor(t, func() bool {
		svc := &corev1.Service{}
		if err := testCli.Get(testCtx, extSvcKey, svc); err != nil {
			return false
		}
		return metav1.IsControlledBy(svc, snd) && len(listHTTPRoutes(t, snd)) == 4
	}, "external Service + 4 HTTPRoutes owned by SND before orphan")

	// Orphan: stamp the annotation so the controller strips owner-refs from the
	// external Service + HTTPRoutes. The template-label edit bumps generation so
	// the generation-gated watch reticks promptly (orthogonal to the guard).
	latest := getSND(t, client.ObjectKeyFromObject(snd))
	patch := client.MergeFrom(latest.DeepCopy())
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	latest.Annotations[networkingOrphanedAnnotation] = "true"
	if latest.Spec.Template.Metadata == nil {
		latest.Spec.Template.Metadata = &seiv1alpha1.SeiNodeTemplateMeta{}
	}
	if latest.Spec.Template.Metadata.Labels == nil {
		latest.Spec.Template.Metadata.Labels = map[string]string{}
	}
	latest.Spec.Template.Metadata.Labels["test.sei.io/orphan-retick"] = "1"
	g.Expect(testCli.Patch(testCtx, latest, patch)).To(Succeed())

	// External Service owner-ref stripped — now GitOps-owned, still present.
	waitFor(t, func() bool {
		svc := &corev1.Service{}
		if err := testCli.Get(testCtx, extSvcKey, svc); err != nil {
			return false
		}
		return len(svc.GetOwnerReferences()) == 0
	}, "external Service owner-ref stripped after orphan")

	// The footgun: unset the annotation AND remove spec.networking together.
	// The gate is now off, so reconcileNetworking runs and reaches the
	// networking==nil delete branch — against objects the SND no longer owns.
	latest = getSND(t, client.ObjectKeyFromObject(snd))
	patch = client.MergeFrom(latest.DeepCopy())
	delete(latest.Annotations, networkingOrphanedAnnotation)
	latest.Spec.Networking = nil
	g.Expect(testCli.Patch(testCtx, latest, patch)).To(Succeed())

	// The delete path ran (condition flips to NetworkingDisabled)...
	waitForStatus(t, client.ObjectKeyFromObject(snd), func(l *seiv1alpha1.SeiNodeDeployment) bool {
		c := apimeta.FindStatusCondition(l.Status.Conditions, seiv1alpha1.ConditionNetworkingReady)
		return c != nil && c.Status == metav1.ConditionFalse && c.Reason == "NetworkingDisabled"
	}, "delete branch reached (ConditionNetworkingReady=False/NetworkingDisabled)")

	// ...but the orphaned objects SURVIVE: the delete helpers skip what the SND
	// no longer controls. Without the guard, the Service + routes vanish here.
	g.Consistently(func() bool {
		if err := testCli.Get(testCtx, extSvcKey, &corev1.Service{}); err != nil {
			return false
		}
		routes := &gatewayv1.HTTPRouteList{}
		if err := testCli.List(testCtx, routes, client.InNamespace(ns),
			client.MatchingLabels{"sei.io/nodedeployment": snd.Name}); err != nil {
			return false
		}
		return len(routes.Items) == 4
	}, 2*time.Second, 200*time.Millisecond).Should(BeTrue(),
		"orphaned external Service + HTTPRoutes survive the controller delete path")
}
