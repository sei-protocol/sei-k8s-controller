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

// TestDataVolume_ImmutabilityGate asserts the create-only gate on
// spec.dataVolume: a change, an unset, and a first-time set are all rejected.
//
// Each validator's data PVC is created once (ensure-data-pvc is Get-then-Create
// with no update path) and nothing replaces a node on storage drift, so a later
// edit could never take effect; admission rejects it rather than letting the
// controller silently ignore it.
//
// The gate is split across levels — presence parity on SeiNetworkSpec, values on
// the shared DataVolumeImport/DataVolumeStorage types — so the rejection message
// depends on which half fires. The subtests assert the message they should get,
// which is also how they document the split.
func TestDataVolume_ImmutabilityGate(t *testing.T) {
	t.Run("setting dataVolume after create is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		// Create with no dataVolume, then try to add one.
		network := fixtures.NewNetwork(ns, "dv-add")
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.DataVolume = &seiv1alpha1.DataVolumeSpec{
				Import: &seiv1alpha1.DataVolumeImport{PVCName: "imported-pvc"},
			}
		})
		g.Expect(err).To(HaveOccurred(), "adding spec.dataVolume after create must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.dataVolume is create-only"))
	})

	t.Run("mutating an existing dataVolume is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "dv-mutate", fixtures.WithDataVolumeImport("original-pvc"))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.DataVolume.Import.PVCName = "different-pvc"
		})
		g.Expect(err).To(HaveOccurred(), "changing spec.dataVolume after create must be rejected")
		// pvcName is pinned by its own field-level rule on the shared
		// DataVolumeImport type, not by the spec-level presence parity, so this
		// is the message that surfaces.
		g.Expect(err.Error()).To(ContainSubstring("pvcName is immutable"))
	})

	t.Run("create without dataVolume and never touching it is allowed", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "dv-absent")
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		// A write that leaves dataVolume absent (still unset) must pass the
		// has()-guarded equality branch.
		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.Paused = true
		})
		g.Expect(err).NotTo(HaveOccurred(),
			"editing other fields while dataVolume stays absent must succeed")
	})
}

// TestDataVolume_SizeCreateOnly covers the storage half of the gate: the size a
// pool's volumes are provisioned at is fixed for the network's life.
func TestDataVolume_SizeCreateOnly(t *testing.T) {
	t.Run("changing the size is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "dv-size-change", fixtures.WithDataVolumeStorage("500Gi"))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.DataVolume.Storage.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("2Ti")
		})
		g.Expect(err).To(HaveOccurred(), "resizing a pool's volumes after create must be rejected")
		// A size CHANGE is caught by the shared DataVolumeStorage value rule, whose
		// message is Kind-neutral — assert its specific text so a regression to the
		// old node-only wording (wrong remedy for a network operator) is caught.
		g.Expect(err.Error()).To(ContainSubstring("recreate the owning resource"))
	})

	t.Run("adding a size to an existing network is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "dv-size-add")
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			fixtures.WithDataVolumeStorage("500Gi")(cur)
		})
		g.Expect(err).To(HaveOccurred(), "adding a size after create must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.dataVolume is create-only"))
	})

	t.Run("removing dataVolume from an existing network is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "dv-size-remove", fixtures.WithDataVolumeStorage("500Gi"))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		// Presence parity at spec level catches the whole-block removal a sub-type
		// value rule would miss (present -> absent, so the sub-type rule never fires).
		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.DataVolume = nil
		})
		g.Expect(err).To(HaveOccurred(), "removing dataVolume after create must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.dataVolume is create-only"))
	})
}

