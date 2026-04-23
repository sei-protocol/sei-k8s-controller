package node

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

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
		"peer-1-0.peer-1.default.svc.cluster.local",
		"peer-2-0.peer-2.default.svc.cluster.local",
	}
	for i, w := range want {
		if node.Status.ResolvedPeers[i] != w {
			t.Errorf("resolvedPeers[%d] = %q, want %q", i, node.Status.ResolvedPeers[i], w)
		}
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
	if node.Status.ResolvedPeers[0] != "other-node-0.other-node.default.svc.cluster.local" {
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
					Selector:  map[string]string{"role": "peer"},
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
			Labels: map[string]string{"role": "peer"},
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
			ResolvedPeers: []string{"peer-1-0.peer-1.default.svc.cluster.local"},
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
