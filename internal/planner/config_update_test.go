package planner

import (
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

func TestBuildConfigUpdatePlan_WithPeers(t *testing.T) {
	g := NewWithT(t)

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
	}

	params := &task.ConfigApplyParams{Mode: "full", Overrides: map[string]string{"key": "val"}}
	plan, err := BuildConfigUpdatePlan(node, params)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive))
	g.Expect(plan.Tasks).To(HaveLen(3))
	g.Expect(plan.Tasks[0].Type).To(Equal(TaskDiscoverPeers))
	g.Expect(plan.Tasks[1].Type).To(Equal(TaskConfigApply))
	g.Expect(plan.Tasks[2].Type).To(Equal(TaskConfigValidate))
}

func TestBuildConfigUpdatePlan_NoPeers(t *testing.T) {
	g := NewWithT(t)

	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-0", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "sei-test",
			Image:    "ghcr.io/sei-protocol/seid:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}

	params := &task.ConfigApplyParams{Mode: "full"}
	plan, err := BuildConfigUpdatePlan(node, params)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(plan.Tasks).To(HaveLen(2))
	g.Expect(plan.Tasks[0].Type).To(Equal(TaskConfigApply))
	g.Expect(plan.Tasks[1].Type).To(Equal(TaskConfigValidate))
}

func TestBuildConfigUpdatePlan_ConfigApplyIsIncremental(t *testing.T) {
	g := NewWithT(t)

	node := &seiv1alpha1.SeiNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-0", Namespace: "default"},
		Spec: seiv1alpha1.SeiNodeSpec{
			ChainID:  "sei-test",
			Image:    "ghcr.io/sei-protocol/seid:latest",
			FullNode: &seiv1alpha1.FullNodeSpec{},
		},
	}

	params := &task.ConfigApplyParams{Mode: "full", Incremental: false}
	plan, err := BuildConfigUpdatePlan(node, params)
	g.Expect(err).NotTo(HaveOccurred())

	// The original params should not be mutated.
	g.Expect(params.Incremental).To(BeFalse())

	// The plan's config-apply task should use Incremental: true.
	configApplyTask := plan.Tasks[0]
	g.Expect(configApplyTask.Type).To(Equal(TaskConfigApply))
	g.Expect(configApplyTask.Params).NotTo(BeNil())
	g.Expect(string(configApplyTask.Params.Raw)).To(ContainSubstring(`"incremental":true`))
}
