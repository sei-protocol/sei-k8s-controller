package planner

import (
	"encoding/json"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

func TestInPlacePlan_ThreeTasks(t *testing.T) {
	g := NewWithT(t)

	group := &seiv1alpha1.SeiNodeDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "wave-group", Namespace: "pacific-1", Generation: 2},
		Spec: seiv1alpha1.SeiNodeDeploymentSpec{
			Replicas:       3,
			UpdateStrategy: seiv1alpha1.UpdateStrategy{Type: seiv1alpha1.UpdateStrategyInPlace},
		},
		Status: seiv1alpha1.SeiNodeDeploymentStatus{
			IncumbentNodes: []string{"wave-group-0", "wave-group-1", "wave-group-2"},
		},
	}

	planner := &inPlaceDeploymentPlanner{}
	plan, err := planner.BuildPlan(group)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(plan.Phase).To(Equal(seiv1alpha1.TaskPlanActive))
	g.Expect(plan.Tasks).To(HaveLen(2))

	g.Expect(plan.Tasks[0].Type).To(Equal(task.TaskTypeUpdateNodeSpecs))
	g.Expect(plan.Tasks[1].Type).To(Equal(task.TaskTypeAwaitSpecUpdate))

	for i, pt := range plan.Tasks {
		g.Expect(pt.Status).To(Equal(seiv1alpha1.TaskPending), "task[%d] should be Pending", i)
		g.Expect(pt.ID).NotTo(BeEmpty(), "task[%d] should have an ID", i)
		g.Expect(pt.Params).NotTo(BeNil(), "task[%d] should have params", i)
	}

	var updateParams task.UpdateNodeSpecsParams
	g.Expect(json.Unmarshal(plan.Tasks[0].Params.Raw, &updateParams)).To(Succeed())
	g.Expect(updateParams.GroupName).To(Equal("wave-group"))
	g.Expect(updateParams.Namespace).To(Equal("pacific-1"))
	g.Expect(updateParams.NodeNames).To(Equal([]string{"wave-group-0", "wave-group-1", "wave-group-2"}))

	var awaitParams task.AwaitSpecUpdateParams
	g.Expect(json.Unmarshal(plan.Tasks[1].Params.Raw, &awaitParams)).To(Succeed())
	g.Expect(awaitParams.Namespace).To(Equal("pacific-1"))
	g.Expect(awaitParams.NodeNames).To(Equal([]string{"wave-group-0", "wave-group-1", "wave-group-2"}))
}
