package k8s

import (
	"slices"
	"testing"

	"github.com/sei-protocol/sei-k8s-controller/sdk/sei"
)

func TestRenderNetwork_PropagatesVesting(t *testing.T) {
	spec := sei.NetworkSpec{
		Name: testNet, Image: testImage, Validators: 1,
		Accounts: []sei.GenesisAccount{
			{
				Address: testGenesisAddr,
				Balance: "2000000usei",
				Vesting: &sei.GenesisAccountVesting{Amount: "1000000usei", EndTime: 1893456000, Delayed: true},
			},
			{Address: "sei1def", Balance: "100usei"}, // no Vesting: must stay nil, not zero-valued
		},
	}
	net := renderNetwork(spec, testNS)

	if len(net.Spec.Genesis.Accounts) != 2 {
		t.Fatalf("genesis accounts = %+v", net.Spec.Genesis.Accounts)
	}
	v := net.Spec.Genesis.Accounts[0].Vesting
	if v == nil {
		t.Fatalf("Accounts[0].Vesting: got nil, want set")
	}
	if v.Amount != "1000000usei" || v.EndTime != 1893456000 || !v.Delayed {
		t.Errorf("Accounts[0].Vesting = %+v", v)
	}
	if net.Spec.Genesis.Accounts[1].Vesting != nil {
		t.Errorf("Accounts[1].Vesting: got %+v, want nil", net.Spec.Genesis.Accounts[1].Vesting)
	}
}

func TestRenderNetwork_ChainIDDefaultsToName(t *testing.T) {
	spec := sei.NetworkSpec{
		Name: testNet, Image: testImage, Validators: 4,
		Genesis:        map[string]string{"staking.params.unbonding_time": "60s"},
		Config:         map[string]string{"app.pruning": "nothing"},
		Accounts:       []sei.GenesisAccount{{Address: testGenesisAddr, Balance: "100usei"}},
		Labels:         map[string]string{testRunLabel: testRunID},
		DeletionPolicy: sei.DeletionDelete,
	}
	net := renderNetwork(spec, testNS)

	if net.Name != testNet || net.Namespace != testNS {
		t.Fatalf("metadata = %s/%s", net.Namespace, net.Name)
	}
	// ChainID defaults to Name and maps to spec.genesis.chainId.
	if net.Spec.Genesis.ChainID != testNet {
		t.Errorf("genesis.chainId = %q, want %q (defaults to Name)", net.Spec.Genesis.ChainID, testNet)
	}
	if net.Spec.Image != testImage || net.Spec.Replicas != 4 {
		t.Errorf("image/replicas = %q/%d", net.Spec.Image, net.Spec.Replicas)
	}
	if got := net.Spec.Genesis.Overrides["staking.params.unbonding_time"]; string(got.Raw) != `"60s"` {
		t.Errorf("genesis override raw JSON = %q, want \"60s\"", got.Raw)
	}
	if got := net.Spec.ConfigOverrides["app.pruning"]; got != "nothing" {
		t.Errorf("configOverrides = %v, want app.pruning=nothing", net.Spec.ConfigOverrides)
	}
	if len(net.Spec.Genesis.Accounts) != 1 || net.Spec.Genesis.Accounts[0].Address != testGenesisAddr {
		t.Errorf("genesis accounts = %+v", net.Spec.Genesis.Accounts)
	}
	// DeletionPolicy threads through so an ephemeral chain cascades to its
	// validators on teardown instead of orphaning them (the CRD default Retain).
	if got := string(net.Spec.DeletionPolicy); got != sei.DeletionDelete {
		t.Errorf("deletionPolicy = %q, want %q", got, sei.DeletionDelete)
	}
	// The network carries caller labels (e.g. a GC/run-id selector) but NOT the
	// canonical sei.io/seinetwork (nodes peer by its name, not a label on it).
	if got := net.Labels[testRunLabel]; got != testRunID {
		t.Errorf("caller label sei.io/harness-run = %q, want run-xyz (testRunID)", got)
	}
	if _, ok := net.Labels[sei.LabelSeiNetwork]; ok {
		t.Errorf("network object must not carry sei.io/seinetwork label")
	}
}

func TestRenderNetwork_SidecarImage(t *testing.T) {
	// Set: pins spec.sidecar.image, which the controller propagates to children.
	set := renderNetwork(sei.NetworkSpec{Name: testNet, Image: testImage, Validators: 4, SidecarImage: "seictl:dev"}, testNS)
	if set.Spec.Sidecar == nil || set.Spec.Sidecar.Image != "seictl:dev" {
		t.Errorf("sidecar = %+v, want image seictl:dev", set.Spec.Sidecar)
	}
	// Empty: leaves spec.sidecar unset so the platform default resolves.
	unset := renderNetwork(sei.NetworkSpec{Name: testNet, Image: testImage, Validators: 4}, testNS)
	if unset.Spec.Sidecar != nil {
		t.Errorf("sidecar = %+v, want nil (platform default)", unset.Spec.Sidecar)
	}
}

