package node

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	testRoleLabel       = "role"
	testRoleValue       = "validator"
	testConsumerName    = "consumer"
	testPeer1ResolvedID = "mock-node-id@peer-1-0.peer-1.default.svc.cluster.local:26656"
	testWitnessNS       = "arctic-1"
	testWitnessRole     = "syncer"
)

type errStub string

func (e errStub) Error() string { return string(e) }

func TestReconcilePeers_ResolvesLabelSource(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "my-node", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-1",
			Image:   "sei:latest",
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{"sei.io/nodedeployment": "validators"},
				}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	peer1 := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "peer-1", Namespace: "default",
			Labels: map[string]string{"sei.io/nodedeployment": "validators"},
		},
		Spec: seiv1alpha1.SeiNodeSpec{ChainID: "test-1", Image: "sei:latest", FullNode: &seiv1alpha1.FullNodeSpec{}},
	}
	peer2 := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "peer-2", Namespace: "default",
			Labels: map[string]string{"sei.io/nodedeployment": "validators"},
		},
		Spec: seiv1alpha1.SeiNodeSpec{ChainID: "test-1", Image: "sei:latest", FullNode: &seiv1alpha1.FullNodeSpec{}},
	}

	r, _ := newNodeReconciler(t, node, peer1, peer2)
	ctx := context.Background()

	if err := r.reconcilePeers(ctx, node); err != nil {
		t.Fatalf("reconcilePeers: %v", err)
	}

	if len(node.Status.ResolvedPeers) != 2 {
		t.Fatalf("expected 2 resolved peers, got %d: %v", len(node.Status.ResolvedPeers), node.Status.ResolvedPeers)
	}
	want := []string{
		testPeer1ResolvedID,
		"mock-node-id@peer-2-0.peer-2.default.svc.cluster.local:26656",
	}
	for i, w := range want {
		if node.Status.ResolvedPeers[i] != w {
			t.Errorf("resolvedPeers[%d] = %q, want %q", i, node.Status.ResolvedPeers[i], w)
		}
	}
}

func TestReconcilePeers_PrefersExternalAddress(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testConsumerName, Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-1",
			Image:   "sei:latest",
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{testRoleLabel: "publishable"},
				}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	peer := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pub-peer", Namespace: "default",
			Labels: map[string]string{testRoleLabel: "publishable"},
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:         "test-1",
			Image:           "sei:latest",
			ExternalAddress: "pub-peer-p2p.test-1.example.com:26656",
			FullNode:        &seiv1alpha1.FullNodeSpec{},
		},
	}

	r, _ := newNodeReconciler(t, node, peer)
	if err := r.reconcilePeers(context.Background(), node); err != nil {
		t.Fatalf("reconcilePeers: %v", err)
	}
	if len(node.Status.ResolvedPeers) != 1 {
		t.Fatalf("expected 1 peer, got %d: %v", len(node.Status.ResolvedPeers), node.Status.ResolvedPeers)
	}
	want := "mock-node-id@pub-peer-p2p.test-1.example.com:26656"
	if node.Status.ResolvedPeers[0] != want {
		t.Errorf("resolvedPeers[0] = %q, want %q", node.Status.ResolvedPeers[0], want)
	}

	// The witness must be the internal RPC DNS, NOT the external P2P address:
	// the NLB exposes P2P only. Writing the external address as a witness is
	// the regression this fix prevents.
	wantWitness := "pub-peer-0.pub-peer.default.svc.cluster.local:26657"
	if len(node.Status.ResolvedRPCWitnesses) != 1 || node.Status.ResolvedRPCWitnesses[0] != wantWitness {
		t.Errorf("resolvedRPCWitnesses = %v, want [%q]", node.Status.ResolvedRPCWitnesses, wantWitness)
	}
}

func TestReconcilePeers_WitnessesExcludeSelfAndUseRPCPort(t *testing.T) {
	const peerName = "syncer-0-1"
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "syncer-0-0", Namespace: testWitnessNS,
			Labels: map[string]string{testRoleLabel: testWitnessRole},
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: testWitnessNS,
			Image:   "sei:latest",
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{testRoleLabel: testWitnessRole},
				}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	peer := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: peerName, Namespace: testWitnessNS,
			Labels: map[string]string{testRoleLabel: testWitnessRole},
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  testWitnessNS,
			Image:    "sei:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}

	r, _ := newNodeReconciler(t, node, peer)
	if err := r.reconcilePeers(context.Background(), node); err != nil {
		t.Fatalf("reconcilePeers: %v", err)
	}

	want := peerName + "-0." + peerName + "." + testWitnessNS + ".svc.cluster.local:26657"
	if len(node.Status.ResolvedRPCWitnesses) != 1 || node.Status.ResolvedRPCWitnesses[0] != want {
		t.Errorf("resolvedRPCWitnesses = %v, want [%q] (self excluded, RPC port)",
			node.Status.ResolvedRPCWitnesses, want)
	}
}

