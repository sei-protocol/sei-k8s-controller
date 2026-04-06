package node

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func newRunningFullNode(name, namespace string) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: 1,
			Finalizers: []string{nodeFinalizerName},
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: "sei-test",
			Image:   "ghcr.io/sei-protocol/seid:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{
				Snapshot: &seiv1alpha1.SnapshotSource{
					S3: &seiv1alpha1.S3SnapshotSource{TargetHeight: 100000},
				},
			},
			Sidecar: &seiv1alpha1.SidecarConfig{Port: 7777},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase:              seiv1alpha1.PhaseRunning,
			ObservedGeneration: 1,
		},
	}
}

func TestDetectPeerDrift_NoDrift(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newRunningFullNode("node-0", "default")
	// Generation == ObservedGeneration → no drift.
	r, c := newNodeReconciler(t, node)

	err := r.detectPeerDrift(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "node-0", "default")
	cond := meta.FindStatusCondition(fetched.Status.Conditions, ConditionPeerUpdateNeeded)
	g.Expect(cond).To(BeNil())
}

func TestDetectPeerDrift_PeersAdded(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newRunningFullNode("node-0", "default")
	// Simulate spec change: generation advances, peers added.
	node.Generation = 2
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"abc@1.2.3.4:26656"}}},
	}

	r, c := newNodeReconciler(t, node)

	err := r.detectPeerDrift(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "node-0", "default")
	cond := meta.FindStatusCondition(fetched.Status.Conditions, ConditionPeerUpdateNeeded)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonPeerSpecChanged))
}

func TestDetectPeerDrift_SkippedWhenPlanActive(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newRunningFullNode("node-0", "default")
	node.Generation = 2
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"abc@1.2.3.4:26656"}}},
	}
	node.Status.Plan = &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{},
	}

	r, c := newNodeReconciler(t, node)

	err := r.detectPeerDrift(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "node-0", "default")
	cond := meta.FindStatusCondition(fetched.Status.Conditions, ConditionPeerUpdateNeeded)
	g.Expect(cond).To(BeNil(), "should not set condition when plan is active")
}

func TestDetectPeerDrift_SkippedWhenNotRunning(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newRunningFullNode("node-0", "default")
	node.Status.Phase = seiv1alpha1.PhaseInitializing
	node.Generation = 2
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"abc@1.2.3.4:26656"}}},
	}

	r, c := newNodeReconciler(t, node)

	err := r.detectPeerDrift(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "node-0", "default")
	cond := meta.FindStatusCondition(fetched.Status.Conditions, ConditionPeerUpdateNeeded)
	g.Expect(cond).To(BeNil(), "should not set condition when not running")
}

func TestDetectPeerDrift_SkippedWhenNoPeers(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newRunningFullNode("node-0", "default")
	// Generation changed but no peers configured.
	node.Generation = 2

	r, c := newNodeReconciler(t, node)

	err := r.detectPeerDrift(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "node-0", "default")
	cond := meta.FindStatusCondition(fetched.Status.Conditions, ConditionPeerUpdateNeeded)
	g.Expect(cond).To(BeNil(), "should not set condition when no peers are configured")
}

func TestDetectPeerDrift_AlreadyFlagged(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newRunningFullNode("node-0", "default")
	node.Generation = 2
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"abc@1.2.3.4:26656"}}},
	}
	// Condition already set.
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:   ConditionPeerUpdateNeeded,
		Status: metav1.ConditionTrue,
		Reason: ReasonPeerSpecChanged,
	})

	r, _ := newNodeReconciler(t, node)

	// Should be a no-op (no patch attempted).
	err := r.detectPeerDrift(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())
}
