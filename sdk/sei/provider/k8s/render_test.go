package k8s

import (
	"testing"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

func TestRenderNetwork_ChainIDIntoGenesis(t *testing.T) {
	spec := sei.NetworkSpec{
		Name: testNet, ChainID: testChainID, Image: testImage, Replicas: 4,
		Overrides:       map[string]string{"staking.params.unbonding_time": "60s"},
		GenesisAccounts: []sei.GenesisAccount{{Address: "sei1abc", Balance: "100usei"}},
	}
	net := renderNetwork(spec, testNS)

	if net.Name != testNet || net.Namespace != testNS {
		t.Fatalf("metadata = %s/%s", net.Namespace, net.Name)
	}
	// ChainID maps to spec.genesis.chainId, NOT a top-level field.
	if net.Spec.Genesis.ChainID != "sei-chaos-1" {
		t.Errorf("genesis.chainId = %q, want sei-chaos-1", net.Spec.Genesis.ChainID)
	}
	if net.Spec.Image != "img:1" || net.Spec.Replicas != 4 {
		t.Errorf("image/replicas = %q/%d", net.Spec.Image, net.Spec.Replicas)
	}
	if got := net.Spec.Genesis.Overrides["staking.params.unbonding_time"]; string(got.Raw) != `"60s"` {
		t.Errorf("override raw JSON = %q, want \"60s\"", got.Raw)
	}
	if len(net.Spec.Genesis.Accounts) != 1 || net.Spec.Genesis.Accounts[0].Address != "sei1abc" {
		t.Errorf("genesis accounts = %+v", net.Spec.Genesis.Accounts)
	}
	// The network object is NOT label-stamped (followers match on its name).
	if _, ok := net.Labels[sei.LabelSeiNetwork]; ok {
		t.Errorf("network object must not carry sei.io/seinetwork label")
	}
}

func TestRenderNode_LabelsAndPeerWiring(t *testing.T) {
	spec := sei.FleetSpec{NamePrefix: rpcRole, Image: testImage, Replicas: 3}
	node := renderNode(spec, "nodes-ns", testNet, "genesis-ns", testChainID, 1)

	if node.Name != rpc1Name || node.Namespace != "nodes-ns" {
		t.Fatalf("name/ns = %s/%s, want rpc-1/nodes-ns", node.Name, node.Namespace)
	}
	// Canonical object labels — the fleet-wide producer contract (D5).
	if got := node.Labels[sei.LabelRole]; got != sei.RoleNode {
		t.Errorf("label %s = %q, want %q", sei.LabelRole, got, sei.RoleNode)
	}
	if got := node.Labels[sei.LabelSeiNetwork]; got != testNet {
		t.Errorf("label %s = %q, want chaos-net", sei.LabelSeiNetwork, got)
	}
	// The literal values must match the controller/seictl contract.
	if sei.LabelRole != "sei.io/role" || sei.RoleNode != "node" || sei.LabelSeiNetwork != "sei.io/seinetwork" {
		t.Fatalf("object-label producer contract drifted")
	}
	// Peer auto-wiring: derived from the network, selecting sei.io/seinetwork.
	if len(node.Spec.Peers) != 1 || node.Spec.Peers[0].Label == nil {
		t.Fatalf("synthesized label peer missing: %+v", node.Spec.Peers)
	}
	lbl := node.Spec.Peers[0].Label
	if lbl.Selector[sei.LabelSeiNetwork] != testNet {
		t.Errorf("peer selector = %v, want sei.io/seinetwork=chaos-net", lbl.Selector)
	}
	if lbl.Namespace != "genesis-ns" {
		t.Errorf("peer namespace = %q, want genesis-ns", lbl.Namespace)
	}
	// "rpc" preset renders a fullNode SeiNode.
	if node.Spec.FullNode == nil {
		t.Errorf("rpc preset must render fullNode mode: %+v", node.Spec)
	}
	if node.Spec.ChainID != testChainID {
		t.Errorf("chainId = %q, want sei-chaos-1", node.Spec.ChainID)
	}
}

func TestNodeName(t *testing.T) {
	cases := []struct {
		ordinal int
		want    string
	}{{0, rpc0Name}, {1, rpc1Name}, {12, "rpc-12"}}
	for _, tc := range cases {
		if got := nodeName(rpcRole, tc.ordinal); got != tc.want {
			t.Errorf("nodeName(rpc, %d) = %q, want %q", tc.ordinal, got, tc.want)
		}
	}
}

// Guard: a foreign struct change to the CRD endpoint leaves shouldn't silently
// pass — assert the projection field names line up by constructing the source.
func TestEndpointProjection_FieldAlignment(t *testing.T) {
	src := &seiv1alpha1.NodeEndpointStatus{
		EvmJsonRpc:     "http://n:8545",
		EvmWs:          "ws://n:8546",
		TendermintRpc:  "http://n:26657",
		TendermintRest: "http://n:1317",
	}
	got := sei.FleetNodeEndpoint{
		Name:           "n",
		EvmJsonRPC:     src.EvmJsonRpc,
		EvmWS:          src.EvmWs,
		TendermintRPC:  src.TendermintRpc,
		TendermintREST: src.TendermintRest,
	}
	if got.EvmJsonRPC == "" || got.TendermintRPC == "" || got.TendermintREST == "" || got.EvmWS == "" {
		t.Fatalf("projection dropped a field: %+v", got)
	}
}
