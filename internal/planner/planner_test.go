package planner

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

// The init/bootstrap discover-peers task freezes its sources at plan-build time
// from the live SeiNode: ec2Tags→EC2Tags, static→Static, and label→Static of
// status.resolvedPeers (pre-composed addresses written verbatim). A label-only
// node with empty resolvedPeers freezes a single zero-address static source —
// the behavior the SeiNodeTask DiscoverPeers kind diverges from (it fails fast),
// but the init path intentionally retains.
func TestDiscoverPeersTask_FreezesSourcesFromLiveNode(t *testing.T) {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "peer-node", Namespace: "peer-ns"},
		Spec: seiv1alpha1.SeiNodeSpec{
			Peers: []seiv1alpha1.PeerSource{
				{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "us-east-1", Tags: map[string]string{"role": "seed"}}},
				{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"abc@1.2.3.4:26656"}}},
				{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{"sei.io/chain": "c"}}},
			},
		},
		Status: seiv1alpha1.SeiNodeStatus{ResolvedPeers: []string{"def@5.6.7.8:26656"}},
	}

	got := discoverPeersTask(node)
	want := sidecar.DiscoverPeersTask{Sources: []sidecar.PeerSource{
		{Type: sidecar.PeerSourceEC2Tags, Region: "us-east-1", Tags: map[string]string{"role": "seed"}},
		{Type: sidecar.PeerSourceStatic, Addresses: []string{"abc@1.2.3.4:26656"}},
		{Type: sidecar.PeerSourceStatic, Addresses: []string{"def@5.6.7.8:26656"}},
	}}
	if len(got.Sources) != len(want.Sources) {
		t.Fatalf("source count = %d, want %d (%+v)", len(got.Sources), len(want.Sources), got.Sources)
	}
	for i := range want.Sources {
		if got.Sources[i].Type != want.Sources[i].Type {
			t.Errorf("source[%d].Type = %q, want %q", i, got.Sources[i].Type, want.Sources[i].Type)
		}
	}
}

// No spec.peers freezes an empty DiscoverPeersTask (the init path submits it
// rather than failing — distinct from the nodetask kind's fail-fast).
func TestDiscoverPeersTask_NoPeers_Empty(t *testing.T) {
	node := &seiv1alpha1.SeiNode{ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns"}}
	if got := discoverPeersTask(node); len(got.Sources) != 0 {
		t.Errorf("expected empty sources, got %+v", got.Sources)
	}
}

func TestTaskMaxRetries(t *testing.T) {
	cases := map[string]int{
		TaskConfigureGenesis: genesisConfigureMaxRetries,
		TaskAssembleGenesis:  groupAssemblyMaxRetries,
		TaskDiscoverPeers:    discoverPeersMaxRetries,
		"unknown-task-type":  0,
		"":                   0,
	}
	for taskType, want := range cases {
		if got := taskMaxRetries(taskType); got != want {
			t.Errorf("taskMaxRetries(%q) = %d, want %d", taskType, got, want)
		}
	}
}