func TestReconcilePeers_ExcludesSelf(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "my-node", Namespace: "default",
			Labels: map[string]string{"sei.io/nodedeployment": "validators"},
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-1",
			Image:   "sei:latest",
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{"sei.io/nodedeployment": "validators"},
				}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	peer := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-node", Namespace: "default",
			Labels: map[string]string{"sei.io/nodedeployment": "validators"},
		},
		Spec: seiv1alpha1.SeiNodeSpec{ChainID: "test-1", Image: "sei:latest", FullNode: &seiv1alpha1.FullNodeSpec{}},
	}

	r, _ := newNodeReconciler(t, node, peer)
	ctx := context.Background()

	if err := r.reconcilePeers(ctx, node); err != nil {
		t.Fatalf("reconcilePeers: %v", err)
	}

	if len(node.Status.ResolvedPeers) != 1 {
		t.Fatalf("expected 1 resolved peer (self excluded), got %d: %v", len(node.Status.ResolvedPeers), node.Status.ResolvedPeers)
	}
	if node.Status.ResolvedPeers[0] != "mock-node-id@other-node-0.other-node.default.svc.cluster.local:26656" {
		t.Errorf("resolvedPeers[0] = %q", node.Status.ResolvedPeers[0])
	}
}

func TestReconcilePeers_CrossNamespace_DoesNotExcludeMatchingName(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-name", Namespace: "ns-a"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-1",
			Image:   "sei:latest",
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector:  map[string]string{testRoleLabel: "peer"},
					Namespace: "ns-b",
				}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	// Same name, different namespace — should NOT be excluded
	peerSameName := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "shared-name", Namespace: "ns-b",
			Labels: map[string]string{testRoleLabel: "peer"},
		},
		Spec: seiv1alpha1.SeiNodeSpec{ChainID: "test-1", Image: "sei:latest", FullNode: &seiv1alpha1.FullNodeSpec{}},
	}

	r, _ := newNodeReconciler(t, node, peerSameName)
	ctx := context.Background()

	if err := r.reconcilePeers(ctx, node); err != nil {
		t.Fatalf("reconcilePeers: %v", err)
	}

	if len(node.Status.ResolvedPeers) != 1 {
		t.Fatalf("expected 1 peer (same name, different ns), got %d: %v", len(node.Status.ResolvedPeers), node.Status.ResolvedPeers)
	}
}

func TestReconcilePeers_NoPatchWhenUnchanged(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "my-node", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-1",
			Image:   "sei:latest",
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{"sei.io/nodedeployment": "validators"},
				}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			ResolvedPeers: []string{testPeer1ResolvedID},
		},
	}
	peer := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "peer-1", Namespace: "default",
			Labels: map[string]string{"sei.io/nodedeployment": "validators"},
		},
		Spec: seiv1alpha1.SeiNodeSpec{ChainID: "test-1", Image: "sei:latest", FullNode: &seiv1alpha1.FullNodeSpec{}},
	}

	r, _ := newNodeReconciler(t, node, peer)

	// Should not error — resolved peers match, no patch needed
	if err := r.reconcilePeers(context.Background(), node); err != nil {
		t.Fatalf("reconcilePeers: %v", err)
	}
}

func TestReconcilePeers_NoLabelSources_NoPatch(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "my-node", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-1",
			Image:   "sei:latest",
			Peers: []seiv1alpha1.PeerSource{
				{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "us-east-1", Tags: map[string]string{"Chain": "test-1"}}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}

	r, _ := newNodeReconciler(t, node)

	if err := r.reconcilePeers(context.Background(), node); err != nil {
		t.Fatalf("reconcilePeers: %v", err)
	}
	// No label sources means no resolved peers, no patch — just verifying no error
}

// Transient sidecar failure: prior entry is preserved.
func TestReconcilePeers_PreservesPriorEntryOnTransientFailure(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testConsumerName, Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-1",
			Image:   "sei:latest",
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{testRoleLabel: testRoleValue},
				}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			ResolvedPeers: []string{
				testPeer1ResolvedID,
			},
		},
	}
	peer := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "peer-1", Namespace: "default",
			Labels: map[string]string{testRoleLabel: testRoleValue},
		},
		Spec: seiv1alpha1.SeiNodeSpec{ChainID: "test-1", Image: "sei:latest", FullNode: &seiv1alpha1.FullNodeSpec{}},
	}

	// Sidecar returns an error — simulates a peer mid-restart.
	mock := &mockSidecarClient{nodeIDErr: errStub("sidecar unreachable")}
	r, _ := newNodeReconcilerWithSidecar(t, mock, node, peer)

	if err := r.reconcilePeers(context.Background(), node); err != nil {
		t.Fatalf("reconcilePeers errored on transient peer failure: %v", err)
	}
	if len(node.Status.ResolvedPeers) != 1 {
		t.Fatalf("expected prior entry preserved, got %d: %v", len(node.Status.ResolvedPeers), node.Status.ResolvedPeers)
	}
	want := testPeer1ResolvedID
	if node.Status.ResolvedPeers[0] != want {
		t.Errorf("resolvedPeers[0] = %q, want preserved %q", node.Status.ResolvedPeers[0], want)
	}
}