func TestRenderNode_LabelsAndPeerWiring(t *testing.T) {
	spec := sei.NodeSpec{Name: rpc1Name, Network: testNet, Image: testImage}
	node := renderNode(spec, testNS, testNS)

	if node.Name != rpc1Name || node.Namespace != testNS {
		t.Fatalf("name/ns = %s/%s, want %s/%s", node.Name, node.Namespace, rpc1Name, testNS)
	}
	// Canonical object labels — the fleet-wide producer contract.
	if got := node.Labels[sei.LabelRole]; got != sei.RoleNode {
		t.Errorf("label %s = %q, want %q", sei.LabelRole, got, sei.RoleNode)
	}
	if got := node.Labels[sei.LabelSeiNetwork]; got != testNet {
		t.Errorf("label %s = %q, want %s", sei.LabelSeiNetwork, got, testNet)
	}
	// The literal values must match the controller/seictl contract.
	if sei.LabelRole != "sei.io/role" || sei.RoleNode != "node" || sei.LabelSeiNetwork != "sei.io/seinetwork" {
		t.Fatalf("object-label producer contract drifted")
	}
	// Peer auto-wiring: derived from spec.Network, selecting sei.io/seinetwork at
	// the node's own namespace.
	if len(node.Spec.Peers) != 1 || node.Spec.Peers[0].Label == nil {
		t.Fatalf("synthesized label peer missing: %+v", node.Spec.Peers)
	}
	lbl := node.Spec.Peers[0].Label
	if lbl.Selector[sei.LabelSeiNetwork] != testNet {
		t.Errorf("peer selector = %v, want sei.io/seinetwork=%s", lbl.Selector, testNet)
	}
	if lbl.Namespace != testNS {
		t.Errorf("peer namespace = %q, want %s", lbl.Namespace, testNS)
	}
	// rpc renders a fullNode SeiNode; ChainID defaults to the network name.
	if node.Spec.FullNode == nil {
		t.Errorf("rpc node must render fullNode mode: %+v", node.Spec)
	}
	if node.Spec.ChainID != testNet {
		t.Errorf("chainId = %q, want %q (defaults to Network)", node.Spec.ChainID, testNet)
	}
}

func TestRenderNode_CallerLabelsMergeUnderCanonical(t *testing.T) {
	spec := sei.NodeSpec{
		Name: rpc0Name, Network: testNet, Image: testImage,
		Labels: map[string]string{
			testRunLabel:  testRunID,   // caller GC selector — must land
			sei.LabelRole: "validator", // collides with canonical — must NOT win
		},
	}
	node := renderNode(spec, testNS, testNS)

	if got := node.Labels[testRunLabel]; got != testRunID {
		t.Errorf("caller label sei.io/harness-run = %q, want run-xyz (testRunID)", got)
	}
	// Canonical labels are load-bearing (peer wiring, chaos selectors) and win
	// on any key collision with caller labels.
	if got := node.Labels[sei.LabelRole]; got != sei.RoleNode {
		t.Errorf("canonical %s = %q, want %q (must override caller)", sei.LabelRole, got, sei.RoleNode)
	}
	if got := node.Labels[sei.LabelSeiNetwork]; got != testNet {
		t.Errorf("canonical %s = %q, want %s", sei.LabelSeiNetwork, got, testNet)
	}
}

func TestRenderNode_StateSync(t *testing.T) {
	// Two DISTINCT witnesses: the served CRD field is a MinItems=2 listType=set,
	// so a duplicated set is not what render carries end to end.
	witnesses := []string{"validator-0:26657", "validator-1:26657"}
	spec := sei.NodeSpec{
		Name: rpc0Name, Network: testNet, Image: testImage,
		StateSync: &sei.NodeStateSync{RpcServers: witnesses},
	}
	node := renderNode(spec, testNS, testNS)

	// StateSync maps to spec.fullNode.snapshot with the stateSync variant and the
	// caller's bare host:port witnesses.
	snap := node.Spec.FullNode.Snapshot
	if snap == nil || snap.StateSync == nil {
		t.Fatalf("state-sync node must render fullNode.snapshot.stateSync: %+v", node.Spec.FullNode)
	}
	if !slices.Equal(snap.RpcServers, witnesses) {
		t.Errorf("snapshot.rpcServers = %v, want %v", snap.RpcServers, witnesses)
	}

	// Genesis block-sync (nil StateSync) leaves Snapshot unset — the unchanged path.
	genesis := renderNode(sei.NodeSpec{Name: rpc0Name, Network: testNet, Image: testImage}, testNS, testNS)
	if genesis.Spec.FullNode.Snapshot != nil {
		t.Errorf("genesis node must leave snapshot nil, got %+v", genesis.Spec.FullNode.Snapshot)
	}
}

func TestRenderNode_ConfigIntoOverrides(t *testing.T) {
	spec := sei.NodeSpec{
		Name: rpc0Name, Network: testNet, Image: testImage,
		Config: map[string]string{"config.moniker": "rpc-0"},
	}
	node := renderNode(spec, testNS, testNS)
	if got := node.Spec.Overrides["config.moniker"]; got != "rpc-0" {
		t.Errorf("node overrides = %v, want config.moniker=rpc-0", node.Spec.Overrides)
	}
}
