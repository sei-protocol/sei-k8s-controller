//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// seedNode returns a seed whose identity comes from the named Secret. An empty
// secretName yields a seed with no pinned identity, which admission must reject.
func seedNode(ns, name, secretName string) *seiv1alpha1.SeiNode {
	spec := seiv1alpha1.SeiNodeSpec{
		ChainID: "envtest-1",
		Image:   "sei:latest",
		Seed:    &seiv1alpha1.SeedSpec{},
	}
	if secretName != "" {
		spec.Seed.NodeKey = seiv1alpha1.NodeKeySource{
			Secret: &seiv1alpha1.SecretNodeKeySource{SecretName: secretName},
		}
	}
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	}
}

func TestSeed_WithNodeKey_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)
	g.Expect(testCli.Create(testCtx, seedNode(ns, "seed-ok", "seed-0-node-key"))).To(Succeed())
}

// A seed's NodeID is published, so an unpinned identity is rejected at admission
// rather than left to regenerate onto the data volume at boot.
func TestSeed_WithoutNodeKey_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	err := testCli.Create(testCtx, seedNode(ns, "seed-no-key", ""))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("nodeKey"))
}

// The mode sub-specs stay mutually exclusive with seed added to the set.
func TestSeed_WithSecondMode_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := seedNode(ns, "seed-plus-full", "seed-0-node-key")
	node.Spec.FullNode = &seiv1alpha1.FullNodeSpec{}

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("exactly one of"))
}

// Widening the exactly-one rule must not reject the pre-existing modes.
// Replayer is omitted: CEL additionally requires peers for it.
func TestSeed_OtherModesStillAccepted(t *testing.T) {
	ns := makeNamespace(t)

	modes := map[string]func(*seiv1alpha1.SeiNodeSpec){
		"fullnode":  func(s *seiv1alpha1.SeiNodeSpec) { s.FullNode = &seiv1alpha1.FullNodeSpec{} },
		"archive":   func(s *seiv1alpha1.SeiNodeSpec) { s.Archive = &seiv1alpha1.ArchiveSpec{} },
		"validator": func(s *seiv1alpha1.SeiNodeSpec) { s.Validator = &seiv1alpha1.ValidatorSpec{} },
	}
	for name, set := range modes {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			spec := seiv1alpha1.SeiNodeSpec{ChainID: "envtest-1", Image: "sei:latest"}
			set(&spec)
			node := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "mode-" + name, Namespace: ns},
				Spec:       spec,
			}
			g.Expect(testCli.Create(testCtx, node)).To(Succeed())
		})
	}
}

// The Secret backs a published NodeID, so re-pointing it in place is refused;
// rotating the identity means delete-and-recreate.
func TestSeed_NodeKeySecretName_Immutable(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := seedNode(ns, "seed-immutable", "seed-0-node-key")
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())

	node.Spec.Seed.NodeKey.Secret.SecretName = "seed-0-node-key-v2"
	err := testCli.Update(testCtx, node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("immutable"))
}
