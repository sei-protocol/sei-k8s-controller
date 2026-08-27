//go:build envtest

package envtest_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// Covers spec freeze-node: FN-1, FN-2, FN-3, FN-5, FN-6.
//
// These cases need no controller. The API server alone accepts the valid
// shapes and rejects each invalid one, which is the spec's Independent Test for
// the CRD-contract group.

// frozenFullNode returns a full node held at height. A height of 0 leaves
// freeze unset, which admission must accept as an ordinary full node.
func frozenFullNode(ns, name string, height int64) *seiv1alpha1.SeiNode {
	spec := seiv1alpha1.SeiNodeSpec{
		ChainID:  "envtest-1",
		Image:    "sei:latest",
		FullNode: &seiv1alpha1.FullNodeSpec{},
	}
	if height != 0 {
		spec.FullNode.Freeze = &seiv1alpha1.FreezeSpec{Height: height}
	}
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	}
}

// frozenArchiveNode returns an archive node held at height.
func frozenArchiveNode(ns, name string, height int64) *seiv1alpha1.SeiNode {
	spec := seiv1alpha1.SeiNodeSpec{
		ChainID: "envtest-1",
		Image:   "sei:latest",
		Archive: &seiv1alpha1.ArchiveSpec{},
	}
	if height != 0 {
		spec.Archive.Freeze = &seiv1alpha1.FreezeSpec{Height: height}
	}
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	}
}

// updateNodeWithRetry re-fetches and re-applies mutate on a resourceVersion
// conflict, so a concurrent status write cannot lose the 409 race before CEL
// validation fires.
func updateNodeWithRetry(t *testing.T, key client.ObjectKey, mutate func(*seiv1alpha1.SeiNode)) error {
	t.Helper()
	var lastErr error
	for range 10 {
		cur := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, key, cur); err != nil {
			return err
		}
		mutate(cur)
		err := testCli.Update(testCtx, cur)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	return lastErr
}

// FN-1, FN-2: both RPC-serving modes carry freeze, and a height of at least 1
// is accepted.
func TestFreeze_OnFullNodeAndArchive_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	g.Expect(testCli.Create(testCtx, frozenFullNode(ns, "freeze-full", 100))).
		To(Succeed(), "a frozen full node must be accepted")
	g.Expect(testCli.Create(testCtx, frozenArchiveNode(ns, "freeze-archive", 100))).
		To(Succeed(), "a frozen archive node must be accepted")
}

// FN-2: the minimum guards against a freeze at height 0, which seid treats as
// "not frozen" — an operator who writes 0 means something else.
func TestFreeze_HeightZero_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := frozenFullNode(ns, "freeze-zero", 1)
	node.Spec.FullNode.Freeze.Height = 0

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred(), "freeze.height of 0 must be rejected")
}

// FN-3: the node has already stopped at its height, so a change cannot take
// effect without a rebuild. Admission rejects it rather than leaving a spec
// that disagrees with the running node.
func TestFreeze_HeightImmutable(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := frozenFullNode(ns, "freeze-immutable", 100)
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
	key := client.ObjectKeyFromObject(node)

	t.Run("raising the height is rejected", func(t *testing.T) {
		g := NewWithT(t)
		err := updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
			cur.Spec.FullNode.Freeze.Height = 200
		})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("immutable"))
	})

	t.Run("lowering the height is rejected", func(t *testing.T) {
		g := NewWithT(t)
		err := updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
			cur.Spec.FullNode.Freeze.Height = 50
		})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("immutable"))
	})
}

// FN-5: mergeOverrides copies user overrides last, so a user key would outrank
// the controller-derived one and leave the probe disagreeing with the config.
// Admission rejects the key instead.
func TestFreeze_HeightInOverrides_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := frozenFullNode(ns, "freeze-override", 0)
	node.Spec.Overrides = map[string]string{"chain.freeze_height": "100"}

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred(), "chain.freeze_height in overrides must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("not overrides"))
}

// FN-5: the guard targets one key. Every other override still works.
func TestFreeze_OtherOverrides_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := frozenFullNode(ns, "freeze-other-override", 100)
	node.Spec.Overrides = map[string]string{"chain.halt_time": "0"}

	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
}

// FN-6: snapshot generation produces snapshots from new blocks, and a frozen
// node has none. The two fields together describe nothing coherent.
func TestFreeze_WithSnapshotGeneration_Rejected(t *testing.T) {
	generation := &seiv1alpha1.SnapshotGenerationConfig{
		Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{KeepRecent: 2},
	}

	t.Run("fullNode", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		node := frozenFullNode(ns, "freeze-snapgen-full", 100)
		node.Spec.FullNode.SnapshotGeneration = generation

		err := testCli.Create(testCtx, node)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
	})

	t.Run("archive", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		node := frozenArchiveNode(ns, "freeze-snapgen-archive", 100)
		node.Spec.Archive.SnapshotGeneration = generation

		err := testCli.Create(testCtx, node)
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
	})
}

// FN-6: snapshot generation on its own is untouched by the new rule.
func TestFreeze_SnapshotGenerationWithoutFreeze_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := frozenArchiveNode(ns, "snapgen-only", 0)
	node.Spec.Archive.SnapshotGeneration = &seiv1alpha1.SnapshotGenerationConfig{
		Tendermint: &seiv1alpha1.TendermintSnapshotGenerationConfig{KeepRecent: 2},
	}

	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
}
