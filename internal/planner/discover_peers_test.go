package planner

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sidecar "github.com/sei-protocol/seictl/sidecar/client"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

const (
	dpChainID  = "arctic-1"
	dpChainKey = "sei.io/chain"
	dpStatic   = "peer1@host:26656"
)

func nodeWithPeers(peers []seiv1alpha1.PeerSource, resolved []string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "n-0", Namespace: dpChainID},
		Spec:       seiv1alpha1.SeiNodeSpec{Peers: peers},
		Status:     seiv1alpha1.SeiNodeStatus{ResolvedPeers: resolved},
	}
}

func labelSource() seiv1alpha1.PeerSource {
	return seiv1alpha1.PeerSource{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{dpChainKey: dpChainID}}}
}

func staticSource() seiv1alpha1.PeerSource {
	return seiv1alpha1.PeerSource{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{dpStatic}}}
}

func TestDiscoverPeersTask(t *testing.T) {
	resolved := []string{"a-0@a:26656", "b-0@b:26656"}

	cases := []struct {
		name string
		node *seiv1alpha1.SeiNode
		want []sidecar.PeerSource
	}{
		{
			name: "no peers configured",
			node: nodeWithPeers(nil, nil),
			want: nil,
		},
		{
			name: "label with zero resolved peers is omitted",
			node: nodeWithPeers([]seiv1alpha1.PeerSource{labelSource()}, nil),
			want: nil,
		},
		{
			name: "label with resolved peers emits static",
			node: nodeWithPeers([]seiv1alpha1.PeerSource{labelSource()}, resolved),
			want: []sidecar.PeerSource{{Type: sidecar.PeerSourceStatic, Addresses: resolved}},
		},
		{
			name: "static with empty addresses is omitted",
			node: nodeWithPeers(
				[]seiv1alpha1.PeerSource{{Static: &seiv1alpha1.StaticPeerSource{Addresses: nil}}},
				nil,
			),
			want: nil,
		},
		{
			name: "static with addresses is emitted",
			node: nodeWithPeers([]seiv1alpha1.PeerSource{staticSource()}, nil),
			want: []sidecar.PeerSource{{Type: sidecar.PeerSourceStatic, Addresses: []string{dpStatic}}},
		},
		{
			name: "mixed sources skip empty label and preserve order",
			node: nodeWithPeers(
				[]seiv1alpha1.PeerSource{
					{EC2Tags: &seiv1alpha1.EC2TagsPeerSource{Region: "eu-west-1", Tags: map[string]string{"chain": dpChainID}}},
					labelSource(), // resolves to zero
					staticSource(),
				},
				nil,
			),
			want: []sidecar.PeerSource{
				{Type: sidecar.PeerSourceEC2Tags, Region: "eu-west-1", Tags: map[string]string{"chain": dpChainID}},
				{Type: sidecar.PeerSourceStatic, Addresses: []string{dpStatic}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := discoverPeersTask(tc.node).Sources
			if !peerSourcesEqual(got, tc.want) {
				t.Errorf("discoverPeersTask sources = %+v, want %+v", got, tc.want)
			}

			// needsDiscoverPeers gates task insertion. When it reports true the
			// materialized task must pass seictl validation; when false the
			// task is empty (seictl rejects a zero-source task), so the planner
			// must omit it rather than wedge discover-peers on a retry loop.
			err := discoverPeersTask(tc.node).Validate()
			if needsDiscoverPeers(tc.node) {
				if err != nil {
					t.Errorf("needsDiscoverPeers=true but task is invalid: %v", err)
				}
			} else if err == nil {
				t.Error("needsDiscoverPeers=false but empty task unexpectedly validates")
			}
		})
	}
}

func peerSourcesEqual(a, b []sidecar.PeerSource) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type || a[i].Region != b[i].Region {
			return false
		}
		if !stringSlicesEqual(a[i].Addresses, b[i].Addresses) || !stringSlicesEqual(a[i].Endpoints, b[i].Endpoints) {
			return false
		}
		if len(a[i].Tags) != len(b[i].Tags) {
			return false
		}
		for k, v := range a[i].Tags {
			if b[i].Tags[k] != v {
				return false
			}
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
