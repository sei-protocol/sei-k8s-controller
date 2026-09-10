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

// The create-only gate on spec.dataVolume: change, unset and first-time set all
// rejected. Split across levels, so each subtest asserts its half's message.
func TestDataVolume_ImmutabilityGate(t *testing.T) {
	t.Run("setting dataVolume after create is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

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
		// pvcName is pinned by its own rule on DataVolumeImport.
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

// The storage half: a pool's provisioned size is fixed for the network's life.
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
		// The shared value rule's text is Kind-neutral; node-only wording regresses.
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

		// Spec-level presence catches a removal the sub-type rule never fires on.
		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.DataVolume = nil
		})
		g.Expect(err).To(HaveOccurred(), "removing dataVolume after create must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.dataVolume is create-only"))
	})
}

// The case that forced the rewrite: the finalizer and child-sync Updates
// re-encode a bare-int size as a string, which the structural == rejected.
func TestDataVolume_BareIntSizeSurvivesControllerReencode(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

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

	// The controller's own writes must land — this is what proves no wedge.
	g.Eventually(func(g Gomega) {
		kids := listChildren(t, getNetwork(t, key))
		g.Expect(kids).To(HaveLen(1))
		g.Expect(kids[0].Spec.Image).To(Equal(bumped))
	}, pollTimeout, pollInterval).Should(Succeed(),
		"the controller must be able to create and sync children carrying a bare-int size")
}

// The pool's size reaches every child (two replicas, so an aliased claim shows).
// The image bump matters: the child's size is create-only too and ensureSeiNode
// sends its whole spec, so an over-broad gate would wedge rollout.
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

// The performance half: the pool's VolumeAttributesClass selection is fixed for
// the network's life. DR-001 requires a change, an unset AND a first-time set to
// be rejected, which needs the value rule on the shared sub-type and the
// presence term in this Kind's enumerated rule working together.
func TestDataVolume_VACCreateOnly(t *testing.T) {
	t.Run("changing the VAC is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "dv-vac-change", fixtures.WithDataVolumeVAC("vac-beta"))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			name := "vac-alpha"
			cur.Spec.DataVolume.Storage.VolumeAttributesClassName = &name
		})
		g.Expect(err).To(HaveOccurred(), "reselecting a pool's storage performance must be rejected")
		// The shared value rule's text is Kind-neutral; node-only wording regresses.
		g.Expect(err.Error()).To(ContainSubstring("recreate the owning resource"))
	})

	t.Run("adding a VAC to an existing network is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "dv-vac-add")
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			fixtures.WithDataVolumeVAC("vac-alpha")(cur)
		})
		g.Expect(err).To(HaveOccurred(), "a first-time set must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.dataVolume is create-only"))
	})

	// The COMPLETENESS trap this record documents: dataVolume and storage are
	// both already present and the size never changes, so every pre-existing
	// presence term holds. Only the VAC's own term catches the edit.
	t.Run("adding a VAC beside an unchanged size is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "dv-vac-sneak", fixtures.WithDataVolumeStorage("500Gi"))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			name := "vac-alpha"
			cur.Spec.DataVolume.Storage.VolumeAttributesClassName = &name
		})
		g.Expect(err).To(HaveOccurred(),
			"a VAC selection must not be silently mutable beside an unchanged size")
		g.Expect(err.Error()).To(ContainSubstring("spec.dataVolume is create-only"))
	})

	t.Run("removing the VAC is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "dv-vac-remove",
			fixtures.WithDataVolumeStorage("500Gi"), fixtures.WithDataVolumeVAC("vac-alpha"))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		// Presence catches an unset the sub-type value rule never fires on.
		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.DataVolume.Storage.VolumeAttributesClassName = nil
		})
		g.Expect(err).To(HaveOccurred(), "unsetting after create must be rejected")
		g.Expect(err.Error()).To(ContainSubstring("spec.dataVolume is create-only"))
	})

	t.Run("an unrelated edit is still accepted", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		network := fixtures.NewNetwork(ns, "dv-vac-unrelated", fixtures.WithDataVolumeVAC("vac-alpha"))
		g.Expect(testCli.Create(testCtx, network)).To(Succeed())

		err := updateNetworkWithRetry(t, client.ObjectKeyFromObject(network), func(cur *seiv1alpha1.SeiNetwork) {
			cur.Spec.Paused = true
		})
		g.Expect(err).NotTo(HaveOccurred(),
			"an edit leaving the selection unchanged must be accepted — the controller's own writes depend on it")
	})
}

// The pool's selection reaches every child, and keeps reaching them across an
// image bump: the child's VAC is create-only too and ensureSeiNode sends the
// whole spec, so an over-broad gate on either Kind would wedge rollout.
func TestDataVolume_VACStampedOntoChildren(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	// The pre-flight holds provisioning until the class exists, so the pool
	// cannot reach Running (and the image cannot propagate) without it. The
	// parameters are opaque placeholders: the controller never reads them.
	ensureVolumeAttributesClass(t, "vac-children")

	const replicas = 2
	network := fixtures.NewNetwork(ns, "dv-vac-children",
		fixtures.WithReplicas(replicas),
		fixtures.WithDataVolumeStorage("500Gi"),
		fixtures.WithDataVolumeVAC("vac-children"))
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	key := client.ObjectKeyFromObject(network)

	g.Eventually(func(g Gomega) {
		kids := listChildren(t, getNetwork(t, key))
		g.Expect(kids).To(HaveLen(replicas))
		for i := range kids {
			dv := kids[i].Spec.DataVolume
			g.Expect(dv).NotTo(BeNil(), "child %s must carry the pool's data volume", kids[i].Name)
			g.Expect(dv.Storage).NotTo(BeNil())
			g.Expect(dv.Storage.VolumeAttributesClassName).NotTo(BeNil())
			g.Expect(*dv.Storage.VolumeAttributesClassName).To(Equal("vac-children"))
		}
	}, pollTimeout, pollInterval).Should(Succeed(),
		"every child SeiNode carries the network's VolumeAttributesClass selection")

	const bumped = "ghcr.io/sei-protocol/seid:v3.1.0"
	g.Expect(updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
		cur.Spec.Image = bumped
	})).To(Succeed())

	g.Eventually(func(g Gomega) {
		kids := listChildren(t, getNetwork(t, key))
		g.Expect(kids).To(HaveLen(replicas))
		for i := range kids {
			g.Expect(kids[i].Spec.Image).To(Equal(bumped),
				"the image bump must reach a child that carries a create-only VAC selection")
			g.Expect(kids[i].Spec.DataVolume.Storage.VolumeAttributesClassName).NotTo(BeNil(),
				"the child keeps its create-time selection across the sync")
		}
	}, pollTimeout, pollInterval).Should(Succeed(),
		"a create-only VAC on both parent and child must not block image propagation")
}
