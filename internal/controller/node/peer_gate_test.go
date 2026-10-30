package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/planner"
)

const (
	gatedNodeName  = "rpc-0"
	gatedPeerName  = "validator-0"
	gatedPeerLabel = "sei.io/seinetwork"
	gatedPeerValue = "harbor-test"
)

// labelPeeredNode returns a pre-Running full node that peers by label with
// gatedPeer, created age ago.
func labelPeeredNode(age time.Duration) *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:              gatedNodeName,
			Namespace:         testNamespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
		},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID: testChainID,
			Image:   testImage,
			Peers: []seiv1alpha1.PeerSource{
				{Label: &seiv1alpha1.LabelPeerSource{Selector: map[string]string{gatedPeerLabel: gatedPeerValue}}},
			},
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}
}

func gatedPeer() *seiv1alpha1.SeiNode {
	return &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gatedPeerName,
			Namespace: testNamespace,
			Labels:    map[string]string{gatedPeerLabel: gatedPeerValue},
		},
		Spec: seiv1alpha1.SeiNodeSpec{ChainID: testChainID, Image: testImage, FullNode: &seiv1alpha1.FullNodeSpec{}},
	}
}

func peersCondition(node *seiv1alpha1.SeiNode) *metav1.Condition {
	return apimeta.FindStatusCondition(node.Status.Conditions, seiv1alpha1.ConditionPeersReady)
}

// While no label peer resolves, a new node gets neither an init plan nor a
// StatefulSet, and it polls. Once the peer's sidecar answers, the next
// reconcile builds the plan with that peer in config-apply's persistent_peers.
func TestReconcile_PeersUnresolved_HoldsInitPlanUntilResolved(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	mock := &mockSidecarClient{nodeID: "mock-node-id", nodeIDErr: errors.New("sidecar not up")}
	r, c := newNodeReconcilerWithSidecar(t, mock, labelPeeredNode(time.Minute), gatedPeer())

	res, err := r.Reconcile(ctx, nodeReqFor(gatedNodeName, testNamespace))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(statusPollInterval))

	fetched := getSeiNode(t, ctx, c, gatedNodeName, testNamespace)
	g.Expect(fetched.Status.Plan).To(BeNil(), "no init plan while no label peer has resolved")
	g.Expect(fetched.Status.StatefulSet).To(BeNil())
	sts := &appsv1.StatefulSet{}
	g.Expect(c.Get(ctx, types.NamespacedName{Name: gatedNodeName, Namespace: testNamespace}, sts)).NotTo(Succeed())
	cond := peersCondition(fetched)
	g.Expect(cond).NotTo(BeNil())
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonPeersAwaiting))

	mock.nodeIDErr = nil
	_, err = r.Reconcile(ctx, nodeReqFor(gatedNodeName, testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	fetched = getSeiNode(t, ctx, c, gatedNodeName, testNamespace)
	g.Expect(fetched.Status.Plan).NotTo(BeNil(), "the init plan is built once a label peer resolves")
	apply := findPlannedTask(fetched.Status.Plan, planner.TaskConfigApply)
	g.Expect(apply).NotTo(BeNil())
	g.Expect(string(apply.Params.Raw)).To(ContainSubstring("mock-node-id@" + gatedPeerName + "-0."))
	g.Expect(peersCondition(fetched).Status).To(Equal(metav1.ConditionTrue))
}

// After PeerResolutionWait the gate releases, so nodes whose label sources
// only select each other still start, without persistent peers.
func TestReconcile_PeersUnresolved_WaitExpiredBuildsPlan(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	mock := &mockSidecarClient{nodeIDErr: errors.New("sidecar not up")}
	node := labelPeeredNode(planner.PeerResolutionWait + time.Minute)
	r, c := newNodeReconcilerWithSidecar(t, mock, node, gatedPeer())

	_, err := r.Reconcile(ctx, nodeReqFor(gatedNodeName, testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, gatedNodeName, testNamespace)
	g.Expect(fetched.Status.Plan).NotTo(BeNil())
	cond := peersCondition(fetched)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonPeersUnresolved))
}

// A paused node returns before peers are resolved, but still carries
// PeersReady, and builds no plan.
func TestReconcile_PausedNode_PeersReadyStillSeeded(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := labelPeeredNode(time.Minute)
	node.Spec.Paused = true
	r, c := newNodeReconciler(t, node, gatedPeer())

	_, err := r.Reconcile(ctx, nodeReqFor(gatedNodeName, testNamespace))
	g.Expect(err).NotTo(HaveOccurred())

	fetched := getSeiNode(t, ctx, c, gatedNodeName, testNamespace)
	cond := peersCondition(fetched)
	g.Expect(cond).NotTo(BeNil(), "PeersReady must be present even on a paused node")
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonPeersAwaiting))
	g.Expect(fetched.Status.Plan).To(BeNil(), "a paused node must build no plan")
}

func TestSetPeersReady_NoPeersNotApplicable(t *testing.T) {
	g := NewWithT(t)
	node := labelPeeredNode(time.Minute)
	node.Spec.Peers = nil

	setPeersReady(node, time.Now())

	cond := peersCondition(node)
	g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(cond.Reason).To(Equal(seiv1alpha1.ReasonPeersNotApplicable))
	g.Expect(strings.Contains(cond.Message, "spec.peers")).To(BeTrue())
}
