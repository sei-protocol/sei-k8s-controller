//go:build envtest

package envtest_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/seinetwork/envtest/fixtures"
)

// Peers on a child SeiNode are controller-owned: the genesis ceremony's
// collect-and-set-peers task patches each child's Spec.Peers with the
// assembled validator set. generateSeiNode emits empty peers at create, so a
// subsequent network reconcile MUST NOT clobber those controller writes.
//
// This drives the invariant against a real apiserver: after the controller
// converges the children, we simulate the ceremony's peer write, then force a
// network reconcile (an in-place configOverrides edit) and assert the
// controller-set peers survive.
func TestPeers_ControllerSetSurviveReconcile(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	network := fixtures.NewNetwork(ns, "peers-survive", fixtures.WithReplicas(1))
	g.Expect(testCli.Create(testCtx, network)).To(Succeed())
	key := client.ObjectKeyFromObject(network)

	childKey := types.NamespacedName{Name: network.Name + "-0", Namespace: ns}
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		return testCli.Get(testCtx, childKey, child) == nil
	}, "child SeiNode created")

	// Simulate the collect-and-set-peers ceremony task writing the assembled
	// validator peer set onto the child.
	peer := "stub-node-id@peers-survive-0.peers-survive-0." + ns + ".svc.cluster.local:26656"
	g.Eventually(func() error {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return err
		}
		patch := client.MergeFrom(child.DeepCopy())
		child.Spec.Peers = []seiv1alpha1.PeerSource{
			{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{peer}}},
		}
		return testCli.Patch(testCtx, child, patch)
	}, 5*time.Second, pollInterval).Should(Succeed())

	// Force a network reconcile via an in-place configOverrides edit (a
	// non-deployment field that propagates through ensureSeiNode).
	g.Expect(updateNetworkWithRetry(t, key, func(cur *seiv1alpha1.SeiNetwork) {
		cur.Spec.ConfigOverrides = map[string]string{"evm.http_port": "8545"}
	})).To(Succeed())

	// The override must reach the child AND the controller-set peers must
	// remain intact across the reconcile.
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return child.Spec.Overrides["evm.http_port"] == "8545" &&
			len(child.Spec.Peers) == 1 &&
			child.Spec.Peers[0].Static != nil
	}, "config override propagated and controller-set peers survived reconcile")

	// Belt-and-suspenders: peers stay put across several more reconcile laps.
	g.Consistently(func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return len(child.Spec.Peers) == 1 && child.Spec.Peers[0].Static != nil
	}, 3*time.Second, pollInterval).Should(BeTrue(),
		"controller-set peers must not oscillate or be cleared")
}
