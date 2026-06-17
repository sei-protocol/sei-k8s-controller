//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"

	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// TestDataVolume_ImmutabilityGate asserts the spec-level CEL rule rejects
// post-creation mutation of spec.dataVolume. It backs a StatefulSet
// volumeClaimTemplate, which Kubernetes forbids changing after create — a
// later edit could never take effect, so admission rejects it rather than
// letting the controller silently ignore it.
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
		g.Expect(err.Error()).To(ContainSubstring("spec.dataVolume is immutable"))
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
		g.Expect(err.Error()).To(ContainSubstring("spec.dataVolume is immutable"))
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
