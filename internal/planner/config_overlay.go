package planner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch"
)

// configValuesOverlay builds an independent, unvalidated merge patch. In
// particular, these values must never pass through ConfigIntent.Overrides.
func configValuesOverlay(values []seiv1alpha1.ConfigValue) (*task.ConfigPatchTask, error) {
	patch := &task.ConfigPatchTask{Files: make(map[string]map[string]any)}
	for _, entry := range values {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(entry.Value.Raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("configValues %s:%s: %w", entry.FileName, entry.Key, err)
		}
		path := strings.Split(entry.Key, ".")
		for i := len(path) - 1; i >= 0; i-- {
			value = map[string]any{path[i]: value}
		}
		patch.Files[entry.FileName] = tomlpatch.Merge(patch.Files[entry.FileName], value).(map[string]any)
	}
	return patch, nil
}

// withConfigValues is only used by INIT plan builders. Apply the overlay after
// base regeneration and any state-sync/genesis peer writes, before validation.
// Bootstrap plans have two validation stages and need the overlay in both.
func withConfigValues(plan *seiv1alpha1.TaskPlan, node *seiv1alpha1.SeiNode) (*seiv1alpha1.TaskPlan, error) {
	if len(node.Spec.ConfigValues) == 0 {
		return plan, nil
	}
	patch, err := configValuesOverlay(node.Spec.ConfigValues)
	if err != nil {
		return nil, err
	}
	tasks := make([]seiv1alpha1.PlannedTask, 0, len(plan.Tasks)+2)
	for _, planned := range plan.Tasks {
		if planned.Type == TaskConfigValidate {
			overlay, err := buildPlannedTask(plan.ID, TaskConfigPatch, len(tasks), patch)
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, overlay)
		}
		planned.ID = task.DeterministicTaskID(plan.ID, planned.Type, len(tasks))
		tasks = append(tasks, planned)
	}
	plan.Tasks = tasks
	return plan, nil
}
