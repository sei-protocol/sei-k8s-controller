package planner

import (
	"fmt"

	"github.com/google/uuid"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// ForDeployment returns the appropriate GroupPlanner for the group's
// configured update strategy.
func ForDeployment(group *seiv1alpha1.SeiNodeDeployment) (GroupPlanner, error) {
	switch group.Spec.UpdateStrategy.Type {
	case seiv1alpha1.UpdateStrategyInPlace:
		return &inPlaceDeploymentPlanner{}, nil
	default:
		return nil, fmt.Errorf("unknown update strategy type %q", group.Spec.UpdateStrategy.Type)
	}
}

// inPlaceDeploymentPlanner builds a deployment plan for the InPlace strategy.
type inPlaceDeploymentPlanner struct{}

func (p *inPlaceDeploymentPlanner) BuildPlan(
	group *seiv1alpha1.SeiNodeDeployment,
) (*seiv1alpha1.TaskPlan, error) {
	planID := uuid.New().String()
	nodeNames := group.Status.IncumbentNodes
	ns := group.Namespace

	prog := []struct {
		taskType string
		params   any
	}{
		{task.TaskTypeUpdateNodeSpecs, &task.UpdateNodeSpecsParams{
			GroupName: group.Name,
			Namespace: ns,
			NodeNames: nodeNames,
		}},
		{task.TaskTypeAwaitSpecUpdate, &task.AwaitSpecUpdateParams{
			Namespace: ns,
			NodeNames: nodeNames,
		}},
	}

	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, p := range prog {
		t, err := buildPlannedTask(planID, p.taskType, i, p.params)
		if err != nil {
			return nil, err
		}
		tasks[i] = t
	}
	return &seiv1alpha1.TaskPlan{ID: planID, Phase: seiv1alpha1.TaskPlanActive, Tasks: tasks}, nil
}
