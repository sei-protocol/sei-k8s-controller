package planner

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func TestPeersBlockPlan(t *testing.T) {
	now := time.Now()
	label := seiv1alpha1.PeerSource{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{"sei.io/chain": "arctic-1"}}}
	static := seiv1alpha1.PeerSource{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"id@host:26656"}}}

	cases := []struct {
		name     string
		peers    []seiv1alpha1.PeerSource
		resolved []string
		phase    seiv1alpha1.SeiNodePhase
		age      time.Duration
		want     bool
	}{
		{name: "label peer unresolved on a new node", peers: []seiv1alpha1.PeerSource{label}, age: time.Minute, want: true},
		{name: "label peer unresolved while initializing", peers: []seiv1alpha1.PeerSource{label}, phase: seiv1alpha1.PhaseInitializing, age: time.Minute, want: true},
		{name: "label peer resolved", peers: []seiv1alpha1.PeerSource{label}, resolved: []string{"id@host:26656"}, age: time.Minute},
		{name: "wait expired", peers: []seiv1alpha1.PeerSource{label}, age: PeerResolutionWait + time.Second},
		{name: "running node", peers: []seiv1alpha1.PeerSource{label}, phase: seiv1alpha1.PhaseRunning, age: time.Minute},
		{name: "static peers only", peers: []seiv1alpha1.PeerSource{static}, age: time.Minute},
		{name: "no peers", age: time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			node := &seiv1alpha1.SeiNode{
				ObjectMeta: metav1.ObjectMeta{Name: "rpc-0", CreationTimestamp: metav1.NewTime(now.Add(-tc.age))},
				Spec:       seiv1alpha1.SeiNodeSpec{Peers: tc.peers},
				Status:     seiv1alpha1.SeiNodeStatus{Phase: tc.phase, ResolvedPeers: tc.resolved},
			}
			g.Expect(PeersBlockPlan(node, now)).To(Equal(tc.want))
		})
	}
}
