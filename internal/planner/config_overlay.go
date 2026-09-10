package planner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	seiv1alpha1 "github.com/sei-protocol/sei-k8s-controller/api/v1alpha1"
	"github.com/sei-protocol/sei-k8s-controller/internal/task"
	"github.com/sei-protocol/sei-k8s-controller/sidecarapi/tomlpatch"
)

// configValuesOverlay builds an independent merge patch. In
// particular, these values must never pass through ConfigIntent.Overrides.
func configValuesOverlay(values []seiv1alpha1.ConfigValue) (*task.ConfigPatchTask, error) {
	patch := &task.ConfigPatchTask{Files: make(map[string]map[string]any)}
	for i, entry := range values {
		for _, previous := range values[:i] {
			if previous.FileName == entry.FileName &&
				(strings.HasPrefix(entry.Key, previous.Key+".") || strings.HasPrefix(previous.Key, entry.Key+".")) {
				return nil, fmt.Errorf("configValues %s: overlapping keys %q and %q; use non-overlapping dotted paths", entry.FileName, previous.Key, entry.Key)
			}
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(entry.Value.Raw))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("configValues %s:%s: %w", entry.FileName, entry.Key, err)
		}
		if containsNull(value) {
			return nil, fmt.Errorf("configValues %s:%s: null values are not supported, including inside objects or arrays; supply a TOML value", entry.FileName, entry.Key)
		}
		path := strings.Split(entry.Key, ".")
		for _, key := range slices.Backward(path) {
			value = map[string]any{key: value}
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
	tasks := make([]seiv1alpha1.PlannedTask, 0, 2*len(plan.Tasks))
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

// containsNull rejects merge-patch deletion semantics and unencodable TOML nils.
func containsNull(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case map[string]any:
		for _, child := range value {
			if containsNull(child) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(value, containsNull)
	}
	return false
}