// TestDataVolume_BareIntSizeSurvivesControllerReencode is the case that forced
// the rewrite of the spec.dataVolume rule, and it fails against the structural
// `self.dataVolume == oldSelf.dataVolume` this replaced.
//
// A network applied with a bare-integer size stores an int in etcd. The network
// controller's finalizer Update, and every child-sync Update, re-encode it as a
// string. A structural == reads int != string, rejects the controller's own
// write, and the ceremony never starts. Presence parity plus a quantity()
// comparison one level down compares values, so the re-encode is admitted.
// Mirrors the compute-side case in resources_test.go.
func TestDataVolume_BareIntSizeSurvivesControllerReencode(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	// 500Gi in bytes as a bare JSON integer — the spelling only the unstructured
	// path can put on the wire.
	network := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "sei.io/v1alpha1",
		"kind":       "SeiNetwork",
		"metadata":   map[string]any{"name": "dv-reencode", "namespace": ns},
		"spec": map[string]any{
			"image":    fixtures.DefaultImage,
			"replicas": int64(1),
			"genesis":  map[string]any{"chainId": fixtures.DefaultChainID},
			"dataVolume": map[string]any{
				"storage": map[string]any{
					"resources": map[string]any{
						"requests": map[string]any{"storage": int64(536870912000)},
					},
				},
			},
		},
	}}
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())

	const bumped = "ghcr.io/sei-protocol/seid:v2.0.1"
	key := client.ObjectKeyFromObject(network)
	err := updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
		cur.Spec.Image = bumped
	})
	g.Expect(err).NotTo(HaveOccurred(),
		"a typed re-encode of a bare-int size must not trip the create-only rule (the finalizer Update depends on it)")

	// And the controller's own writes must land: a child appears and takes the
	// bumped image. This is the half that proves no wedge, rather than only that
	// one hand-rolled Update was admitted.
	g.Eventually(func(g Gomega) {
		kids := listChildren(t, getNetwork(t, key))
		g.Expect(kids).To(HaveLen(1))
		g.Expect(kids[0].Spec.Image).To(Equal(bumped))
	}, pollTimeout, pollInterval).Should(Succeed(),
		"the controller must be able to create and sync children carrying a bare-int size")
}

// TestDataVolume_SizeStampedOntoChildren asserts the pool's size reaches every
// child, and that a child carrying it can still be synced.
//
// Two replicas, because one cannot distinguish a per-child DeepCopy from a
// shared aliased claim. The image bump at the end is the load-bearing half: the
// child's own spec.dataVolume.storage size is create-only too, and
// ensureSeiNode's Update sends the child's whole spec, so a gate that compared
// too broadly on either side would reject that Update and wedge the ordinary
// image rollout for every network that sets a size.
func TestDataVolume_SizeStampedOntoChildren(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	const replicas = 2
	network := fixtures.NewNetwork(ns, "dv-size-children",
		fixtures.WithReplicas(replicas),
		fixtures.WithDataVolumeStorage("500Gi"))
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	key := client.ObjectKeyFromObject(network)

	g.Eventually(func(g Gomega) {
		kids := listChildren(t, getNetwork(t, key))
		g.Expect(kids).To(HaveLen(replicas))
		for i := range kids {
			dv := kids[i].Spec.DataVolume
			g.Expect(dv).NotTo(BeNil(), "child %s must carry the pool's data volume", kids[i].Name)
			g.Expect(dv.Storage).NotTo(BeNil())
			got := dv.Storage.Resources.Requests[corev1.ResourceStorage]
			g.Expect(got.String()).To(Equal("500Gi"))
		}
	}, pollTimeout, pollInterval).Should(Succeed(),
		"every child SeiNode carries the network's data volume size")

	const bumped = "ghcr.io/sei-protocol/seid:v3.0.0"
	g.Expect(updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
		cur.Spec.Image = bumped
	})).To(Succeed())

	g.Eventually(func(g Gomega) {
		kids := listChildren(t, getNetwork(t, key))
		g.Expect(kids).To(HaveLen(replicas))
		for i := range kids {
			g.Expect(kids[i].Spec.Image).To(Equal(bumped),
				"the image bump must reach a child that carries a create-only size")
			g.Expect(kids[i].Spec.DataVolume.Storage).NotTo(BeNil(),
				"the child keeps its create-time size across the sync")
		}
	}, pollTimeout, pollInterval).Should(Succeed(),
		"a create-only size on both parent and child must not block image propagation")
}
