//go:build envtest

package envtest_test

import (
	"testing"

	. "github.com/onsi/gomega"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// Admission-level coverage of spec.nodeConfig: which shapes the API server
// accepts, and which it rejects.
//
// These cases need no controller. A failure here is a CRD-contract defect and
// never a reconcile bug.

func nodeConfigNode(ns, name string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "envtest-1",
			Image:    "sei:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{},
			NodeConfig: &seiv1alpha1.NodeConfig{
				ConfigRef: seiv1alpha1.ConfigFileRef{Name: "rpc-config-v1"},
				AppRef:    seiv1alpha1.ConfigFileRef{Name: "rpc-app-v1"},
			},
		},
	}
}

func TestNodeConfig_BothRefs_Accepted(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	g.Expect(testCli.Create(testCtx, nodeConfigNode(ns, "nc-both"))).To(Succeed())

	// One ConfigMap carrying both keys is the common case and equally valid.
	same := nodeConfigNode(ns, "nc-same")
	same.Spec.NodeConfig.AppRef.Name = same.Spec.NodeConfig.ConfigRef.Name
	g.Expect(testCli.Create(testCtx, same)).To(Succeed())
}

// Both files are required. A node taking config.toml from a ConfigMap and
// app.toml from the controller would have two owners of one directory, and the
// controller's writer would detach the mount delivering the other file.
func TestNodeConfig_OneRefMissing_Rejected(t *testing.T) {
	cases := []struct {
		name  string
		clear func(*seiv1alpha1.NodeConfig)
	}{
		{"appRef missing", func(c *seiv1alpha1.NodeConfig) { c.AppRef = seiv1alpha1.ConfigFileRef{} }},
		{"configRef missing", func(c *seiv1alpha1.NodeConfig) { c.ConfigRef = seiv1alpha1.ConfigFileRef{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ns := makeNamespace(t)

			node := nodeConfigNode(ns, "nc-partial")
			tc.clear(node.Spec.NodeConfig)

			g.Expect(testCli.Create(testCtx, node)).To(HaveOccurred(),
				"nodeConfig must name both config.toml and app.toml")
		})
	}
}

// Each of these reached config.toml or app.toml through a task a node with
// spec.nodeConfig does not run, so accepting the pair would report success on
// an edit that never reached seid. peers is the sharpest: status.resolvedPeers
// would keep updating and keep looking correct.
func TestNodeConfig_WithControllerManagedConfig_Rejected(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*seiv1alpha1.SeiNode)
		wantMsg string
	}{
		{"with configValues", func(n *seiv1alpha1.SeiNode) {
			n.Spec.ConfigValues = []seiv1alpha1.ConfigValue{{
				FileName: "config.toml",
				Key:      "p2p.persistent-peers",
				Value:    apiextensionsv1.JSON{Raw: []byte(`"peer@host:26656"`)},
			}}
		}, "configValues"},
		{"with overrides", func(n *seiv1alpha1.SeiNode) {
			n.Spec.Overrides = map[string]string{"logging.level": "debug"}
		}, "overrides"},
		{"with peers", func(n *seiv1alpha1.SeiNode) {
			n.Spec.Peers = []seiv1alpha1.PeerSource{{
				Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"peer@host:26656"}},
			}}
		}, "peers"},
		{"with externalAddress", func(n *seiv1alpha1.SeiNode) {
			n.Spec.ExternalAddress = "node.example:26656"
		}, "externalAddress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ns := makeNamespace(t)

			node := nodeConfigNode(ns, "nc-conflict")
			tc.mutate(node)

			err := testCli.Create(testCtx, node)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantMsg))
		})
	}
}

// nodeConfig is fixed at creation in both directions: the StatefulSet's
// podManagementPolicy follows it, and that field cannot change on an existing
// StatefulSet. Republishing under a new ConfigMap name stays allowed.
func TestNodeConfig_CreateOnly(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	withConfig := nodeConfigNode(ns, "nc-fixed")
	g.Expect(testCli.Create(testCtx, withConfig)).To(Succeed())
	key := client.ObjectKeyFromObject(withConfig)

	g.Expect(updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.NodeConfig.ConfigRef.Name = "rpc-config-v2"
	})).To(Succeed(), "republishing under a new name must be accepted")

	err := updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.NodeConfig = nil
	})
	g.Expect(err).To(HaveOccurred(), "removing nodeConfig must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("fixed at creation"))

	without := nodeConfigNode(ns, "nc-never")
	without.Spec.NodeConfig = nil
	g.Expect(testCli.Create(testCtx, without)).To(Succeed())

	err = updateNodeWithRetry(t, client.ObjectKeyFromObject(without), func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.NodeConfig = &seiv1alpha1.NodeConfig{
			ConfigRef: seiv1alpha1.ConfigFileRef{Name: "rpc-config-v1"},
			AppRef:    seiv1alpha1.ConfigFileRef{Name: "rpc-app-v1"},
		}
	})
	g.Expect(err).To(HaveOccurred(), "adding nodeConfig must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("fixed at creation"))
}

// These node shapes write config.toml at run time with values no ConfigMap
// written beforehand can hold, so the CRD rejects them beside nodeConfig.
func TestNodeConfig_RuntimeDiscoveredConfig_Rejected(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*seiv1alpha1.SeiNode)
		wantMsg string
	}{
		{"genesis ceremony", func(n *seiv1alpha1.SeiNode) {
			n.Spec.FullNode = nil
			n.Spec.Validator = &seiv1alpha1.ValidatorSpec{
				GenesisCeremony: &seiv1alpha1.GenesisCeremonyNodeConfig{
					ChainID:        "envtest-1",
					StakingAmount:  "1000000usei",
					AccountBalance: "2000000usei",
				},
			}
		}, "genesis-ceremony"},
		{"state sync on a full node", func(n *seiv1alpha1.SeiNode) {
			n.Spec.FullNode.Snapshot = &seiv1alpha1.SnapshotSource{StateSync: &seiv1alpha1.StateSyncSource{}}
		}, "state-sync"},
		{"state sync on a validator", func(n *seiv1alpha1.SeiNode) {
			n.Spec.FullNode = nil
			n.Spec.Validator = &seiv1alpha1.ValidatorSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{StateSync: &seiv1alpha1.StateSyncSource{}},
			}
		}, "state-sync"},
		{"autobahn consensus", func(n *seiv1alpha1.SeiNode) {
			n.Spec.Consensus = &seiv1alpha1.ConsensusSpec{Engine: seiv1alpha1.ConsensusEngineAutobahn}
		}, "Autobahn"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ns := makeNamespace(t)

			node := nodeConfigNode(ns, "nc-runtime")
			tc.mutate(node)

			err := testCli.Create(testCtx, node)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring(tc.wantMsg))

			node = nodeConfigNode(ns, "nc-runtime-ok")
			tc.mutate(node)
			node.Spec.NodeConfig = nil
			g.Expect(testCli.Create(testCtx, node)).To(Succeed(), "the shape itself is valid without nodeConfig")
		})
	}
}