// New peer with no prior entry + sidecar failure: skip, don't wedge.
func TestReconcilePeers_SkipsNewPeerOnSidecarFailure(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testConsumerName, Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-1",
			Image:   "sei:latest",
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{testRoleLabel: testRoleValue},
				}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	peer := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "peer-1", Namespace: "default",
			Labels: map[string]string{testRoleLabel: testRoleValue},
		},
		Spec: seiv1alpha1.SeiNodeSpec{ChainID: "test-1", Image: "sei:latest", FullNode: &seiv1alpha1.FullNodeSpec{}},
	}

	mock := &mockSidecarClient{nodeIDErr: errStub("sidecar unreachable")}
	r, _ := newNodeReconcilerWithSidecar(t, mock, node, peer)

	if err := r.reconcilePeers(context.Background(), node); err != nil {
		t.Fatalf("reconcilePeers errored on new-peer sidecar failure: %v", err)
	}
	if len(node.Status.ResolvedPeers) != 0 {
		t.Fatalf("expected new unresolvable peer to be skipped, got %d: %v", len(node.Status.ResolvedPeers), node.Status.ResolvedPeers)
	}
}

// Nil factory + new peer: skip without panic.
func TestReconcilePeers_NilSidecarFactorySkipsNewPeer(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testConsumerName, Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-1",
			Image:   "sei:latest",
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{testRoleLabel: testRoleValue},
				}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	peer := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "peer-1", Namespace: "default",
			Labels: map[string]string{testRoleLabel: testRoleValue},
		},
		Spec: seiv1alpha1.SeiNodeSpec{ChainID: "test-1", Image: "sei:latest", FullNode: &seiv1alpha1.FullNodeSpec{}},
	}

	r, _ := newNodeReconciler(t, node, peer)
	r.Planner.BuildSidecarClient = nil

	if err := r.reconcilePeers(context.Background(), node); err != nil {
		t.Fatalf("reconcilePeers errored on nil factory: %v", err)
	}
	if len(node.Status.ResolvedPeers) != 0 {
		t.Fatalf("expected unresolvable peer to be skipped, got %d: %v", len(node.Status.ResolvedPeers), node.Status.ResolvedPeers)
	}
	// Intentional asymmetry: the witness needs no node_id, so it is emitted
	// even though the peer was skipped from persistent_peers. seid can dial a
	// state-sync RPC witness it has no P2P peering with; do not "symmetrize"
	// this with ResolvedPeers.
	wantWitness := "peer-1-0.peer-1.default.svc.cluster.local:26657"
	if len(node.Status.ResolvedRPCWitnesses) != 1 || node.Status.ResolvedRPCWitnesses[0] != wantWitness {
		t.Errorf("expected witness despite skipped peer, got %v", node.Status.ResolvedRPCWitnesses)
	}
}

// Nil factory + prior entry: preserve-prior branch fires.
func TestReconcilePeers_NilSidecarFactoryPreservesPriorEntry(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: testConsumerName, Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-1",
			Image:   "sei:latest",
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{testRoleLabel: testRoleValue},
				}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			ResolvedPeers: []string{testPeer1ResolvedID},
		},
	}
	peer := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "peer-1", Namespace: "default",
			Labels: map[string]string{testRoleLabel: testRoleValue},
		},
		Spec: seiv1alpha1.SeiNodeSpec{ChainID: "test-1", Image: "sei:latest", FullNode: &seiv1alpha1.FullNodeSpec{}},
	}

	r, _ := newNodeReconciler(t, node, peer)
	r.Planner.BuildSidecarClient = nil

	if err := r.reconcilePeers(context.Background(), node); err != nil {
		t.Fatalf("reconcilePeers errored on nil factory: %v", err)
	}
	if len(node.Status.ResolvedPeers) != 1 || node.Status.ResolvedPeers[0] != testPeer1ResolvedID {
		t.Fatalf("expected prior entry preserved, got %v", node.Status.ResolvedPeers)
	}
}

func TestReconcilePeers_DeduplicatesOverlappingSources(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "my-node", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "test-1",
			Image:   "sei:latest",
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{"sei.io/nodedeployment": "validators"},
				}},
				{Label: &seiv1alpha1.LabelPeerSource{
					Selector: map[string]string{"sei.io/chain": "test-1"},
				}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
	// This peer matches both selectors
	peer := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "peer-1", Namespace: "default",
			Labels: map[string]string{
				"sei.io/nodedeployment": "validators",
				"sei.io/chain":          "test-1",
			},
		},
		Spec: seiv1alpha1.SeiNodeSpec{ChainID: "test-1", Image: "sei:latest", FullNode: &seiv1alpha1.FullNodeSpec{}},
	}

	r, _ := newNodeReconciler(t, node, peer)
	ctx := context.Background()

	if err := r.reconcilePeers(ctx, node); err != nil {
		t.Fatalf("reconcilePeers: %v", err)
	}

	if len(node.Status.ResolvedPeers) != 1 {
		t.Fatalf("expected 1 deduplicated peer, got %d: %v", len(node.Status.ResolvedPeers), node.Status.ResolvedPeers)
	}
}
