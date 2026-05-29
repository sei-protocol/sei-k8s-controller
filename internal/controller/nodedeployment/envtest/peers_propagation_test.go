//go:build envtest

package envtest_test

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/controller/nodedeployment/envtest/fixtures"
)

// Live edit to SND.spec.template.spec.peers must reach the already-created
// child. Regression for the silent-allow-list gap in ensureSeiNode.
func TestPeersPropagation_AddSourceOnLiveSND(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	initial := []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{
			Region: "eu-central-1",
			Tags:   map[string]string{"ChainIdentifier": "pacific-1", "Component": "state-syncer"},
		}},
	}
	snd := fixtures.NewSND(ns, "peers-add",
		fixtures.WithReplicas(1),
		fixtures.WithPeers(initial...),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return len(child.Spec.Peers) == 1
	}, "child converged with initial peer set")

	// Live edit: add a LabelPeerSource (the platform#759 shape).
	g.Eventually(func() error {
		latest := getSND(t, client.ObjectKeyFromObject(snd))
		patch := client.MergeFrom(latest.DeepCopy())
		latest.Spec.Template.Spec.Peers = append(latest.Spec.Template.Spec.Peers,
			seiv1alpha1.PeerSource{Label: &seiv1alpha1.LabelPeerSource{
				Namespace: ns,
				Selector:  map[string]string{"sei.io/chain": "pacific-1"},
			}})
		return testCli.Patch(testCtx, latest, patch)
	}, 5*time.Second, 200*time.Millisecond).Should(Succeed())

	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		if len(child.Spec.Peers) != 2 {
			return false
		}
		return child.Spec.Peers[1].Label != nil &&
			child.Spec.Peers[1].Label.Selector["sei.io/chain"] == "pacific-1"
	}, "child Spec.Peers reflected the live SND peer-list addition")
}

// Removing a peer source on a live SND must propagate (shrink case).
func TestPeersPropagation_RemoveSourceOnLiveSND(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	initial := []seiv1alpha1.PeerSource{
		{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-central-1", Tags: map[string]string{"k": "v"}}},
		{Label: &seiv1alpha1.LabelPeerSource{Namespace: ns, Selector: map[string]string{"k": "v"}}},
	}
	snd := fixtures.NewSND(ns, "peers-remove",
		fixtures.WithReplicas(1),
		fixtures.WithPeers(initial...),
	)
	g.Expect(testCli.Create(testCtx, snd)).To(Succeed())

	childKey := types.NamespacedName{Name: snd.Name + "-0", Namespace: ns}
	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return len(child.Spec.Peers) == 2
	}, "child converged with initial 2-source peer set")

	g.Eventually(func() error {
		latest := getSND(t, client.ObjectKeyFromObject(snd))
		patch := client.MergeFrom(latest.DeepCopy())
		latest.Spec.Template.Spec.Peers = latest.Spec.Template.Spec.Peers[:1]
		return testCli.Patch(testCtx, latest, patch)
	}, 5*time.Second, 200*time.Millisecond).Should(Succeed())

	waitFor(t, func() bool {
		child := &seiv1alpha1.SeiNode{}
		if err := testCli.Get(testCtx, childKey, child); err != nil {
			return false
		}
		return len(child.Spec.Peers) == 1 && child.Spec.Peers[0].EC2Tags != nil
	}, "child Spec.Peers shrunk to match the live SND")
}
