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
	node := &seiv1alpha1.SeiNode{
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
	return node
}

func TestDetectConfigDrift_NoDrift(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newRunningFullNode("node-0", "default")
	// Set LastAppliedPeerParams to match current spec (no peers).
	peerParams, err := MarshalPeerParams(node)
	g.Expect(err).NotTo(HaveOccurred())
	node.Status.LastAppliedPeerParams = peerParams

	r, c := newNodeReconciler(t, node)

	err = r.detectConfigDrift(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "node-0", "default")
	cond := meta.FindStatusCondition(fetched.Status.Conditions, ConditionConfigUpdateNeeded)
	g.Expect(cond).To(BeNil(), "no condition should be set when there is no drift")
}

func TestDetectConfigDrift_PeersAdded(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newRunningFullNode("node-0", "default")
	// LastAppliedPeerParams is nil (no peers at init time).
	node.Status.LastAppliedPeerParams = nil

	// Now add peers to the spec.
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"abc@1.2.3.4:26656"}}},
	}
	node.Generation = 2

	r, c := newNodeReconciler(t, node)

	err := r.detectConfigDrift(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "node-0", "default")
	cond := meta.FindStatusCondition(fetched.Status.Conditions, ConditionConfigUpdateNeeded)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(cond.Reason).To(Equal(ReasonConfigDriftDetected))
}

func TestDetectConfigDrift_SkippedWhenPlanActive(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newRunningFullNode("node-0", "default")
	node.Status.Plan = &seiv1alpha1.TaskPlan{
		Phase: seiv1alpha1.TaskPlanActive,
		Tasks: []seiv1alpha1.PlannedTask{},
	}
	// Introduce drift.
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"abc@1.2.3.4:26656"}}},
	}

	r, c := newNodeReconciler(t, node)

	err := r.detectConfigDrift(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "node-0", "default")
	cond := meta.FindStatusCondition(fetched.Status.Conditions, ConditionConfigUpdateNeeded)
	g.Expect(cond).To(BeNil(), "should not set condition when plan is active")
}

func TestDetectConfigDrift_SkippedWhenNotRunning(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newRunningFullNode("node-0", "default")
	node.Status.Phase = seiv1alpha1.PhaseInitializing
	node.Spec.Peers = []seiv1alpha1.PeerSource{
		{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"abc@1.2.3.4:26656"}}},
	}

	r, c := newNodeReconciler(t, node)

	err := r.detectConfigDrift(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "node-0", "default")
	cond := meta.FindStatusCondition(fetched.Status.Conditions, ConditionConfigUpdateNeeded)
	g.Expect(cond).To(BeNil(), "should not set condition when not running")
}

func TestDetectConfigDrift_ClearsConditionOnRevert(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := newRunningFullNode("node-0", "default")
	// Simulate: condition was set, but spec was reverted back to match lastApplied.
	peerParams, err := MarshalPeerParams(node)
	g.Expect(err).NotTo(HaveOccurred())
	node.Status.LastAppliedPeerParams = peerParams
	meta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:   ConditionConfigUpdateNeeded,
		Status: metav1.ConditionTrue,
		Reason: ReasonConfigDriftDetected,
	})

	r, c := newNodeReconciler(t, node)

	err = r.detectConfigDrift(ctx, node)
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, "node-0", "default")
	cond := meta.FindStatusCondition(fetched.Status.Conditions, ConditionConfigUpdateNeeded)
	g.Expect(cond).To(BeNil(), "condition should be cleared when spec matches lastApplied")
}

func TestPeerParamsEqual(t *testing.T) {
	g := NewWithT(t)

	node := newRunningFullNode("node-0", "default")

	// Both nil — equal.
	g.Expect(peerParamsEqual(nil, nil)).To(BeTrue())

	// One nil — not equal.
	params, err := MarshalPeerParams(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(peerParamsEqual(nil, params)).To(BeFalse())
	g.Expect(peerParamsEqual(params, nil)).To(BeFalse())

	// Same — equal.
	params2, err := MarshalPeerParams(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(peerParamsEqual(params, params2)).To(BeTrue())
}
