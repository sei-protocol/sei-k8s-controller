package planner

import (
	"fmt"
	"strconv"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
)

// DeploymentPlanner builds a TaskPlan for blue-green deployment orchestration.
type DeploymentPlanner struct{}

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

// BuildDeploymentPlan builds the task plan for a blue-green deployment
// based on the configured update strategy.
func BuildDeploymentPlan(
	group *seiv1alpha1.SeiNodeGroup,
	blueNodes []string,
) (*seiv1alpha1.DeploymentStatus, error) {
	strategy := group.Spec.UpdateStrategy
	if strategy == nil {
		return nil, fmt.Errorf("no update strategy configured on %s/%s", group.Namespace, group.Name)
	}

	incomingRevision := strconv.FormatInt(group.Generation, 10)

	// Determine active revision from existing blue nodes' labels or current status.
	activeRevision := ""
	if group.Status.Deployment != nil && group.Status.Deployment.ActiveRevision != "" {
		activeRevision = group.Status.Deployment.ActiveRevision
	} else {
		// First deployment: the active revision is the previous generation.
		activeRevision = strconv.FormatInt(group.Generation-1, 10)
	}

	greenNames := make([]string, int(group.Spec.Replicas))
	for i := range int(group.Spec.Replicas) {
		greenNames[i] = fmt.Sprintf("%s-g%d-%d", group.Name, group.Generation, i)
	}

	var tasks []seiv1alpha1.PlannedTask
	var err error

	switch strategy.Type {
	case seiv1alpha1.UpdateStrategyHardFork:
		tasks, err = buildHardForkTasks(group, blueNodes, greenNames)
	case seiv1alpha1.UpdateStrategyBlueGreen:
		tasks, err = buildBlueGreenTasks(group, blueNodes, greenNames)
	default:
		return nil, fmt.Errorf("unknown update strategy type %q", strategy.Type)
	}
	if err != nil {
		return nil, err
	}

	return &seiv1alpha1.DeploymentStatus{
		Plan: seiv1alpha1.TaskPlan{
			Phase: seiv1alpha1.TaskPlanActive,
			Tasks: tasks,
		},
		ActiveRevision:   activeRevision,
		IncomingRevision: incomingRevision,
		BlueNodes:        blueNodes,
		GreenNodes:       greenNames,
	}, nil
}

func buildHardForkTasks(
	group *seiv1alpha1.SeiNodeGroup,
	blueNodes, greenNames []string,
) ([]seiv1alpha1.PlannedTask, error) {
	haltHeight := group.Spec.UpdateStrategy.HardFork.HaltHeight
	groupName := group.Name
	ns := group.Namespace
	incomingRevision := strconv.FormatInt(group.Generation, 10)
	attempt := 0

	prog := []struct {
		taskType string
		params   any
	}{
		{task.TaskTypeCreateGreenNodes, &task.CreateGreenNodesParams{
			GroupName:        groupName,
			Namespace:        ns,
			IncomingRevision: incomingRevision,
			NodeNames:        greenNames,
		}},
		{task.TaskTypeAwaitGreenRunning, &task.AwaitGreenRunningParams{
			Namespace: ns,
			NodeNames: greenNames,
		}},
		{task.TaskTypeSubmitHaltSignal, &task.SubmitHaltSignalParams{
			Namespace:  ns,
			NodeNames:  blueNodes,
			HaltHeight: haltHeight,
		}},
		{task.TaskTypeAwaitGreenAtHeight, &task.AwaitGreenAtHeightParams{
			Namespace:  ns,
			NodeNames:  greenNames,
			HaltHeight: haltHeight,
		}},
		{task.TaskTypeSwitchTraffic, &task.SwitchTrafficParams{
			GroupName:        groupName,
			Namespace:        ns,
			IncomingRevision: incomingRevision,
		}},
		{task.TaskTypeTeardownBlue, &task.TeardownBlueParams{
			Namespace: ns,
			NodeNames: blueNodes,
		}},
	}

	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, p := range prog {
		t, err := buildPlannedTask2(groupName, p.taskType, attempt, p.params)
		if err != nil {
			return nil, err
		}
		tasks[i] = t
	}
	return tasks, nil
}

func buildBlueGreenTasks(
	group *seiv1alpha1.SeiNodeGroup,
	blueNodes, greenNames []string,
) ([]seiv1alpha1.PlannedTask, error) {
	groupName := group.Name
	ns := group.Namespace
	incomingRevision := strconv.FormatInt(group.Generation, 10)
	attempt := 0

	prog := []struct {
		taskType string
		params   any
	}{
		{task.TaskTypeCreateGreenNodes, &task.CreateGreenNodesParams{
			GroupName:        groupName,
			Namespace:        ns,
			IncomingRevision: incomingRevision,
			NodeNames:        greenNames,
		}},
		{task.TaskTypeAwaitGreenRunning, &task.AwaitGreenRunningParams{
			Namespace: ns,
			NodeNames: greenNames,
		}},
		{task.TaskTypeAwaitGreenCaughtUp, &task.AwaitGreenCaughtUpParams{
			Namespace: ns,
			NodeNames: greenNames,
		}},
		{task.TaskTypeSwitchTraffic, &task.SwitchTrafficParams{
			GroupName:        groupName,
			Namespace:        ns,
			IncomingRevision: incomingRevision,
		}},
		{task.TaskTypeTeardownBlue, &task.TeardownBlueParams{
			Namespace: ns,
			NodeNames: blueNodes,
		}},
	}

	tasks := make([]seiv1alpha1.PlannedTask, len(prog))
	for i, p := range prog {
		t, err := buildPlannedTask2(groupName, p.taskType, attempt, p.params)
		if err != nil {
			return nil, err
		}
		tasks[i] = t
	}
	return tasks, nil
}

// buildPlannedTask2 is like buildPlannedTask but uses the group name
// instead of node name for deterministic IDs.
func buildPlannedTask2(resourceName, taskType string, attempt int, params any) (seiv1alpha1.PlannedTask, error) {
	id := task.DeterministicTaskID(resourceName, taskType, attempt)
	p, err := marshalParams(params)
	if err != nil {
		return seiv1alpha1.PlannedTask{}, fmt.Errorf("task %s: %w", taskType, err)
	}
	return seiv1alpha1.PlannedTask{
		Type:   taskType,
		ID:     id,
		Status: seiv1alpha1.TaskPending,
		Params: p,
	}, nil
}
