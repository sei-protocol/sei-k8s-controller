//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// TestResources_StampedOntoChildren asserts one field on the network sizes the
// whole validator pool: every controller-generated child carries the footprint
// on its own spec.resources, where it becomes that node's highest-precedence
// sizing source.
//
// Two replicas, because a single one cannot distinguish a per-child DeepCopy
// from one shared aliased ResourceList.
func TestResources_StampedOntoChildren(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	const replicas = 2
	network := fixtures.NewNetwork(ns, "res-stamp",
		fixtures.WithReplicas(replicas),
		fixtures.WithResources("4", "32Gi"))
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	key := client.ObjectKeyFromObject(network)

	g.Eventually(func(g Gomega) {
		kids := listChildren(t, getNetwork(t, key))
		g.Expect(kids).To(HaveLen(replicas))
		for i := range kids {
			res := kids[i].Spec.Resources
			g.Expect(res).NotTo(BeNil(), "child %s must carry the pool footprint", kids[i].Name)
			g.Expect(res.Requests[corev1.ResourceCPU]).To(Equal(resource.MustParse("4")))
			g.Expect(res.Requests[corev1.ResourceMemory]).To(Equal(resource.MustParse("32Gi")))
		}
	}, pollTimeout, pollInterval).Should(Succeed(),
		"every child SeiNode carries the network's spec.resources footprint")
}

// TestResources_UnsetLeavesChildrenUnset is the no-regression case: a network
// that sets no footprint leaves every child's spec.resources nil, so each stays
// on the app-config override or the per-mode code default.
func TestResources_UnsetLeavesChildrenUnset(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	network := fixtures.NewNetwork(ns, "res-unset")
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	key := client.ObjectKeyFromObject(network)

	g.Eventually(func(g Gomega) {
		kids := listChildren(t, getNetwork(t, key))
		g.Expect(kids).To(HaveLen(1))
		g.Expect(kids[0].Spec.Resources).To(BeNil())
	}, pollTimeout, pollInterval).Should(Succeed(),
		"a network with no footprint leaves its children on the lower sizing sources")
}

// TestResources_BareIntSurvivesControllerReencode reproduces the finalizer
// wedge a structural `==` create-only rule would cause. A network applied with a
// bare-integer footprint (a JSON number) stores an int in etcd; the network
// controller's finalizer Update, and every child-sync Update, re-encode it as a
// string. A structural `==` reads int != string and rejects the controller's own
// write, so the ceremony never starts. The quantity()-based rule compares values,
// so the re-encode is admitted. Mirrors the SeiNode-side case.
func TestResources_BareIntSurvivesControllerReencode(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	// memory as a bare JSON integer (32Gi in bytes) — the spelling only the
	// unstructured path can put on the wire; a typed Update re-encodes it.
	network := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "sei.io/v1alpha1",
		"kind":       "SeiNetwork",
		"metadata":   map[string]any{"name": "res-reencode", "namespace": ns},
		"spec": map[string]any{
			"image":    fixtures.DefaultImage,
			"replicas": int64(1),
			"genesis":  map[string]any{"chainId": fixtures.DefaultChainID},
			"resources": map[string]any{
				"requests": map[string]any{"cpu": int64(4), "memory": int64(34359738368)},
			},
		},
	}}
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())

	// A typed round-trip (an unrelated field) re-marshals the footprint as a
	// string; the create-only rule must treat that as unchanged.
	err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
		cur.Spec.Image = "ghcr.io/sei-protocol/seid:v2.0.0"
	})
	g.Expect(err).NotTo(HaveOccurred(),
		"a typed re-encode of a bare-int footprint must not trip the create-only rule (the finalizer Update depends on it)")
}

// TestResources_ImmutabilityGate asserts the spec-level CEL rule rejects
// post-creation mutation of spec.resources.
//
// The pool is fixed-shape at the genesis ceremony, and each child's own
// spec.resources is itself create-only — so ensureSeiNode deliberately does not
// sync this field, and an accepted edit here would be doubly inert. Admission
// rejects it rather than letting it read as applied. Mirrors the dataVolume,
// genesis and replicas gates.
func TestResources_ImmutabilityGate(t *testing.T) {
	t.Run("setting resources after create is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "res-add")
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.Resources = &seiv1alpha1.Resources{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("32Gi"),
				},
			}
		})
		g.Expect(err).To(HaveOccurred(), "adding spec.resources after create must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.resources is create-only"))
	})

	t.Run("raising an existing footprint is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "res-raise", fixtures.WithResources("4", "32Gi"))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("64")
		})
		g.Expect(err).To(HaveOccurred(), "raising spec.resources after create must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.resources is create-only"))
	})

	t.Run("clearing an existing footprint is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "res-clear", fixtures.WithResources("4", "32Gi"))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.Resources = nil
		})
		g.Expect(err).To(HaveOccurred(), "unsetting spec.resources after create must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.resources is create-only"))
	})

	t.Run("an unrelated spec edit still reaches the children", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "res-image-bump", fixtures.WithResources("4", "32Gi"))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())
		key := client.ObjectKeyFromObject(network)

		g.Eventually(func(g Gomega) {
			kids := listChildren(t, getNetwork(t, key))
			g.Expect(kids).To(HaveLen(1))
			g.Expect(kids[0].Spec.Resources).NotTo(BeNil())
		}, pollTimeout, pollInterval).Should(Succeed(), "child created with the pool footprint")

		const bumped = "ghcr.io/sei-protocol/seid:v2.0.0"
		err := updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.Image = bumped
		})
		g.Expect(err).NotTo(HaveOccurred(),
			"an image bump on a network with a footprint must still be admitted")

		// The load-bearing half. Parent and child are BOTH create-only on
		// resources, and ensureSeiNode's Update sends the child's whole spec —
		// footprint included. If either gate compared too broadly, admission
		// would reject that Update and wedge the ordinary image rollout for
		// every network that sets a footprint, with the failure surfacing as a
		// stuck rollout rather than as anything named "resources".
		g.Eventually(func(g Gomega) {
			kids := listChildren(t, getNetwork(t, key))
			g.Expect(kids).To(HaveLen(1))
			g.Expect(kids[0].Spec.Image).To(Equal(bumped),
				"the image bump must still propagate to a child that carries a footprint")
			g.Expect(kids[0].Spec.Resources).NotTo(BeNil(),
				"the child keeps its create-time footprint across the update")
		}, pollTimeout, pollInterval).Should(Succeed(),
			"a create-only footprint on both parent and child must not block image propagation")
	})
}
