//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// roleLabel is the observability/selection identity the dropped SeiNodeTemplate
// used to carry. A SeiNetwork is always a validator pool, so children carry
// sei.io/role=validator so role-filtering GitOps/selectors resolve.
const (
	roleLabel     = "sei.io/role"
	roleValidator = "validator"
)

// TestChild_RoleLabelStamped asserts every controller-generated child SeiNode
// carries sei.io/role=validator on its metadata. The scoped genesis spec
// dropped the SeiNodeTemplate that used to source this label, so the controller
// must restamp it (a SeiNetwork is always a validator pool).
func TestChild_RoleLabelStamped(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	const replicas = 2
	network := fixtures.NewNetwork(ns, "role-label", fixtures.WithReplicas(replicas))
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	key := client.ObjectKeyFromObject(network)

	g.Eventually(func(g Gomega) {
		kids := listChildren(t, getNetwork(t, key))
		g.Expect(kids).To(HaveLen(replicas))
		for i := range kids {
			g.Expect(kids[i].Labels).To(HaveKeyWithValue(roleLabel, roleValidator),
				"child SeiNode metadata must carry sei.io/role=validator")
		}
	}, pollTimeout, pollInterval).Should(Succeed(),
		"every child SeiNode is stamped sei.io/role=validator")
}

// TestChild_SidecarResourcesPropagate asserts a spec.sidecar.resources change
// on a live network reaches the existing children. ensureSeiNode syncs the
// whole Sidecar struct (not just image/port), so a resource bump propagates.
func TestChild_SidecarResourcesPropagate(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	network := fixtures.NewNetwork(ns, "sidecar-res",
		fixtures.WithSidecar(&seiv1alpha1.SidecarConfig{Port: 7777}),
	)
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	key := client.ObjectKeyFromObject(network)

	// Child exists with a sidecar but no resources set.
	g.Eventually(func(g Gomega) {
		kids := listChildren(t, getNetwork(t, key))
		g.Expect(kids).To(HaveLen(1))
		g.Expect(kids[0].Spec.Sidecar).NotTo(BeNil())
		g.Expect(kids[0].Spec.Sidecar.Resources).To(BeNil())
	}, pollTimeout, pollInterval).Should(Succeed(),
		"child created with a sidecar and no resources")

	// Patch spec.sidecar.resources.
	cur := getNetwork(t, key)
	patch := client.MergeFrom(cur.DeepCopy())
	cur.Spec.Sidecar.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	g.Expect(testCli.Patch(testCtx, cur, patch)).To(Succeed())

	// Resources propagate to the child in-place (whole Sidecar struct synced).
	g.Eventually(func(g Gomega) {
		kids := listChildren(t, getNetwork(t, key))
		g.Expect(kids).To(HaveLen(1))
		res := kids[0].Spec.Sidecar.Resources
		g.Expect(res).NotTo(BeNil(), "sidecar resources must propagate to the child")
		g.Expect(res.Requests.Memory().String()).To(Equal("256Mi"))
		g.Expect(res.Limits.Memory().String()).To(Equal("512Mi"))
	}, pollTimeout, pollInterval).Should(Succeed(),
		"spec.sidecar.resources change propagates to every child")
}
