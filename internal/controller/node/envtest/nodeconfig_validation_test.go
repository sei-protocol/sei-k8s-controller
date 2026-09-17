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

// The field is deliberately mutable, unlike the create-only pod-template
// fields beside it: adopting a ConfigMap on a running node is the point.
func TestNodeConfig_Mutable(t *testing.T) {
	g := NewWithT(t)
	ns := makeNamespace(t)

	// Created without it, then adopted.
	node := nodeConfigNode(ns, "nc-mutable")
	node.Spec.NodeConfig = nil
	g.Expect(testCli.Create(testCtx, node)).To(Succeed())
	key := client.ObjectKeyFromObject(node)

	g.Expect(updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.NodeConfig = &seiv1alpha1.NodeConfig{
			ConfigRef: seiv1alpha1.ConfigFileRef{Name: "rpc-config-v1"},
			AppRef:    seiv1alpha1.ConfigFileRef{Name: "rpc-app-v1"},
		}
	})).To(Succeed(), "a running node must be able to adopt a ConfigMap")

	g.Expect(updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.NodeConfig.ConfigRef.Name = "rpc-config-v2"
	})).To(Succeed(), "republishing under a new name must be accepted")

	g.Expect(updateNodeWithRetry(t, key, func(cur *seiv1alpha1.SeiNode) {
		cur.Spec.NodeConfig = nil
	})).To(Succeed(), "reverting to controller-managed config must be accepted")
}
