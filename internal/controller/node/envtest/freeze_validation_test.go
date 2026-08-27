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
// conflict. This suite starts no controller, so nothing writes concurrently
// today; the retry keeps the helper correct if one is ever added.
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

// FN-5: the guard targets one key. An unrelated override still works.
func TestFreeze_OtherOverrides_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := frozenFullNode(ns, "freeze-other-override", 100)
	node.Spec.Overrides = map[string]string{"logging.level": "info"}

	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
}

// A frozen node plus a halt key is a config seid refuses to load. Without this
// guard the manifest merges, Flux reports success, and the node wedges at
// config-apply with a JSON diagnostic.
func TestFreeze_WithHaltKeys_Rejected(t *testing.T) {
	for _, key := range []string{"chain.halt_height", "chain.halt_time"} {
		t.Run(key, func(t *testing.T) {
			g := NewWithT(t)
			ns := makeNamespace(t)

			node := frozenFullNode(ns, "freeze-halt", 100)
			node.Spec.Overrides = map[string]string{key: "500"}

			err := testCli.Create(testCtx, node)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("halt"))
		})
	}
}

// An unfrozen node may still set a halt key: the guard is conditional on freeze.
func TestFreeze_HaltKeysWithoutFreeze_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := frozenFullNode(ns, "halt-only", 0)
	node.Spec.Overrides = map[string]string{"chain.halt_height": "500"}

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

// FN-3, the half a field-level transition rule cannot express. A CEL rule using
// oldSelf is skipped when the path is absent from the stored object, so
// `self == oldSelf` on height permits ADDING freeze to an unfrozen node and
// REMOVING it from a frozen one. Both are unsafe, for different reasons:
//
//   - Adding it never reaches app.toml. Only the bootstrap path carries a
//     ConfigIntent, so a Running node keeps following the chain while its
//     readiness probe changes on the next reconcile. The node then presents as
//     frozen and behaves as an ordinary RPC node with weaker readiness.
//   - Removing it leaves freeze-height in app.toml while the probe reverts to
//     /lag_status, so the node goes NotReady permanently at the next pod
//     replacement.
//
// The presence rule lives on the mode sub-spec, where has() is observable on
// both sides of the transition.
func TestFreeze_PresenceIsCreateOnly(t *testing.T) {
	t.Run("adding freeze to an existing node is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		node := frozenFullNode(ns, "freeze-add", 0)
		g.Expect(testCli.Create(testCtx, node)).To(Succeed())

		err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
			cur.Spec.FullNode.Freeze = &seiv1alpha1.FreezeSpec{Height: 100}
		})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("create-only"))
	})

	t.Run("removing freeze from an existing node is rejected", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		node := frozenFullNode(ns, "freeze-remove", 100)
		g.Expect(testCli.Create(testCtx, node)).To(Succeed())

		err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
			cur.Spec.FullNode.Freeze = nil
		})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("create-only"))
	})

	t.Run("archive is guarded the same way", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		node := frozenArchiveNode(ns, "freeze-add-archive", 0)
		g.Expect(testCli.Create(testCtx, node)).To(Succeed())

		err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
			cur.Spec.Archive.Freeze = &seiv1alpha1.FreezeSpec{Height: 100}
		})
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("create-only"))
	})

	t.Run("an unrelated spec edit on a frozen node still succeeds", func(t *testing.T) {
		g := NewWithT(t)
		ns := makeNamespace(t)

		node := frozenFullNode(ns, "freeze-edit-ok", 100)
		g.Expect(testCli.Create(testCtx, node)).To(Succeed())

		err := updateNodeWithRetry(t, client.ObjectKeyFromObject(node), func(cur *seiv1alpha1.SeiNode) {
			cur.Spec.Image = "sei:next"
		})
		g.Expect(err).NotTo(HaveOccurred(), "an image bump must not be collateral damage")
	})
}

// FN-1 with a seid-fatal combination the CRD must catch: seid refuses to start
// once a store has already reached the freeze height, so a snapshot target at
// or above it is a permanent CrashLoopBackOff.
func TestFreeze_SnapshotTargetAtOrAboveHeight_Rejected(t *testing.T) {
	for name, target := range map[string]int64{"equal": 100, "above": 200} {
		t.Run(name, func(t *testing.T) {
			g := NewWithT(t)
			ns := makeNamespace(t)

			node := frozenFullNode(ns, "freeze-snap-"+name, 100)
			node.Spec.FullNode.Snapshot = &seiv1alpha1.SnapshotSource{
				S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: target},
			}

			err := testCli.Create(testCtx, node)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("targetHeight"))
		})
	}
}

// A snapshot target below the freeze height is the supported bootstrap: the node
// restores, block-syncs the remainder, and stops.
func TestFreeze_SnapshotTargetBelowHeight_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := frozenFullNode(ns, "freeze-snap-below", 100)
	node.Spec.FullNode.Snapshot = &seiv1alpha1.SnapshotSource{
		S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 50},
	}

	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
}

// seid disables state sync under freeze and falls back to block sync from
// genesis, silently. An operator who asks for a fast bootstrap would instead get
// a multi-week one with nothing saying why.
func TestFreeze_WithStateSync_Rejected(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	node := frozenFullNode(ns, "freeze-statesync", 100)
	node.Spec.FullNode.Snapshot = &seiv1alpha1.SnapshotSource{
		StateSync: &seiv1alpha1.StateSyncSource{},
	}

	err := testCli.Create(testCtx, node)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("stateSync"))
}
