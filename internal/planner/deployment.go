package planner

import (
	"fmt"
	"strconv"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// NeedsDeployment returns true when the group's template has changed
// (generation mismatch) and an update strategy is configured.
func NeedsDeployment(group *seiv1alpha1.SeiNodeGroup) bool {
	if group.Spec.UpdateStrategy == nil {
		return false
	}
	return group.Generation != group.Status.ObservedGeneration
}

// IsDeploymentActive returns true when a deployment plan is in progress.
func IsDeploymentActive(group *seiv1alpha1.SeiNodeGroup) bool {
	return group.Status.Deployment != nil &&
		group.Status.Deployment.Plan.Phase == seiv1alpha1.TaskPlanActive
}

// ForDeployment returns the appropriate GroupPlanner for the group's
// configured update strategy.
func ForDeployment(group *seiv1alpha1.SeiNodeGroup) (GroupPlanner, error) {
	if group.Spec.UpdateStrategy == nil {
		return nil, fmt.Errorf("no update strategy on %s/%s", group.Namespace, group.Name)
	}
	switch group.Spec.UpdateStrategy.Type {
	case seiv1alpha1.UpdateStrategyHardFork:
		return &hardForkDeploymentPlanner{}, nil
	case seiv1alpha1.UpdateStrategyBlueGreen:
		return &blueGreenDeploymentPlanner{}, nil
	default:
		return nil, fmt.Errorf("unknown update strategy type %q", group.Spec.UpdateStrategy.Type)
	}
}

// EntrantNodeNames generates the names for entrant SeiNodes based on the
// group's name and generation.
func EntrantNodeNames(group *seiv1alpha1.SeiNodeGroup) []string {
	names := make([]string, int(group.Spec.Replicas))
	for i := range int(group.Spec.Replicas) {
		names[i] = fmt.Sprintf("%s-g%d-%d", group.Name, group.Generation, i)
	}
	return names
}

// EntrantRevision returns the revision string for the entrant set.
func EntrantRevision(group *seiv1alpha1.SeiNodeGroup) string {
	return strconv.FormatInt(group.Generation, 10)
}

// IncumbentRevision returns the revision string for the incumbent set.
func IncumbentRevision(group *seiv1alpha1.SeiNodeGroup) string {
	if group.Status.Deployment != nil && group.Status.Deployment.IncumbentRevision != "" {
		return group.Status.Deployment.IncumbentRevision
	}
	return strconv.FormatInt(group.Generation-1, 10)
}

// hardForkDeploymentPlanner builds a deployment plan for the HardFork strategy.
type hardForkDeploymentPlanner struct{}

func (p *hardForkDeploymentPlanner) BuildPlan(
	group *seiv1alpha1.SeiNodeGroup,
	_ []seiv1alpha1.SeiNode,
) (*seiv1alpha1.TaskPlan, error) {
	haltHeight := group.Spec.UpdateStrategy.HardFork.HaltHeight
	incumbentNodes := group.Status.IncumbentNodes
	entrantNodes := EntrantNodeNames(group)
	entrantRevision := EntrantRevision(group)
	ns := group.Namespace

	prog := []struct {
		taskType string
		params   any
	}{
		{task.TaskTypeCreateEntrantNodes, &task.CreateEntrantNodesParams{
			GroupName:       group.Name,
			Namespace:       ns,
			EntrantRevision: entrantRevision,
			NodeNames:       entrantNodes,
		}},
		{task.TaskTypeAwaitNodesRunning, &task.AwaitNodesRunningParams{
			GroupName: group.Name,
			Namespace: ns,
			Expected:  len(entrantNodes),
			NodeNames: entrantNodes,
		}},
		{task.TaskTypeSubmitHaltSignal, &task.SubmitHaltSignalParams{
			Namespace:  ns,
			NodeNames:  incumbentNodes,
			HaltHeight: haltHeight,
		}},
		{task.TaskTypeAwaitNodesAtHeight, &task.AwaitNodesAtHeightParams{
			Namespace:    ns,
			NodeNames:    entrantNodes,
			TargetHeight: haltHeight,
		}},
		{task.TaskTypeSwitchTraffic, &task.SwitchTrafficParams{
			GroupName:       group.Name,
			Namespace:       ns,
			EntrantRevision: entrantRevision,
		}},
		{task.TaskTypeTeardownNodes, &task.TeardownNodesParams{
			Namespace: ns,
			NodeNames: incumbentNodes,
		}},
	}

	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, p := range prog {
		t, err := buildGroupPlannedTask(group.Name, p.taskType, p.params)
		if err != nil {
			return nil, err
		}
		tasks[i] = t
	}
	return &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive, Tasks: tasks}, nil
}

// blueGreenDeploymentPlanner builds a deployment plan for the BlueGreen strategy.
type blueGreenDeploymentPlanner struct{}

func (p *blueGreenDeploymentPlanner) BuildPlan(
	group *seiv1alpha1.SeiNodeGroup,
	_ []seiv1alpha1.SeiNode,
) (*seiv1alpha1.TaskPlan, error) {
	incumbentNodes := group.Status.IncumbentNodes
	entrantNodes := EntrantNodeNames(group)
	entrantRevision := EntrantRevision(group)
	ns := group.Namespace

	prog := []struct {
		taskType string
		params   any
	}{
		{task.TaskTypeCreateEntrantNodes, &task.CreateEntrantNodesParams{
			GroupName:       group.Name,
			Namespace:       ns,
			EntrantRevision: entrantRevision,
			NodeNames:       entrantNodes,
		}},
		{task.TaskTypeAwaitNodesRunning, &task.AwaitNodesRunningParams{
			GroupName: group.Name,
			Namespace: ns,
			Expected:  len(entrantNodes),
			NodeNames: entrantNodes,
		}},
		{task.TaskTypeAwaitNodesCaughtUp, &task.AwaitNodesCaughtUpParams{
			Namespace: ns,
			NodeNames: entrantNodes,
		}},
		{task.TaskTypeSwitchTraffic, &task.SwitchTrafficParams{
			GroupName:       group.Name,
			Namespace:       ns,
			EntrantRevision: entrantRevision,
		}},
		{task.TaskTypeTeardownNodes, &task.TeardownNodesParams{
			Namespace: ns,
			NodeNames: incumbentNodes,
		}},
	}

	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, p := range prog {
		t, err := buildGroupPlannedTask(group.Name, p.taskType, p.params)
		if err != nil {
			return nil, err
		}
		tasks[i] = t
	}
	return &seiv1alpha1.TaskPlan{Phase: seiv1alpha1.TaskPlanActive, Tasks: tasks}, nil
}
