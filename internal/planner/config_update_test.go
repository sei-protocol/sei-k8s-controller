package planner

import (
	"testing"

	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
)

func newRunningNodeWithPeerCondition() *seiv1alpha1.SeiNode {
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-0", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "sei-test",
			Image:    "ghcr.io/sei-protocol/seid:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{},
			Peers: []seiv1alpha1.PeerSource{
				{Static: &seiv1alpha1.StaticPeerSource{Addresses: []string{"abc@1.2.3.4:26656"}}},
			},
		},
		Status: seiv1alpha1.SeiNodeStatus{
			Phase: seiv1alpha1.PhaseRunning,
		},
	}
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:   ConditionPeerUpdateNeeded,
		Status: metav1.ConditionTrue,
		Reason: "PeerSpecChanged",
	})
	return node
}

func TestBuildPlan_PeerUpdate_WithPeers(t *testing.T) {
	g := NewWithT(t)

	node := newRunningNodeWithPeerCondition()
	p, err := ForNode(node)
	g.Expect(err).NotTo(HaveOccurred())

	plan, err := p.BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive))
	g.Expect(plan.Tasks).To(HaveLen(3))
	g.Expect(plan.Tasks[0].Type).To(Equal(TaskDiscoverPeers))
	g.Expect(plan.Tasks[1].Type).To(Equal(TaskConfigApply))
	g.Expect(plan.Tasks[2].Type).To(Equal(TaskConfigValidate))
}

func TestBuildPlan_PeerUpdate_ConfigApplyIsIncremental(t *testing.T) {
	g := NewWithT(t)

	node := newRunningNodeWithPeerCondition()
	p, err := ForNode(node)
	g.Expect(err).NotTo(HaveOccurred())

	plan, err := p.BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())

	configApplyTask := plan.Tasks[1]
	g.Expect(configApplyTask.Type).To(Equal(TaskConfigApply))
	g.Expect(string(configApplyTask.Params.Raw)).To(ContainSubstring(`"incremental":true`))
}

func TestBuildPlan_PeerUpdate_NoPeers(t *testing.T) {
	g := NewWithT(t)

	node := newRunningNodeWithPeerCondition()
	node.Spec.Peers = nil // no peers

	p, err := ForNode(node)
	g.Expect(err).NotTo(HaveOccurred())

	plan, err := p.BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Tasks).To(HaveLen(2))
	g.Expect(plan.Tasks[0].Type).To(Equal(TaskConfigApply))
	g.Expect(plan.Tasks[1].Type).To(Equal(TaskConfigValidate))
}

func TestBuildPlan_InitPlan_WhenNotRunning(t *testing.T) {
	g := NewWithT(t)

	// Node is Pending, not Running — should get init plan, not peer update.
	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-0", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "sei-test",
			Image:    "ghcr.io/sei-protocol/seid:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}

	p, err := ForNode(node)
	g.Expect(err).NotTo(HaveOccurred())

	plan, err := p.BuildPlan(node)
	g.Expect(err).NotTo(HaveOccurred())
	// Init plan has config-apply (non-incremental) and mark-ready.
	g.Expect(plan.Tasks).To(HaveLen(4)) // configure-genesis, config-apply, config-validate, mark-ready
}
